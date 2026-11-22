package admin

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/httpx"
	"parley/services/api/internal/delivery"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/settings"
)

//go:embed static
var static embed.FS

const (
	cookieName = "parley_admin"
	// Every state-changing request must carry this header. A cross-site page
	// can't add a custom header without a CORS preflight, which this server
	// never approves, so forms and links from elsewhere can't act as the admin.
	csrfHeader = "X-Parley-Admin"
)

// Server is the admin app's HTTP interface.
type Server struct {
	Accounts *Accounts
	Identity *identity.Service
	Settings *settings.Store
	Pool     *pgxpool.Pool
	// Base is the deployment's (environment) configuration, which stored
	// settings override.
	Base identity.Runtime
	// AllowedHosts are extra Host header values to accept, besides localhost.
	// Checking Host defeats DNS rebinding: a web page can't point its own
	// hostname at 127.0.0.1 and talk to this server.
	AllowedHosts []string
	// InsecureCookie drops the cookie's Secure flag, for browsers that don't
	// treat http://localhost as secure.
	InsecureCookie bool
}

type adminHandler func(w http.ResponseWriter, r *http.Request, adm Admin) error

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(files))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle("POST /api/login", httpx.Handler(s.login))
	a := func(pattern string, h adminHandler) { mux.Handle(pattern, s.authed(h)) }
	a("POST /api/logout", s.logout)
	a("GET /api/me", s.me)
	a("POST /api/me/password", s.changePassword)
	a("GET /api/admins", s.listAdmins)
	a("GET /api/audit", s.auditLog)

	a("GET /api/users", s.listUsers)
	a("POST /api/users", s.createUser)
	a("GET /api/users/{id}", s.getUser)
	a("PATCH /api/users/{id}", s.updateUser)
	a("PUT /api/users/{id}/contact/{kind}", s.setContact)
	a("DELETE /api/users/{id}/contact/{kind}", s.removeContact)
	a("PUT /api/users/{id}/mfa-policy", s.setMFAPolicy)
	a("POST /api/users/{id}/mfa/reset", s.resetMFA)
	a("POST /api/users/{id}/password/send-link", s.sendResetLink)
	a("POST /api/users/{id}/password/create-link", s.createResetLink)
	a("POST /api/users/{id}/sessions/revoke", s.revokeSessions)
	a("POST /api/users/{id}/disable", s.disableUser)
	a("POST /api/users/{id}/enable", s.enableUser)

	a("GET /api/settings", s.getSettings)
	a("PUT /api/settings/{group}", s.putSetting)
	a("DELETE /api/settings/{group}", s.revertSetting)
	a("POST /api/settings/test/{kind}", s.testDelivery)
	return s.guard(mux)
}

// guard applies the checks every request needs, before routing.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")

		if !s.hostAllowed(r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(csrfHeader) != "1" || !sameOrigin(r) {
				httpx.WriteError(w, r, httpx.NewError(http.StatusForbidden, "cross_origin", "request refused"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	for _, a := range s.AllowedHosts {
		if strings.EqualFold(strings.TrimSpace(a), host) {
			return true
		}
	}
	return false
}

// sameOrigin rejects requests a browser marks as coming from another site.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func (s *Server) authed(h adminHandler) http.Handler {
	return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		c, err := r.Cookie(cookieName)
		if err != nil {
			return errUnauthorized
		}
		adm, err := s.Accounts.Session(r.Context(), c.Value)
		if errors.Is(err, ErrNoSession) {
			s.clearCookie(w)
			return errUnauthorized
		}
		if err != nil {
			return err
		}
		return h(w, r, adm)
	})
}

var errUnauthorized = httpx.NewError(http.StatusUnauthorized, "signed_out", "Your session ended. Sign in again.")

func (s *Server) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: !s.InsecureCookie,
		SameSite: http.SameSiteStrictMode, MaxAge: int(SessionAbsolute.Seconds()),
	})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: !s.InsecureCookie,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

// adminErr turns expected failures into messages for the operator.
func adminErr(err error) error {
	var v *settings.ValidationError
	if errors.As(err, &v) {
		return httpx.NewError(http.StatusBadRequest, "invalid", v.Msg)
	}
	var weak *identity.WeakPasswordError
	if errors.As(err, &weak) {
		return httpx.NewError(http.StatusBadRequest, "weak_password", weak.Error())
	}
	switch {
	case errors.Is(err, identity.ErrUserNotFound), errors.Is(err, errUserNotFound):
		return httpx.NewError(http.StatusNotFound, "not_found", "That user no longer exists.")
	case errors.Is(err, ErrBadLogin):
		return httpx.NewError(http.StatusUnauthorized, "bad_login", err.Error())
	case errors.Is(err, ErrLoginLocked):
		return httpx.NewError(http.StatusTooManyRequests, "locked", err.Error())
	}
	for _, known := range []error{
		identity.ErrInvalidUsername, identity.ErrUsernameTaken,
		identity.ErrInvalidDisplayName, identity.ErrInvalidEmail, identity.ErrInvalidPhone, identity.ErrContactInUse,
		identity.ErrFeatureDisabled, identity.ErrInvalidMFAPolicy, identity.ErrNoAccountPage, identity.ErrNoVerifiedContact,
		identity.ErrNotActivated, identity.ErrAccountDisabled, identity.ErrInvalidChannel, identity.ErrTooManyCodes,
		identity.ErrInvalidPassword, ErrWrongPassword, ErrSamePassword,
	} {
		if errors.Is(err, known) {
			return httpx.NewError(http.StatusBadRequest, "invalid", known.Error())
		}
	}
	return err
}

// Session -----------------------------------------------------------------------

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	token, adm, err := s.Accounts.Login(r.Context(), body.Username, body.Password, body.Code, clientIP(r))
	if err != nil {
		return adminErr(err)
	}
	s.setCookie(w, token)
	httpx.WriteJSON(w, http.StatusOK, adm)
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, adm Admin) error {
	if c, err := r.Cookie(cookieName); err == nil {
		if err := s.Accounts.Logout(r.Context(), c.Value); err != nil {
			return err
		}
	}
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, adm Admin) error {
	httpx.WriteJSON(w, http.StatusOK, adm)
	return nil
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, adm Admin) error {
	var body struct {
		Current string `json:"current_password"`
		Code    string `json:"code"`
		New     string `json:"new_password"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	c, _ := r.Cookie(cookieName)
	if err := s.Accounts.ChangePassword(r.Context(), adm, c.Value, body.Current, body.Code, body.New, clientIP(r)); err != nil {
		return adminErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listAdmins(w http.ResponseWriter, r *http.Request, adm Admin) error {
	list, err := s.Accounts.List(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"admins": list})
	return nil
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request, adm Admin) error {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	entries, err := s.Accounts.AuditLog(r.Context(), before, 100)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
	return nil
}

// Users -------------------------------------------------------------------------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, adm Admin) error {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	users, total, err := searchUsers(r.Context(), s.Pool, q.Get("q"), limit, offset)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": users, "total": total})
	return nil
}

// createUser makes a new inactive account and returns its activation link. The
// link is handed back for the admin to pass on, and emailed as a welcome
// message when email delivery is on.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request, adm Admin) error {
	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	res, err := s.Identity.AdminCreateUser(r.Context(), identity.NewUser{
		Username: body.Username, DisplayName: body.DisplayName, Email: body.Email,
	})
	if err != nil {
		return adminErr(err)
	}
	// The link itself is a credential, so only the fact goes in the log.
	s.Accounts.Audit(r.Context(), adm.Username, "user.create", res.User.Username,
		map[string]any{"user_id": res.User.ID, "emailed": res.Emailed}, clientIP(r))
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"user": res.User, "url": res.URL, "expires_at": res.ExpiresAt, "emailed": res.Emailed,
	})
	return nil
}

func userID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, httpx.NewError(http.StatusNotFound, "not_found", "That user no longer exists.")
	}
	return id, nil
}

// respondUser writes the account's current row.
func (s *Server) respondUser(w http.ResponseWriter, r *http.Request, id uuid.UUID) error {
	u, err := getUserRow(r.Context(), s.Pool, id)
	if err != nil {
		return adminErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, adm Admin) error {
	id, err := userID(r)
	if err != nil {
		return err
	}
	return s.respondUser(w, r, id)
}

// userAction runs an action on the account in the path, audits it and
// answers with the account's new state.
func (s *Server) userAction(w http.ResponseWriter, r *http.Request, adm Admin, action string, detail map[string]any,
	do func(ctx context.Context, id uuid.UUID) error) error {
	id, err := userID(r)
	if err != nil {
		return err
	}
	before, err := getUserRow(r.Context(), s.Pool, id)
	if err != nil {
		return adminErr(err)
	}
	if err := do(r.Context(), id); err != nil {
		return adminErr(err)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["user_id"] = id
	s.Accounts.Audit(r.Context(), adm.Username, action, before.Username, detail, clientIP(r))
	return s.respondUser(w, r, id)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, adm Admin) error {
	var body struct {
		DisplayName string `json:"display_name"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	return s.userAction(w, r, adm, "user.display_name", map[string]any{"display_name": body.DisplayName},
		func(ctx context.Context, id uuid.UUID) error {
			_, err := s.Identity.AdminSetDisplayName(ctx, id, body.DisplayName)
			return err
		})
}

func contactKind(r *http.Request) (string, error) {
	switch r.PathValue("kind") {
	case "email":
		return "email", nil
	case "phone":
		return "sms", nil
	}
	return "", httpx.ErrNotFound
}

func (s *Server) setContact(w http.ResponseWriter, r *http.Request, adm Admin) error {
	channel, err := contactKind(r)
	if err != nil {
		return err
	}
	var body struct {
		Value    string `json:"value"`
		Verified bool   `json:"verified"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	if strings.TrimSpace(body.Value) == "" {
		return httpx.NewError(http.StatusBadRequest, "invalid", "Enter a value, or use Remove.")
	}
	return s.userAction(w, r, adm, "user.contact.set", map[string]any{"channel": channel, "value": body.Value, "verified": body.Verified},
		func(ctx context.Context, id uuid.UUID) error {
			_, err := s.Identity.AdminSetContact(ctx, id, channel, body.Value, body.Verified)
			return err
		})
}

func (s *Server) removeContact(w http.ResponseWriter, r *http.Request, adm Admin) error {
	channel, err := contactKind(r)
	if err != nil {
		return err
	}
	return s.userAction(w, r, adm, "user.contact.remove", map[string]any{"channel": channel},
		func(ctx context.Context, id uuid.UUID) error {
			_, err := s.Identity.AdminSetContact(ctx, id, channel, "", false)
			return err
		})
}

func (s *Server) setMFAPolicy(w http.ResponseWriter, r *http.Request, adm Admin) error {
	var body struct {
		Policy string `json:"policy"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	return s.userAction(w, r, adm, "user.mfa_policy", map[string]any{"policy": body.Policy},
		func(ctx context.Context, id uuid.UUID) error {
			_, err := s.Identity.AdminSetMFAPolicy(ctx, id, body.Policy)
			return err
		})
}

func (s *Server) resetMFA(w http.ResponseWriter, r *http.Request, adm Admin) error {
	return s.userAction(w, r, adm, "user.mfa_reset", nil, s.Identity.AdminResetMFAByID)
}

func (s *Server) revokeSessions(w http.ResponseWriter, r *http.Request, adm Admin) error {
	return s.userAction(w, r, adm, "user.sessions_revoke", nil, s.Identity.AdminRevokeSessions)
}

func (s *Server) disableUser(w http.ResponseWriter, r *http.Request, adm Admin) error {
	return s.userAction(w, r, adm, "user.disable", nil, func(ctx context.Context, id uuid.UUID) error {
		_, err := s.Identity.AdminSetDisabled(ctx, id, true)
		return err
	})
}

func (s *Server) enableUser(w http.ResponseWriter, r *http.Request, adm Admin) error {
	return s.userAction(w, r, adm, "user.enable", nil, func(ctx context.Context, id uuid.UUID) error {
		_, err := s.Identity.AdminSetDisabled(ctx, id, false)
		return err
	})
}

func (s *Server) sendResetLink(w http.ResponseWriter, r *http.Request, adm Admin) error {
	id, err := userID(r)
	if err != nil {
		return err
	}
	u, err := getUserRow(r.Context(), s.Pool, id)
	if err != nil {
		return adminErr(err)
	}
	channel, err := s.Identity.AdminSendResetLink(r.Context(), id)
	if err != nil {
		return adminErr(err)
	}
	s.Accounts.Audit(r.Context(), adm.Username, "user.reset_link.send", u.Username, map[string]any{"user_id": id, "channel": channel}, clientIP(r))
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"channel": channel})
	return nil
}

func (s *Server) createResetLink(w http.ResponseWriter, r *http.Request, adm Admin) error {
	id, err := userID(r)
	if err != nil {
		return err
	}
	u, err := getUserRow(r.Context(), s.Pool, id)
	if err != nil {
		return adminErr(err)
	}
	url, expires, err := s.Identity.AdminCreateResetLink(r.Context(), id)
	if err != nil {
		return adminErr(err)
	}
	// The link itself is a credential, so only the fact goes in the log.
	s.Accounts.Audit(r.Context(), adm.Username, "user.reset_link.create", u.Username, map[string]any{"user_id": id}, clientIP(r))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"url": url, "expires_at": expires})
	return nil
}

// Settings ----------------------------------------------------------------------

// groupView is one settings group as the page shows it.
type groupView struct {
	Source  string         `json:"source"` // "deployment" or "override"
	Value   map[string]any `json:"value"`
	Meta    *settings.Meta `json:"meta,omitempty"`
	Problem string         `json:"problem,omitempty"` // why a stored override isn't in effect
}

func lockoutView(c identity.LockoutConfig) map[string]any {
	return map[string]any{"max_failures": c.MaxFailures, "duration": settings.FormatDuration(c.Duration), "window": settings.FormatDuration(c.Window),
		"scope": string(c.Scope), "account_max_failures": c.AccountMaxFailures}
}

func emailView(enabled bool, sender delivery.Sender) map[string]any {
	if !enabled {
		return map[string]any{"mode": "off"}
	}
	switch x := sender.(type) {
	case *delivery.SMTP:
		tls := "starttls"
		if x.ImplicitTLS {
			tls = "implicit"
		}
		return map[string]any{"mode": "smtp", "host": x.Host, "port": x.Port, "username": x.Username, "from": x.From,
			"tls": tls, "has_password": x.Password != ""}
	case delivery.Log:
		return map[string]any{"mode": "log"}
	}
	return map[string]any{"mode": "unknown"}
}

func smsView(enabled bool, sender delivery.Sender) map[string]any {
	if !enabled {
		return map[string]any{"mode": "off"}
	}
	switch x := sender.(type) {
	case *delivery.Twilio:
		return map[string]any{"mode": "twilio", "account_sid": x.AccountSID, "from": x.From, "has_auth_token": x.AuthToken != ""}
	case delivery.Log:
		return map[string]any{"mode": "log"}
	}
	return map[string]any{"mode": "unknown"}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, adm Admin) error {
	snap, err := s.Settings.Load(r.Context())
	if err != nil {
		return err
	}
	_, problems := s.Settings.Resolve(s.Base, snap)
	meta := func(k string) *settings.Meta {
		if m, ok := snap.Meta[k]; ok {
			return &m
		}
		return nil
	}
	view := func(k string, stored bool, override, deployment map[string]any) groupView {
		g := groupView{Source: "deployment", Value: deployment}
		if stored {
			g = groupView{Source: "override", Value: override, Meta: meta(k)}
			if p := problems[k]; p != nil {
				g.Problem = p.Error()
			}
		}
		return g
	}
	b := s.Base
	out := map[string]any{
		"account_url": b.Recovery.AccountURL,
		"mfa":         map[string]any{"require": snap.MFA.Require, "meta": meta(settings.KeyMFA)},
	}
	var lo, se, em, sm map[string]any
	if l := snap.Lockout; l != nil {
		lo = map[string]any{"max_failures": l.MaxFailures, "duration": l.Duration, "window": l.Window, "scope": l.Scope,
			"account_max_failures": l.AccountMaxFailures}
	}
	if x := snap.Sessions; x != nil {
		se = map[string]any{"access_ttl": x.AccessTTL, "refresh_ttl": x.RefreshTTL}
	}
	if e := snap.Email; e != nil {
		em = map[string]any{"mode": e.Mode, "host": e.Host, "port": e.Port, "username": e.Username, "from": e.From,
			"tls": e.TLS, "has_password": e.PasswordSealed != nil}
	}
	if x := snap.SMS; x != nil {
		sm = map[string]any{"mode": x.Mode, "account_sid": x.AccountSID, "from": x.From, "has_auth_token": x.AuthTokenSealed != nil}
	}
	out[settings.KeyLockout] = view(settings.KeyLockout, snap.Lockout != nil, lo, lockoutView(b.Lockout))
	out[settings.KeySessions] = view(settings.KeySessions, snap.Sessions != nil, se,
		map[string]any{"access_ttl": settings.FormatDuration(b.AccessTTL), "refresh_ttl": settings.FormatDuration(b.RefreshTTL)})
	out[settings.KeyEmail] = view(settings.KeyEmail, snap.Email != nil, em, emailView(b.Recovery.EmailEnabled, b.Recovery.Email))
	out[settings.KeySMS] = view(settings.KeySMS, snap.SMS != nil, sm, smsView(b.Recovery.SMSEnabled, b.Recovery.SMS))
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) putSetting(w http.ResponseWriter, r *http.Request, adm Admin) error {
	ctx, group, by := r.Context(), r.PathValue("group"), adm.Username
	url := s.Base.Recovery.AccountURL
	var (
		err    error
		detail map[string]any
	)
	switch group {
	case settings.KeyMFA:
		var v settings.MFA
		if err := httpx.DecodeJSON(w, r, &v); err != nil {
			return err
		}
		err, detail = s.Settings.PutMFA(ctx, v, by), map[string]any{"require": v.Require}
	case settings.KeyLockout:
		var v settings.Lockout
		if err := httpx.DecodeJSON(w, r, &v); err != nil {
			return err
		}
		err, detail = s.Settings.PutLockout(ctx, v, by), map[string]any{"value": v}
	case settings.KeySessions:
		var v settings.Sessions
		if err := httpx.DecodeJSON(w, r, &v); err != nil {
			return err
		}
		err, detail = s.Settings.PutSessions(ctx, v, by), map[string]any{"value": v}
	case settings.KeyEmail:
		var v settings.Email
		if err := httpx.DecodeJSON(w, r, &v); err != nil {
			return err
		}
		detail = map[string]any{"mode": v.Mode, "host": v.Host, "port": v.Port, "username": v.Username, "from": v.From,
			"tls": v.TLS, "password_changed": v.Password != ""}
		err = s.Settings.PutEmail(ctx, v, url, by)
	case settings.KeySMS:
		var v settings.SMS
		if err := httpx.DecodeJSON(w, r, &v); err != nil {
			return err
		}
		detail = map[string]any{"mode": v.Mode, "account_sid": v.AccountSID, "from": v.From, "auth_token_changed": v.AuthToken != ""}
		err = s.Settings.PutSMS(ctx, v, url, by)
	default:
		return httpx.ErrNotFound
	}
	if err != nil {
		return adminErr(err)
	}
	s.Accounts.Audit(ctx, by, "settings."+group, "", detail, clientIP(r))
	s.applySettings(ctx)
	return s.getSettings(w, r, adm)
}

func (s *Server) revertSetting(w http.ResponseWriter, r *http.Request, adm Admin) error {
	group := r.PathValue("group")
	switch group {
	case settings.KeyMFA, settings.KeyLockout, settings.KeySessions, settings.KeyEmail, settings.KeySMS:
	default:
		return httpx.ErrNotFound
	}
	if err := s.Settings.Revert(r.Context(), group, adm.Username); err != nil {
		return err
	}
	s.Accounts.Audit(r.Context(), adm.Username, "settings."+group+".revert", "", nil, clientIP(r))
	s.applySettings(r.Context())
	return s.getSettings(w, r, adm)
}

// applySettings updates this process at once; the api servers follow within
// their polling interval.
func (s *Server) applySettings(ctx context.Context) {
	_ = s.Settings.Apply(ctx, s.Base, s.Identity.SetRuntime)
}

func (s *Server) testDelivery(w http.ResponseWriter, r *http.Request, adm Admin) error {
	var body struct {
		To string `json:"to"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		return err
	}
	to := strings.TrimSpace(body.To)
	if to == "" || strings.ContainsAny(to, "\r\n") {
		return httpx.NewError(http.StatusBadRequest, "invalid", "Enter where to send the test message.")
	}
	rc := s.Identity.Runtime().Recovery
	var (
		sender  delivery.Sender
		enabled bool
		kind    = r.PathValue("kind")
	)
	switch kind {
	case "email":
		sender, enabled = rc.Email, rc.EmailEnabled
	case "sms":
		sender, enabled = rc.SMS, rc.SMSEnabled
	default:
		return httpx.ErrNotFound
	}
	if !enabled || sender == nil {
		return httpx.NewError(http.StatusBadRequest, "invalid", "This channel is off. Save working settings first.")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := sender.Send(ctx, delivery.Message{To: to, Subject: rc.AppName + " test message",
		Body: "This is a test message from the " + rc.AppName + " admin app. Delivery works."})
	s.Accounts.Audit(r.Context(), adm.Username, "settings.test."+kind, to, map[string]any{"ok": err == nil}, clientIP(r))
	if err != nil {
		return httpx.NewError(http.StatusBadGateway, "delivery_failed", "Sending failed: "+err.Error())
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"message": "Sent."})
	return nil
}
