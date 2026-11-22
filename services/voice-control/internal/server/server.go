// Package server is voice-control's REST API (see api/openapi.yaml):
// credentials to join a call, declining, and listing active calls.
package server

import (
	"errors"
	"net/http"
	"slices"
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
	// Outbound proxies clients connect to. SIPProxyTLS (e.g.
	// sip:voice.example.org:5061;transport=tls) is given to clients that
	// support TLS; SIPProxy (plain TCP, e.g. sip:203.0.113.5:5060;transport=tcp)
	// to older ones. Either may be empty, not both.
	SIPProxy    string
	SIPProxyTLS string
	SIPDomain   string // host part of room URIs
	CredTTL     time.Duration
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
	case errors.Is(err, calls.ErrBadReason):
		return httpx.NewError(http.StatusBadRequest, "bad_request", err.Error())
	}
	return err
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) error {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"calls": s.Calls.Active(r.Context(), caller(r))})
	return nil
}

var errUpgradeRequired = httpx.NewError(http.StatusUpgradeRequired, "upgrade_required",
	"This server only accepts encrypted calls. Update Parley to join calls.")

// joinRequest is the optional body of a join: the SIP transports the client
// supports. Clients that send none (before 0.4) only support "tcp".
type joinRequest struct {
	Transports []string `json:"transports"`
}

// proxyFor picks the outbound proxy for a client: TLS whenever both sides
// support it, plain TCP only for clients that can't and only if enabled.
func (s *Server) proxyFor(transports []string) (string, error) {
	if len(transports) == 0 {
		transports = []string{"tcp"}
	}
	if s.SIPProxyTLS != "" && slices.Contains(transports, "tls") {
		return s.SIPProxyTLS, nil
	}
	if s.SIPProxy != "" && slices.Contains(transports, "tcp") {
		return s.SIPProxy, nil
	}
	return "", errUpgradeRequired
}

// join returns short-lived SIP credentials for the conversation's call room,
// plus the call's current state (null if nobody's in it yet: the caller's
// joining starts it).
func (s *Server) join(w http.ResponseWriter, r *http.Request) error {
	conv, err := convID(r)
	if err != nil {
		return err
	}
	var req joinRequest
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			return err
		}
	}
	proxy, err := s.proxyFor(req.Transports)
	if err != nil {
		return err
	}
	user := caller(r)
	if _, err := s.Calls.Authorize(r.Context(), user, conv); err != nil {
		return mapErr(err)
	}
	creds := calls.Mint(s.SIPSecret, user, conv, proxy, s.SIPDomain, s.CredTTL, time.Now())
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
	// Optional body: {"reason": "declined" | "failed"} (older clients send none).
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			return err
		}
	}
	if err := s.Calls.Decline(r.Context(), caller(r), conv, req.Reason); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
