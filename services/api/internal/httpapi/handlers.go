package httpapi

import (
	"net/http"

	"parley/pkg/httpx"
	"parley/services/api/internal/identity"
)

// Auth ------------------------------------------------------------------------

func (s *Server) register(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
		Phone       string `json:"phone"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	sess, err := s.Identity.Register(r.Context(), identity.Registration{
		Username:    body.Username,
		Password:    body.Password,
		DisplayName: body.DisplayName,
		Email:       body.Email,
		Phone:       body.Phone,
	})
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusCreated, sess)
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
		Username string `json:"username"`
		Channel  string `json:"channel"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Identity.RequestPasswordReset(r.Context(), body.Username, body.Channel); err != nil {
		return mapErr(err)
	}
	// Same answer whether or not anything was sent; see RequestPasswordReset.
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{
		"message": "If that account has a verified address for this method, a reset code is on its way.",
	})
	return nil
}

func (s *Server) confirmPasswordReset(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Username    string `json:"username"`
		Code        string `json:"code"`
		NewPassword string `json:"new_password"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Identity.ConfirmPasswordReset(r.Context(), body.Username, body.Code, body.NewPassword); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
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
