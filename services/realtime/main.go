// Command realtime pushes live events (new messages, friend requests, ...)
// to clients over WebSocket at GET /v1/realtime.
//
// Configuration (environment):
//
//	PARLEY_NATS_URL      NATS server the api publishes events to (required)
//	PARLEY_VERIFY_KEY    base64 Ed25519 public key for access tokens (required;
//	                     the api logs it at startup as PARLEY_VERIFY_KEY)
//	PARLEY_LISTEN_ADDR   default ":8081"
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
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"parley/pkg/auth"
	"parley/pkg/buildinfo"
	"parley/services/realtime/internal/gateway"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("realtime exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	natsURL := os.Getenv("PARLEY_NATS_URL")
	if natsURL == "" {
		return errors.New("PARLEY_NATS_URL is required")
	}
	key, err := auth.ParsePublicKey(os.Getenv("PARLEY_VERIFY_KEY"))
	if err != nil {
		return fmt.Errorf("PARLEY_VERIFY_KEY: %w", err)
	}
	addr := os.Getenv("PARLEY_LISTEN_ADDR")
	if addr == "" {
		addr = ":8081"
	}

	nc, err := nats.Connect(natsURL, nats.Name("parley-realtime"), nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { slog.Warn("nats disconnected", "err", err) }),
		nats.ReconnectHandler(func(*nats.Conn) { slog.Info("nats reconnected") }))
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	defer nc.Drain()

	gw := gateway.New(nc, auth.NewVerifier(key))
	mux := http.NewServeMux()
	mux.Handle("GET /v1/realtime", gw)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !nc.IsConnected() {
			http.Error(w, "nats disconnected", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// No Read/WriteTimeout: WebSocket connections are long-lived.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b := buildinfo.Get()
	slog.Info("realtime listening", "addr", ln.Addr().String(), "version", b.Version, "commit", b.Commit, "modified", b.Modified)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
