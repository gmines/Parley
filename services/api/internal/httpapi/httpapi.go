// Package httpapi exposes the api service's modules over REST. See api/openapi.yaml.
package httpapi

import (
	"errors"
	"math"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/auth"
	"parley/pkg/httpx"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/social"
)

type Server struct {
	Identity *identity.Service
	Social   *social.Service
	Verifier *auth.Verifier
	Pool     *pgxpool.Pool
	// TrustedProxies are the addresses (e.g. the ingress) whose
	// X-Forwarded-For header is believed when determining the client IP.
	TrustedProxies []netip.Prefix
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn httpx.Handler) { mux.Handle(pattern, fn) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h("GET /readyz", s.ready)

	h("POST /v1/auth/register", s.register)
	h("POST /v1/auth/login", s.login)
	h("POST /v1/auth/refresh", s.refresh)
	h("POST /v1/auth/logout", s.logout)
	h("GET /v1/auth/features", s.features)
	h("POST /v1/auth/password-reset/request", s.requestPasswordReset)
	h("POST /v1/auth/password-reset/confirm", s.confirmPasswordReset)

	authed := http.NewServeMux()
	authed.Handle("GET /v1/users/me", httpx.Handler(s.me))
	authed.Handle("POST /v1/users/me/password", httpx.Handler(s.changePassword))
	authed.Handle("PUT /v1/users/me/{contact}", httpx.Handler(s.setContact))
	authed.Handle("POST /v1/users/me/{contact}/verify", httpx.Handler(s.verifyContact))
	authed.Handle("DELETE /v1/users/me/{contact}", httpx.Handler(s.removeContact))
	authed.Handle("GET /v1/friends", httpx.Handler(s.listFriends))
	authed.Handle("DELETE /v1/friends/{userID}", httpx.Handler(s.removeFriend))
	authed.Handle("GET /v1/friends/requests", httpx.Handler(s.listRequests))
	authed.Handle("POST /v1/friends/requests", httpx.Handler(s.sendRequest))
	authed.Handle("POST /v1/friends/requests/{requestID}/accept", httpx.Handler(s.acceptRequest))
	authed.Handle("DELETE /v1/friends/requests/{requestID}", httpx.Handler(s.deleteRequest))
	authed.Handle("GET /v1/blocks", httpx.Handler(s.listBlocked))
	authed.Handle("PUT /v1/blocks/{userID}", httpx.Handler(s.block))
	authed.Handle("DELETE /v1/blocks/{userID}", httpx.Handler(s.unblock))

	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
	})
	protected := auth.Middleware(s.Verifier, unauthorized)(authed)
	mux.Handle("/v1/users/", protected)
	mux.Handle("/v1/friends", protected)
	mux.Handle("/v1/friends/", protected)
	mux.Handle("/v1/blocks", protected)
	mux.Handle("/v1/blocks/", protected)
	return mux
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) error {
	if err := s.Pool.Ping(r.Context()); err != nil {
		return httpx.NewError(http.StatusServiceUnavailable, "unavailable", "database unreachable")
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// caller returns the authenticated user's ID. Only valid behind auth.Middleware.
func caller(r *http.Request) uuid.UUID {
	c, _ := auth.FromContext(r.Context())
	return c.UserID
}

func (s *Server) clientIP(r *http.Request) string {
	return httpx.ClientIP(r, s.TrustedProxies)
}

// contactChannel maps the {contact} path segment ("email" or "phone") to a
// delivery channel.
func contactChannel(r *http.Request) (string, error) {
	switch r.PathValue("contact") {
	case "email":
		return "email", nil
	case "phone":
		return "sms", nil
	}
	return "", httpx.ErrNotFound
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, httpx.ErrNotFound
	}
	return id, nil
}

// mapErr translates domain errors to HTTP errors; anything unknown becomes a 500.
func mapErr(err error) error {
	type m struct {
		status int
		code   string
	}
	table := map[error]m{
		identity.ErrInvalidUsername:    {http.StatusBadRequest, "invalid_username"},
		identity.ErrInvalidPassword:    {http.StatusBadRequest, "invalid_password"},
		identity.ErrInvalidDisplayName: {http.StatusBadRequest, "invalid_display_name"},
		identity.ErrUsernameTaken:      {http.StatusConflict, "username_taken"},
		identity.ErrBadCredentials:     {http.StatusUnauthorized, "bad_credentials"},
		identity.ErrInvalidRefresh:     {http.StatusUnauthorized, "invalid_refresh_token"},
		identity.ErrUserNotFound:       {http.StatusNotFound, "user_not_found"},
		identity.ErrFeatureDisabled:    {http.StatusForbidden, "feature_disabled"},
		identity.ErrInvalidEmail:       {http.StatusBadRequest, "invalid_email"},
		identity.ErrInvalidPhone:       {http.StatusBadRequest, "invalid_phone"},
		identity.ErrInvalidCode:        {http.StatusBadRequest, "invalid_code"},
		identity.ErrContactInUse:       {http.StatusConflict, "contact_in_use"},
		identity.ErrTooManyCodes:       {http.StatusTooManyRequests, "too_many_codes"},
		identity.ErrInvalidChannel:     {http.StatusBadRequest, "invalid_channel"},
		social.ErrCannotSend:           {http.StatusBadRequest, "cannot_send_request"},
		social.ErrSelf:                 {http.StatusBadRequest, "self"},
		social.ErrAlreadyFriends:       {http.StatusConflict, "already_friends"},
		social.ErrYouBlocked:           {http.StatusConflict, "you_blocked_user"},
		social.ErrRequestNotFound:      {http.StatusNotFound, "request_not_found"},
		social.ErrNotFriends:           {http.StatusNotFound, "not_friends"},
		social.ErrUserNotFound:         {http.StatusNotFound, "user_not_found"},
	}
	if locked := (*identity.LockedError)(nil); errors.As(err, &locked) {
		e := httpx.NewError(http.StatusTooManyRequests, "too_many_attempts", locked.Error())
		e.Header = http.Header{"Retry-After": {strconv.Itoa(int(math.Ceil(locked.RetryAfter.Seconds())))}}
		return e
	}
	// Carries a specific hint, so its own message is used.
	if weak := (*identity.WeakPasswordError)(nil); errors.As(err, &weak) {
		return httpx.NewError(http.StatusBadRequest, "weak_password", weak.Error())
	}
	for target, e := range table {
		if errors.Is(err, target) {
			return httpx.NewError(e.status, e.code, target.Error())
		}
	}
	return err
}
