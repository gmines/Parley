package httpapi

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"parley/pkg/buildinfo"
	"parley/pkg/httpx"
)

// meta tells clients where the realtime service is and which optional
// features are enabled. Public, so clients can call it before logging in.
func (s *Server) meta(w http.ResponseWriter, r *http.Request) error {
	f := s.Identity.Features()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"server":         buildinfo.Get(),
		"realtime_url":   s.RealtimeURL,
		"voice_url":      s.VoiceURL,
		"email_recovery": f.EmailRecovery,
		"sms_recovery":   f.SMSRecovery,
	})
	return nil
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) error {
	convs, err := s.Messaging.ListConversations(r.Context(), caller(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"conversations": convs})
	return nil
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "convID")
	if err != nil {
		return err
	}
	c, err := s.Messaging.GetConversation(r.Context(), caller(r), id)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, c)
	return nil
}

func (s *Server) openDM(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		UserID uuid.UUID `json:"user_id"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	c, err := s.Messaging.OpenDM(r.Context(), caller(r), body.UserID)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, c)
	return nil
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		UserIDs []uuid.UUID `json:"user_ids"`
		Name    string      `json:"name"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	c, err := s.Messaging.CreateGroup(r.Context(), caller(r), body.UserIDs, body.Name)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
	return nil
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "convID")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	before, err := optionalUUID(q.Get("before"))
	if err != nil {
		return err
	}
	after, err := optionalUUID(q.Get("after"))
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	msgs, err := s.Messaging.ListMessages(r.Context(), caller(r), id, before, after, limit)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"messages": msgs})
	return nil
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "convID")
	if err != nil {
		return err
	}
	var body struct {
		Content string `json:"content"`
		Nonce   string `json:"nonce"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	msg, err := s.Messaging.SendMessage(r.Context(), caller(r), id, body.Content, body.Nonce)
	if err != nil {
		return mapErr(err)
	}
	httpx.WriteJSON(w, http.StatusCreated, msg)
	return nil
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "convID")
	if err != nil {
		return err
	}
	var body struct {
		MessageID uuid.UUID `json:"message_id"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if err := s.Messaging.MarkRead(r.Context(), caller(r), id, body.MessageID); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func optionalUUID(s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, httpx.NewError(http.StatusBadRequest, "bad_request", "invalid message id")
	}
	return &id, nil
}
