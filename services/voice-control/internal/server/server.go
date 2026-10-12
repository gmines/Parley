// Package server is voice-control's REST API (see api/openapi.yaml):
// credentials to join a call, declining, and listing active calls.
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"parley/pkg/auth"
	"parley/pkg/httpx"
	"parley/services/voice-control/internal/calls"
)

type Server struct {
	Calls    *calls.Manager
	Verifier *auth.Verifier

	SIPSecret []byte
	SIPProxy  string // outbound proxy clients connect to, e.g. sip:203.0.113.5:5060;transport=tcp
	SIPDomain string // host part of room URIs
	CredTTL   time.Duration
}

func (s *Server) Handler() http.Handler {
	authed := http.NewServeMux()
	authed.Handle("GET /v1/calls", httpx.Handler(s.list))
	authed.Handle("POST /v1/calls/{convID}/join", httpx.Handler(s.join))
	authed.Handle("POST /v1/calls/{convID}/decline", httpx.Handler(s.decline))
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/v1/calls", auth.Middleware(s.Verifier, unauthorized)(authed))
	mux.Handle("/v1/calls/", auth.Middleware(s.Verifier, unauthorized)(authed))
	return mux
}

func caller(r *http.Request) uuid.UUID {
	c, _ := auth.FromContext(r.Context())
	return c.UserID
}

func convID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("convID"))
	if err != nil {
		return uuid.Nil, httpx.ErrNotFound
	}
	return id, nil
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, calls.ErrNotMember):
		return httpx.NewError(http.StatusNotFound, "conversation_not_found", err.Error())
	case errors.Is(err, calls.ErrCannotCall):
		return httpx.NewError(http.StatusForbidden, "cannot_call", err.Error())
	}
	return err
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) error {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"calls": s.Calls.Active(r.Context(), caller(r))})
	return nil
}

// join returns short-lived SIP credentials for the conversation's call room,
// plus the call's current state (null if nobody's in it yet: the caller's
// joining starts it).
func (s *Server) join(w http.ResponseWriter, r *http.Request) error {
	conv, err := convID(r)
	if err != nil {
		return err
	}
	user := caller(r)
	if _, err := s.Calls.Authorize(r.Context(), user, conv); err != nil {
		return mapErr(err)
	}
	creds := calls.Mint(s.SIPSecret, user, conv, s.SIPProxy, s.SIPDomain, s.CredTTL, time.Now())
	var current *calls.Call
	if c, ok := s.Calls.Get(conv); ok {
		current = &c
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sip": creds, "call": current})
	return nil
}

func (s *Server) decline(w http.ResponseWriter, r *http.Request) error {
	conv, err := convID(r)
	if err != nil {
		return err
	}
	if err := s.Calls.Decline(r.Context(), caller(r), conv); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
