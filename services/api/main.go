// Command api serves Parley's REST API: accounts, sessions and the friend graph.
//
// Configuration (environment):
//
//	PARLEY_DATABASE_URL   Postgres URL (required)
//	PARLEY_SIGNING_KEY    base64 32-byte Ed25519 seed for access tokens (required;
//	                      `api keygen` prints a new one with its PARLEY_VERIFY_KEY)
//	PARLEY_MFA_KEY        base64 32-byte key sealing TOTP secrets (required)
//	PARLEY_LISTEN_ADDR    default ":8080"
//	PARLEY_INTERNAL_LISTEN_ADDR  serves the account page's API on this address,
//	                      which must not be exposed publicly; empty = off
//	PARLEY_ACCESS_TTL     default "15m"
//	PARLEY_REFRESH_TTL    default "720h"
//	PARLEY_NATS_URL       NATS server for realtime events, e.g. "nats://localhost:4222".
//	                      Without it nothing is pushed live (clients only see
//	                      changes when they reload).
//	PARLEY_REALTIME_PUBLIC_URL  WebSocket URL of the realtime service as clients
//	                      reach it, e.g. "wss://chat.example.org/v1/realtime";
//	                      advertised to clients via GET /v1/meta.
//	PARLEY_VOICE_PUBLIC_URL     base URL of voice-control as clients reach it,
//	                      e.g. "https://chat.example.org"; empty = same as the api.
//
// Login lockout and account recovery settings are documented in config.go.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"parley/pkg/auth"
	"parley/pkg/buildinfo"
	"parley/pkg/db"
	"parley/pkg/events"
	"parley/pkg/httpx"
	"parley/services/api/internal/httpapi"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/messaging"
	"parley/services/api/internal/settings"
	"parley/services/api/internal/social"
	"parley/services/api/migrations"
)

func main() {
	// `api keygen`: a new access-token key pair, for deployment secrets.
	if len(os.Args) == 2 && os.Args[1] == "keygen" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		mfa := make([]byte, 32)
		if _, err := rand.Read(mfa); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("PARLEY_SIGNING_KEY=%s\nPARLEY_VERIFY_KEY=%s\nPARLEY_MFA_KEY=%s\n",
			base64.StdEncoding.EncodeToString(priv.Seed()), auth.EncodePublicKey(pub), base64.StdEncoding.EncodeToString(mfa))
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	// `api mfa-reset <username>`: for someone who lost their authenticator and
	// recovery codes. Needs the same environment as the server.
	if len(os.Args) == 3 && os.Args[1] == "mfa-reset" {
		if err := adminResetMFA(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("MFA cleared; the user must set it up again at next login.")
		return
	}
	// `api admin <command>`: the admin app and its accounts; see admin.go.
	if len(os.Args) >= 2 && os.Args[1] == "admin" {
		if err := adminMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func adminResetMFA(username string) error {
	ctx := context.Background()
	key, err := auth.ParsePrivateKey(os.Getenv("PARLEY_SIGNING_KEY"))
	if err != nil {
		return fmt.Errorf("PARLEY_SIGNING_KEY: %w", err)
	}
	mfa, err := mfaKey()
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, os.Getenv("PARLEY_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	ident, err := identity.NewService(pool, identity.Config{
		Signer: auth.NewSigner(key, time.Minute), Verifier: auth.NewVerifier(key.Public().(ed25519.PublicKey)),
		MFAKey: mfa, Hash: identity.DefaultHashParams,
	})
	if err != nil {
		return err
	}
	return ident.AdminResetMFA(ctx, username)
}

// settingsPoll is how soon a change made in the admin app reaches the api.
const settingsPoll = 10 * time.Second

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("PARLEY_DATABASE_URL")
	if dbURL == "" {
		return errors.New("PARLEY_DATABASE_URL is required")
	}
	key, err := auth.ParsePrivateKey(os.Getenv("PARLEY_SIGNING_KEY"))
	if err != nil {
		return fmt.Errorf("PARLEY_SIGNING_KEY: %w", err)
	}
	accessTTL, err := durationEnv("PARLEY_ACCESS_TTL", 15*time.Minute)
	if err != nil {
		return err
	}
	refreshTTL, err := durationEnv("PARLEY_REFRESH_TTL", 30*24*time.Hour)
	if err != nil {
		return err
	}
	mfa, err := mfaKey()
	if err != nil {
		return err
	}
	addr := envOr("PARLEY_LISTEN_ADDR", ":8080")
	internalAddr := os.Getenv("PARLEY_INTERNAL_LISTEN_ADDR")
	lockout, err := lockoutConfig()
	if err != nil {
		return err
	}
	recovery, err := recoveryConfig()
	if err != nil {
		return err
	}
	trusted, err := httpx.ParsePrefixes(os.Getenv("PARLEY_TRUSTED_PROXIES"))
	if err != nil {
		return fmt.Errorf("PARLEY_TRUSTED_PROXIES: %w", err)
	}

	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		return err
	}

	ident, err := identity.NewService(pool, identity.Config{
		Signer:     auth.NewSigner(key, accessTTL),
		Verifier:   auth.NewVerifier(key.Public().(ed25519.PublicKey)),
		MFAKey:     mfa,
		RefreshTTL: refreshTTL,
		Hash:       identity.DefaultHashParams,
		Lockout:    lockout,
		Recovery:   recovery,
	})
	if err != nil {
		return err
	}
	// Settings changed in the admin app override the environment's, and are
	// picked up while running.
	store, err := settings.NewStore(pool, mfa)
	if err != nil {
		return err
	}
	base := ident.Runtime()
	if err := store.Apply(ctx, base, ident.SetRuntime); err != nil {
		slog.Error("loading stored settings; using the environment's", "err", err)
	}
	go store.Watch(ctx, base, ident.SetRuntime, settingsPoll)

	var pub events.Publisher = events.Discard{}
	if natsURL := os.Getenv("PARLEY_NATS_URL"); natsURL != "" {
		nc, err := nats.Connect(natsURL, nats.Name("parley-api"), nats.MaxReconnects(-1))
		if err != nil {
			return fmt.Errorf("PARLEY_NATS_URL: %w", err)
		}
		defer nc.Drain()
		pub = events.NATS{Conn: nc}
	} else {
		slog.Warn("PARLEY_NATS_URL not set: realtime events are disabled")
	}
	socialSvc := social.NewService(pool, pub)

	srv := &httpapi.Server{
		Identity:  ident,
		Messaging: messaging.NewService(pool, socialSvc, pub),
		Social:    socialSvc,
		Verifier:  auth.NewVerifier(key.Public().(ed25519.PublicKey)),
		Pool:      pool,

		TrustedProxies: trusted,
		RealtimeURL:    os.Getenv("PARLEY_REALTIME_PUBLIC_URL"),
		VoiceURL:       os.Getenv("PARLEY_VOICE_PUBLIC_URL"),
	}
	// The realtime service verifies access tokens with this public key
	// (its PARLEY_VERIFY_KEY).
	slog.Info("access token public key", "PARLEY_VERIFY_KEY", auth.EncodePublicKey(key.Public().(ed25519.PublicKey)))
	rt := ident.Runtime()
	slog.Info("config",
		"lockout_max_failures", rt.Lockout.MaxFailures, "lockout_duration", rt.Lockout.Duration.String(),
		"lockout_scope", rt.Lockout.Scope, "email_recovery", rt.Recovery.EmailEnabled, "sms_recovery", rt.Recovery.SMSEnabled,
		"access_ttl", rt.AccessTTL.String(), "refresh_ttl", rt.RefreshTTL.String())

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Bind before logging so a port conflict fails loudly instead of after "listening".
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b := buildinfo.Get()
	slog.Info("api listening", "addr", ln.Addr().String(), "version", b.Version, "commit", b.Commit, "modified", b.Modified)
	errc := make(chan error, 2)
	go func() { errc <- httpSrv.Serve(ln) }()

	var internalSrv *http.Server
	if internalAddr != "" {
		internalSrv = &http.Server{
			Addr:              internalAddr,
			Handler:           srv.InternalHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    8 << 10,
		}
		iln, err := net.Listen("tcp", internalAddr)
		if err != nil {
			return err
		}
		slog.Info("internal api listening", "addr", iln.Addr().String())
		go func() { errc <- internalSrv.Serve(iln) }()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if internalSrv != nil {
		_ = internalSrv.Shutdown(shutdownCtx)
	}
	return httpSrv.Shutdown(shutdownCtx)
}
