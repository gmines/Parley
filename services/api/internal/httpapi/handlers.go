package httpapi

import (
	"log/slog"
	"net/http"

	"parley/pkg/httpx"
	"parley/services/api/internal/identity"
)

// Auth ------------------------------------------------------------------------

func (s *Server) signup(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
		Phone       string `json:"phone"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Identity.Signup(r.Context(), identity.Signup{
		Username: body.Username, DisplayName: body.DisplayName, Email: body.Email, Phone: body.Phone,
	}, s.clientIP(r)); err != nil {
		return mapErr(err)
	}
	// Same answer whether or not anything was sent; see Signup.
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"message": "Check your messages for a link to activate your account."})
	return nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	sess, err := s.Identity.Login(r.Context(), body.Username, body.Password, s.clientIP(r))
	if err != nil {
		return mapErr(err)
	}
	if sess.MFARequired {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"mfa_required": true, "mfa_token": sess.MFAToken, "mfa_expires_at": sess.MFAExpiresAt,
		})
		return nil
	}
	httpx.WriteJSON(w, http.StatusOK, sess)
	return nil
}

func (s *Server) mfaLogin(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		MFAToken string `json:"mfa_token"`
		Code     string `json:"code"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	sess, err := s.Identity.CompleteMFALogin(r.Context(), body.MFAToken, body.Code, s.clientIP(r))
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, sess)
	return nil
}

func (s *Server) mfaEnrollStart(w http.ResponseWriter, r *http.Request) error {
	e, err := s.Identity.StartEnrollment(r.Context(), caller(r))
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, e)
	return nil
}

func (s *Server) mfaEnrollConfirm(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	out, err := s.Identity.ConfirmEnrollment(r.Context(), caller(r), body.Code, s.clientIP(r))
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) mfaReset(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	sess, err := s.Identity.ResetMFA(r.Context(), caller(r), body.Password, body.Code, s.clientIP(r))
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, sess)
	return nil
}

type refreshBody struct {
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) error {
	var body refreshBody
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	sess, err := s.Identity.Refresh(r.Context(), body.RefreshToken)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, sess)
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	var body refreshBody
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Identity.Logout(r.Context(), body.RefreshToken); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	u, err := s.Identity.GetUser(r.Context(), caller(r))
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	sess, err := s.Identity.ChangePassword(r.Context(), caller(r), body.CurrentPassword, body.NewPassword, s.clientIP(r))
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, sess)
	return nil
}

// Account recovery ---------------------------------------------------------------

func (s *Server) features(w http.ResponseWriter, r *http.Request) error {
	httpx.WriteJSON(w, http.StatusOK, s.Identity.Features())
	return nil
}

func (s *Server) requestPasswordReset(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Identifier string `json:"identifier"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Identity.RequestPasswordReset(r.Context(), body.Identifier, s.clientIP(r)); err != nil {
		return mapErr(err)
	}
	// Same answer whether or not anything was sent; see RequestPasswordReset.
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{
		"message": "If that account has a verified email or phone, a reset link is on its way.",
	})
	return nil
}

func (s *Server) inspectLink(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Token string `json:"token"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	info, err := s.Identity.InspectLink(r.Context(), body.Token)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, info)
	return nil
}

func (s *Server) completeLink(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
		// Only used for an invite, where the invitee picks their own.
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	res, err := s.Identity.CompleteLink(r.Context(), body.Token, body.NewPassword, body.Username, body.DisplayName)
	if err != nil {
		return mapErr(err)
	}
	// An invite makes the invitee and inviter friends. Best effort: the account
	// is already active, so a failure here must not fail the request.
	if res.InvitedBy != nil {
		if err := s.Social.BefriendOnInvite(r.Context(), *res.InvitedBy, res.UserID); err != nil {
			slog.ErrorContext(r.Context(), "invite befriend failed", "inviter", *res.InvitedBy, "invitee", res.UserID, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// invite lets a logged-in user invite someone by email. The response is the
// same whether or not a message was sent (see identity.Invite).
func (s *Server) invite(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Email string `json:"email"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Identity.Invite(r.Context(), caller(r), body.Email, s.clientIP(r)); err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"message": "Invitation sent."})
	return nil
}

func (s *Server) setContact(w http.ResponseWriter, r *http.Request) error {
	channel, err := contactChannel(r)
	if err != nil {
		return err
	}
	var body struct {
		Value    string `json:"value"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Identity.SetContact(r.Context(), caller(r), channel, body.Value, body.Password, s.clientIP(r)); err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"message": "A verification code has been sent."})
	return nil
}

func (s *Server) verifyContact(w http.ResponseWriter, r *http.Request) error {
	channel, err := contactChannel(r)
	if err != nil {
		return err
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	u, err := s.Identity.VerifyContact(r.Context(), caller(r), channel, body.Code)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (s *Server) removeContact(w http.ResponseWriter, r *http.Request) error {
	channel, err := contactChannel(r)
	if err != nil {
		return err
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	u, err := s.Identity.RemoveContact(r.Context(), caller(r), channel, body.Password, s.clientIP(r))
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

// Friends ---------------------------------------------------------------------

func (s *Server) listFriends(w http.ResponseWriter, r *http.Request) error {
	friends, err := s.Social.ListFriends(r.Context(), caller(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"friends": friends})
	return nil
}

func (s *Server) removeFriend(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "userID")
	if err != nil {
		return err
	}
	if err := s.Social.RemoveFriend(r.Context(), caller(r), id); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) error {
	in, out, err := s.Social.ListRequests(r.Context(), caller(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"incoming": in, "outgoing": out})
	return nil
}

func (s *Server) sendRequest(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Username string `json:"username"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	res, err := s.Social.SendRequest(r.Context(), caller(r), body.Username)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) acceptRequest(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "requestID")
	if err != nil {
		return err
	}
	f, err := s.Social.AcceptRequest(r.Context(), caller(r), id)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, f)
	return nil
}

func (s *Server) deleteRequest(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "requestID")
	if err != nil {
		return err
	}
	if err := s.Social.DeleteRequest(r.Context(), caller(r), id); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// Blocks ----------------------------------------------------------------------

func (s *Server) listBlocked(w http.ResponseWriter, r *http.Request) error {
	blocked, err := s.Social.ListBlocked(r.Context(), caller(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"blocked": blocked})
	return nil
}

func (s *Server) block(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "userID")
	if err != nil {
		return err
	}
	if err := s.Social.Block(r.Context(), caller(r), id); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) unblock(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "userID")
	if err != nil {
		return err
	}
	if err := s.Social.Unblock(r.Context(), caller(r), id); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
