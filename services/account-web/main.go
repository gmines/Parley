// Command account-web is the public page behind sign-up and password-reset
// links. It is the only Parley component meant to sit on the open web, so it
// is deliberately small: it holds no secrets and no database access, uses only
// the standard library, serves three embedded files, and relays two JSON calls
// to a fixed internal address. Nothing a visitor sends can choose where a
// request goes.
//
// Configuration (environment):
//
//	PARLEY_ACCOUNT_API_URL    internal api address, e.g. "http://parley-api:8081" (required)
//	PARLEY_ACCOUNT_HOST       public hostname; other Origins are refused (required)
//	PARLEY_LISTEN_ADDR        default ":8080"
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

//go:embed static
var static embed.FS

const (
	maxBody     = 2 << 10 // a token and a 128-character password fit easily
	maxInFlight = 32      // relayed calls at once; each can cost an Argon2 hash
)

var tokenFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type server struct {
	apiURL string
	host   string
	client *http.Client
	slots  chan struct{}
}

func newServer(apiURL, host string) *server {
	return &server{
		apiURL: strings.TrimRight(apiURL, "/"),
		host:   host,
		client: &http.Client{
			Timeout: 15 * time.Second,
			// The upstream is fixed and never redirects; refuse to follow one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		slots: make(chan struct{}, maxInFlight),
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /account/set-password", file("static/index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /account/app.js", file("static/app.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /account/style.css", file("static/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("POST /account/api/inspect", s.relay("/internal/v1/links/inspect", false))
	mux.HandleFunc("POST /account/api/complete", s.relay("/internal/v1/links/complete", true))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; "+
			"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func file(name, contentType string) http.Handler {
	b, err := static.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write(b)
	})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// relay validates a call and forwards only its known fields. Bodies carry
// passwords and tokens, so nothing here logs them.
func (s *server) relay(path string, withPassword bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// JSON can't be sent cross-site without a CORS preflight, which this
		// server never approves. The Origin check backs that up.
		if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(ct) != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, apiError{"bad_request", "unsupported content type"})
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "https://"+s.host {
			writeJSON(w, http.StatusForbidden, apiError{"bad_origin", "request refused"})
			return
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			writeJSON(w, http.StatusServiceUnavailable, apiError{"busy", "the server is busy; try again in a moment"})
			return
		}

		var in struct {
			Token       string `json:"token"`
			NewPassword string `json:"new_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{"bad_request", "malformed request"})
			return
		}
		// A malformed token can't be valid, so it never reaches the api.
		if !tokenFormat.MatchString(in.Token) {
			writeJSON(w, http.StatusBadRequest, apiError{"invalid_link", "this link is invalid or has expired; request a new one"})
			return
		}
		out := map[string]string{"token": in.Token}
		if withPassword {
			out["new_password"] = in.NewPassword
		}
		s.forward(w, r.Context(), path, out)
	}
}

// forward calls the api and passes back only a status and known fields, never
// upstream headers or raw bodies.
func (s *server) forward(w http.ResponseWriter, ctx context.Context, path string, payload any) {
	fail := func(err error) {
		slog.ErrorContext(ctx, "upstream call failed", "path", path, "err", err)
		writeJSON(w, http.StatusBadGateway, apiError{"unavailable", "something went wrong; try again in a moment"})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fail(err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL+path, bytes.NewReader(body))
	if err != nil {
		fail(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		fail(err)
		return
	}
	switch {
	case resp.StatusCode == http.StatusNoContent:
		w.WriteHeader(http.StatusNoContent)
	case resp.StatusCode == http.StatusOK:
		var info struct {
			Purpose  string `json:"purpose"`
			Username string `json:"username"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			fail(err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		var e apiError
		if err := json.Unmarshal(raw, &e); err != nil {
			fail(err)
			return
		}
		writeJSON(w, resp.StatusCode, e)
	default:
		fail(errors.New(resp.Status))
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	apiURL, host := os.Getenv("PARLEY_ACCOUNT_API_URL"), os.Getenv("PARLEY_ACCOUNT_HOST")
	if apiURL == "" || host == "" {
		slog.Error("PARLEY_ACCOUNT_API_URL and PARLEY_ACCOUNT_HOST are required")
		os.Exit(1)
	}
	addr := os.Getenv("PARLEY_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newServer(apiURL, host).handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("account-web listening", "addr", addr)
	select {
	case err := <-errc:
		slog.Error("account-web exited", "err", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
}
