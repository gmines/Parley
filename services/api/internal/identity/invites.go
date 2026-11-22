package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"parley/pkg/db"
	"parley/services/api/internal/delivery"
)

var (
	ErrInvitesDisabled = errors.New("invites are not enabled on this server")
	ErrInviteLimit     = errors.New("you've sent too many invites today; try again tomorrow")
)

// placeholderUsername is the username an invited account holds until the
// invitee chooses their own at activation. It satisfies the username format
// (a-z0-9_.) and is unguessable, so it never collides with a real name.
func placeholderUsername() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "invite." + hex.EncodeToString(b[:])
}

// Invite creates an inactive account for email and sends its inviter-branded
// activation link, which lets the invitee choose their own username, display
// name and password. On completion the two accounts are made friends (the
// caller wires that up). Like public Signup, it answers the same whether or not
// the address is already in use, so it can't be used to find out who has an
// account; an address that already belongs to an active account gets nothing.
func (s *Service) Invite(ctx context.Context, inviterID uuid.UUID, email, clientIP string) error {
	c := s.cfg().Recovery
	if !c.EmailEnabled {
		return ErrFeatureDisabled
	}
	if !c.InvitesEnabled {
		return ErrInvitesDisabled
	}
	if strings.TrimSpace(c.AccountURL) == "" {
		return ErrNoAccountPage
	}
	addr, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	ttl := c.ActivationTTL

	var link *pendingCode
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		link = nil
		now := s.now()

		// The inviter must be an active, enabled account.
		var (
			inviterName         string
			activated, disabled bool
		)
		err := tx.QueryRow(ctx, `SELECT display_name, activated_at IS NOT NULL, disabled_at IS NOT NULL
			FROM users WHERE id = $1`, inviterID).Scan(&inviterName, &activated, &disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return err
		}
		if disabled {
			return ErrAccountDisabled
		}
		if !activated {
			return ErrNotActivated
		}

		// Per-inviter daily cap.
		var sentToday int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM account_links
			WHERE invited_by = $1 AND created_at > $2`, inviterID, now.Add(-24*time.Hour)).Scan(&sentToday); err != nil {
			return err
		}
		if sentToday >= c.InviteDailyLimit {
			return ErrInviteLimit
		}

		// Abandoned sign-ups and invites release their username and address.
		if _, err := tx.Exec(ctx, `DELETE FROM users u WHERE u.activated_at IS NULL AND u.created_at < $1
			AND NOT EXISTS (SELECT 1 FROM account_links l WHERE l.user_id = u.id AND l.consumed_at IS NULL AND l.expires_at > $2)`,
			now.Add(-c.ActivationTTL), now); err != nil {
			return err
		}

		// An address already on an active account gets nothing (privacy).
		var inUse bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1 AND email_verified_at IS NOT NULL)`,
			addr).Scan(&inUse); err != nil {
			return err
		}
		if inUse {
			slog.InfoContext(ctx, "invite for an address already in use")
			return nil
		}

		// Reuse a pending account for this address; otherwise create one.
		var (
			u           User
			pending     uuid.UUID
			havePending bool
		)
		err = tx.QueryRow(ctx, `SELECT id FROM users WHERE email = $1 AND activated_at IS NULL FOR UPDATE`, addr).Scan(&pending)
		if err == nil {
			havePending = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if havePending {
			if err := s.checkLinkRate(ctx, tx, pending, linkActivate); err != nil {
				slog.InfoContext(ctx, "invite link rate limited", "user_id", pending)
				return nil
			}
			if u, err = getUser(ctx, tx, pending); err != nil {
				return err
			}
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO users (username, display_name, email) VALUES ($1, $1, $2)
				RETURNING `+userCols("users"), placeholderUsername(), addr).Scan(userDest(&u)...)
			if db.IsUniqueViolation(err, "") { // lost a race on the address
				return ErrTooManyCodes
			}
			if err != nil {
				return fmt.Errorf("create invite: %w", err)
			}
		}

		url, err := s.storeLink(ctx, tx, u, linkActivate, channelEmail, addr, clientIP, ttl)
		if err != nil {
			return err
		}
		// Mark the new link as an invite so the account page lets the invitee
		// pick a username, and record who sent it (audit + the daily cap).
		if _, err := tx.Exec(ctx, `UPDATE account_links SET invited_by = $2
			WHERE user_id = $1 AND purpose = $3 AND consumed_at IS NULL`, u.ID, inviterID, linkActivate); err != nil {
			return err
		}
		l := pendingCode{channel: channelEmail, msg: s.inviteMessage(inviterName, addr, url, ttl)}
		link = &l
		return nil
	})
	if err != nil {
		return err
	}
	if link != nil {
		s.deliver(ctx, *link)
	}
	return nil
}

func (s *Service) inviteMessage(inviter, dest, url string, ttl time.Duration) delivery.Message {
	app := s.cfg().Recovery.AppName
	expires := fmt.Sprintf("%d minutes", int(ttl.Minutes()))
	if ttl >= 2*time.Hour {
		expires = fmt.Sprintf("%d hours", int(ttl.Hours()))
	}
	subject := fmt.Sprintf("%s invited you to %s", inviter, app)
	body := fmt.Sprintf("%s invited you to join them on %s.\n\n"+
		"Open this link to choose your username and a password:\n\n%s\n\n"+
		"It expires in %s. If you don't know %s, you can ignore this message and no account will be created.",
		inviter, app, url, expires, inviter)
	return delivery.Message{To: dest, Subject: subject, Body: body}
}
