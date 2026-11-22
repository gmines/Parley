package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"parley/pkg/db/dbtest"
	"parley/services/api/internal/delivery"
	"parley/services/api/internal/identity"
	"parley/services/api/migrations"
)

func TestValidation(t *testing.T) {
	var v *ValidationError
	bad := []Sessions{
		{AccessTTL: "5s", RefreshTTL: "720h"}, // below the floor
		{AccessTTL: "48h", RefreshTTL: "720h"},
		{AccessTTL: "15m", RefreshTTL: "10m"},
		{AccessTTL: "soon", RefreshTTL: "720h"},
		{AccessTTL: "2h", RefreshTTL: "1h"},
	}
	for _, x := range bad {
		if _, _, err := x.Parse(); !errors.As(err, &v) {
			t.Errorf("%+v accepted (%v)", x, err)
		}
	}
	if a, r, err := (Sessions{AccessTTL: "15m", RefreshTTL: "720h"}).Parse(); err != nil || a != 15*time.Minute || r != 720*time.Hour {
		t.Errorf("good sessions: %v %v %v", a, r, err)
	}
	if _, err := (Lockout{MaxFailures: 5, Duration: "5m", Window: "15m", Scope: "everyone"}).Parse(); !errors.As(err, &v) {
		t.Error("bad scope accepted")
	}
	if _, err := (Lockout{MaxFailures: -1, Duration: "5m", Window: "15m", Scope: "account"}).Parse(); !errors.As(err, &v) {
		t.Error("negative failures accepted")
	}
	if c, err := (Lockout{MaxFailures: 0, Duration: "5m", Window: "15m", Scope: "account_ip"}).Parse(); err != nil || c.MaxFailures != 0 {
		t.Errorf("lockout off rejected: %v", err)
	}
	if err := CheckAccountURL("http://account.example.org"); err == nil {
		t.Error("plain http account URL accepted")
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(dbtest.New(t, migrations.FS), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreLayersOverEnvironment(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := identity.Runtime{
		Lockout:   identity.DefaultLockout,
		AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour,
		Recovery: identity.RecoveryConfig{AccountURL: "https://account.example.org"},
	}

	snap, err := s.Load(ctx)
	if err != nil || !snap.MFA.Require || snap.Lockout != nil {
		t.Fatalf("fresh database: %+v %v", snap, err)
	}
	rt, problems := s.Resolve(base, snap)
	if len(problems) != 0 || rt.AccessTTL != base.AccessTTL {
		t.Fatalf("nothing stored should mean the base: %+v %v", rt, problems)
	}

	fp0, _ := s.Fingerprint(ctx)
	if err := s.PutSessions(ctx, Sessions{AccessTTL: "10m", RefreshTTL: "48h"}, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEmail(ctx, Email{Mode: "smtp", Host: "smtp.example.org", Port: 587, TLS: "starttls",
		Username: "mailer", Password: "hunter2-secret", From: "Parley <no-reply@example.org>"}, base.Recovery.AccountURL, "tester"); err != nil {
		t.Fatal(err)
	}
	fp1, _ := s.Fingerprint(ctx)
	if fp0 == fp1 {
		t.Fatal("fingerprint didn't change")
	}

	// The password is sealed in the database, never stored in clear.
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT value FROM server_settings WHERE key = 'email'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("plaintext password stored: %s", raw)
	}

	snap, _ = s.Load(ctx)
	rt, problems = s.Resolve(base, snap)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	if rt.AccessTTL != 10*time.Minute || rt.RefreshTTL != 48*time.Hour || rt.Lockout != base.Lockout {
		t.Fatalf("resolved %+v", rt)
	}
	smtp, ok := rt.Recovery.Email.(*delivery.SMTP)
	if !rt.Recovery.EmailEnabled || !ok || smtp.Password != "hunter2-secret" || smtp.Host != "smtp.example.org" {
		t.Fatalf("email: %+v", rt.Recovery.Email)
	}

	// Saving again without a password keeps the stored one.
	if err := s.PutEmail(ctx, Email{Mode: "smtp", Host: "mail.example.org", Port: 465, TLS: "implicit",
		Username: "mailer", From: "no-reply@example.org"}, base.Recovery.AccountURL, "tester"); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Load(ctx)
	rt, _ = s.Resolve(base, snap)
	if smtp := rt.Recovery.Email.(*delivery.SMTP); smtp.Password != "hunter2-secret" || !smtp.ImplicitTLS {
		t.Fatalf("after re-save: %+v", smtp)
	}

	// A broken stored value is reported and the base stays in effect.
	if _, err := s.pool.Exec(ctx, `UPDATE server_settings SET value = '{"access_ttl":"1s","refresh_ttl":"1h"}' WHERE key = 'sessions'`); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Load(ctx)
	rt, problems = s.Resolve(base, snap)
	if problems[KeySessions] == nil || rt.AccessTTL != base.AccessTTL {
		t.Fatalf("bad stored value: %+v %v", rt, problems)
	}

	// Reverting goes back to the environment.
	if err := s.Revert(ctx, KeyEmail, "tester"); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Load(ctx)
	if snap.Email != nil {
		t.Fatal("email still stored after revert")
	}
}

func TestEmailNeedsAccountPage(t *testing.T) {
	s := newStore(t)
	err := s.PutEmail(context.Background(), Email{Mode: "smtp", Host: "smtp.example.org", Port: 587, TLS: "starttls", From: "a@example.org"}, "", "tester")
	var v *ValidationError
	if !errors.As(err, &v) || !strings.Contains(v.Msg, "PARLEY_ACCOUNT_PUBLIC_URL") {
		t.Fatalf("got %v", err)
	}
}

func TestSecretsBoundToTheirKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	url := "https://account.example.org"
	if err := s.PutSMS(ctx, SMS{Mode: "twilio", AccountSID: "AC" + strings.Repeat("0", 32), AuthToken: "tok-123", From: "+15550000000"}, url, "t"); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Load(ctx)
	// Moving the sealed token into the email password doesn't decrypt.
	e := Email{Mode: "smtp", Host: "h.example.org", Port: 25, TLS: "starttls", Username: "u", From: "a@example.org",
		PasswordSealed: snap.SMS.AuthTokenSealed}
	if _, err := s.EmailSender(e, url); err == nil {
		t.Fatal("SMS token opened as the email password")
	}
	b, _ := json.Marshal(snap.SMS)
	if strings.Contains(string(b), "tok-123") {
		t.Fatal("token in the clear")
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		15 * time.Minute: "15m", 720 * time.Hour: "720h", 90 * time.Minute: "1h30m", 30 * time.Second: "30s", time.Hour: "1h",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("%v: got %q want %q", d, got, want)
		}
	}
}

func TestInvitesGroup(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := identity.Runtime{
		Recovery: identity.RecoveryConfig{AccountURL: "https://account.example.org", InviteDailyLimit: 5},
	}

	// Nothing stored: the base applies (invites off by default).
	snap, _ := s.Load(ctx)
	rt, problems := s.Resolve(base, snap)
	if len(problems) != 0 || rt.Recovery.InvitesEnabled {
		t.Fatalf("default should be off: %+v %v", rt.Recovery, problems)
	}

	// A bad limit is rejected.
	var v *ValidationError
	if err := s.PutInvites(ctx, Invites{Enabled: true, DailyLimit: 0}, "tester"); !errors.As(err, &v) {
		t.Fatalf("zero limit while enabled accepted: %v", err)
	}
	if err := s.PutInvites(ctx, Invites{Enabled: true, DailyLimit: 10}, "tester"); err != nil {
		t.Fatal(err)
	}

	snap, _ = s.Load(ctx)
	if snap.Invites == nil || !snap.Invites.Enabled || snap.Invites.DailyLimit != 10 {
		t.Fatalf("loaded invites = %+v", snap.Invites)
	}
	rt, problems = s.Resolve(base, snap)
	if len(problems) != 0 || !rt.Recovery.InvitesEnabled || rt.Recovery.InviteDailyLimit != 10 {
		t.Fatalf("resolved invites = %+v %v", rt.Recovery, problems)
	}

	// Reverting restores the deployment default.
	if err := s.Revert(ctx, KeyInvites, "tester"); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Load(ctx)
	rt, _ = s.Resolve(base, snap)
	if snap.Invites != nil || rt.Recovery.InvitesEnabled {
		t.Fatalf("revert left invites on: %+v", rt.Recovery)
	}
}
