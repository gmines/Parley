// Command voice-control runs call rooms: it gives clients short-lived SIP
// credentials to join a conversation's call, and controls Asterisk over ARI
// to admit callers into the room's mixing bridge.
//
// Configuration (environment):
//
//	PARLEY_DATABASE_URL   the api's Postgres (read-only use: conversations, blocks). Required.
//	PARLEY_NATS_URL       NATS for call events (call.started, ...). Required.
//	PARLEY_VERIFY_KEY     base64 Ed25519 public key for access tokens. Required.
//	PARLEY_ARI_URL        default "http://127.0.0.1:8088/ari"
//	PARLEY_ARI_USER       default "parley"
//	PARLEY_ARI_PASSWORD   Required.
//	PARLEY_SIP_SECRET     shared with Kamailio. Required.
//	PARLEY_SIP_PROXY      what clients connect to, e.g. "sip:203.0.113.5:5060;transport=tcp". Required.
//	PARLEY_SIP_DOMAIN     host part of room URIs (default: the proxy's host)
//	PARLEY_SIP_CRED_TTL   default "5m"
//	PARLEY_LISTEN_ADDR    default ":8082"
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"parley/pkg/auth"
	"parley/pkg/buildinfo"
	"parley/pkg/db"
	"parley/pkg/events"
	"parley/services/voice-control/internal/ari"
	"parley/services/voice-control/internal/calls"
	"parley/services/voice-control/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("voice-control exited", "err", err)
		os.Exit(1)
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func required(names ...string) error {
	var missing []string
	for _, n := range names {
		if os.Getenv(n) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	return nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := required("PARLEY_DATABASE_URL", "PARLEY_NATS_URL", "PARLEY_VERIFY_KEY", "PARLEY_ARI_PASSWORD",
		"PARLEY_SIP_SECRET", "PARLEY_SIP_PROXY"); err != nil {
		return err
	}
	key, err := auth.ParsePublicKey(os.Getenv("PARLEY_VERIFY_KEY"))
	if err != nil {
		return fmt.Errorf("PARLEY_VERIFY_KEY: %w", err)
	}
	ttl, err := time.ParseDuration(env("PARLEY_SIP_CRED_TTL", "5m"))
	if err != nil {
		return fmt.Errorf("PARLEY_SIP_CRED_TTL: %w", err)
	}
	proxy := os.Getenv("PARLEY_SIP_PROXY")
	domain := env("PARLEY_SIP_DOMAIN", proxyHost(proxy))

	pool, err := db.Connect(ctx, os.Getenv("PARLEY_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	nc, err := nats.Connect(os.Getenv("PARLEY_NATS_URL"), nats.Name("parley-voice-control"), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	defer nc.Drain()
	ariClient, err := ari.New(env("PARLEY_ARI_URL", "http://127.0.0.1:8088/ari"), env("PARLEY_ARI_USER", "parley"), os.Getenv("PARLEY_ARI_PASSWORD"))
	if err != nil {
		return fmt.Errorf("PARLEY_ARI_URL: %w", err)
	}

	mgr := calls.NewManager(ariClient, calls.PGDirectory{Pool: pool}, events.NATS{Conn: nc})
	go ariClient.Run(ctx, calls.StasisApp, mgr.Resync, mgr.HandleEvent)

	srv := &server.Server{
		Calls:     mgr,
		Verifier:  auth.NewVerifier(key),
		SIPSecret: []byte(os.Getenv("PARLEY_SIP_SECRET")),
		SIPProxy:  proxy,
		SIPDomain: domain,
		CredTTL:   ttl,
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", env("PARLEY_LISTEN_ADDR", ":8082"))
	if err != nil {
		return err
	}
	b := buildinfo.Get()
	slog.Info("voice-control listening", "addr", ln.Addr().String(), "sip_proxy", proxy, "version", b.Version, "commit", b.Commit)
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// proxyHost extracts the host from "sip:host:port;params".
func proxyHost(proxy string) string {
	h := strings.TrimPrefix(proxy, "sip:")
	if i := strings.IndexAny(h, ";"); i >= 0 {
		h = h[:i]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}
