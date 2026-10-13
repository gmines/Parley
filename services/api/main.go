// Command api serves Parley's REST API: accounts, sessions and the friend graph.
//
// Configuration (environment):
//
//	PARLEY_DATABASE_URL   Postgres URL (required)
//	PARLEY_SIGNING_KEY    base64 32-byte Ed25519 seed for access tokens (required;
//	                      `api keygen` prints a new one with its PARLEY_VERIFY_KEY)
//	PARLEY_LISTEN_ADDR    default ":8080"
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
		fmt.Printf("PARLEY_SIGNING_KEY=%s\nPARLEY_VERIFY_KEY=%s\n",
			base64.StdEncoding.EncodeToString(priv.Seed()), auth.EncodePublicKey(pub))
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("api exited", "err", err)
		os.Exit(1)
	}
}

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
	addr := envOr("PARLEY_LISTEN_ADDR", ":8080")
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
		RefreshTTL: refreshTTL,
		Hash:       identity.DefaultHashParams,
		Lockout:    lockout,
		Recovery:   recovery,
	})
	if err != nil {
		return err
	}
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
	slog.Info("config",
		"lockout_max_failures", lockout.MaxFailures, "lockout_duration", lockout.Duration.String(),
		"lockout_scope", lockout.Scope, "email_recovery", recovery.EmailEnabled, "sms_recovery", recovery.SMSEnabled)

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
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
