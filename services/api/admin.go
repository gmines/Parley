package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"parley/pkg/auth"
	"parley/pkg/db"
	"parley/services/api/internal/admin"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/settings"
	"parley/services/api/migrations"
)

const adminUsage = `usage: api admin <command>

  serve               run the admin app (PARLEY_ADMIN_LISTEN_ADDR, default 127.0.0.1:8090)
  create <username>   add an admin; prints a password and authenticator key once
  reset <username>    new password and authenticator key; signs the admin out
  disable <username>  turn an admin account off and sign it out
  enable <username>   turn it back on
  list                list admin accounts
  healthcheck         exit 0 if the admin app on this machine answers

The admin app listens on loopback only. Reach it through a tunnel, e.g.
  kubectl -n parley port-forward deploy/parley-admin 8090:8090
and open http://localhost:8090.

Same environment as the api: PARLEY_DATABASE_URL, PARLEY_MFA_KEY and the
account recovery settings (see config.go). PARLEY_SIGNING_KEY is not used.
  PARLEY_ADMIN_ALLOWED_HOSTS     extra Host names to accept besides localhost (comma-separated)
  PARLEY_ADMIN_INSECURE_COOKIE   "true" if your browser rejects Secure cookies on http://localhost`

func adminMain(args []string) error {
	if len(args) == 0 {
		return errors.New(adminUsage)
	}
	cmd, rest := args[0], args[1:]
	one := func() (string, error) {
		if len(rest) != 1 {
			return "", fmt.Errorf("usage: api admin %s <username>", cmd)
		}
		return rest[0], nil
	}
	switch cmd {
	case "serve":
		return adminServe()
	case "healthcheck":
		return adminHealthcheck()
	case "create", "reset":
		name, err := one()
		if err != nil {
			return err
		}
		return withAccounts(func(ctx context.Context, a *admin.Accounts) error {
			var creds admin.Credentials
			if cmd == "create" {
				creds, err = a.Create(ctx, name)
			} else {
				creds, err = a.ResetCredentials(ctx, name)
			}
			if err != nil {
				return err
			}
			printCredentials(creds)
			return nil
		})
	case "disable", "enable":
		name, err := one()
		if err != nil {
			return err
		}
		return withAccounts(func(ctx context.Context, a *admin.Accounts) error {
			if err := a.SetDisabled(ctx, name, cmd == "disable"); err != nil {
				return err
			}
			fmt.Printf("%s %sd.\n", name, cmd)
			return nil
		})
	case "list":
		return withAccounts(func(ctx context.Context, a *admin.Accounts) error {
			list, err := a.List(ctx)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "USERNAME\tCREATED\tSTATUS")
			for _, x := range list {
				status := "active"
				if x.Disabled {
					status = "disabled"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", x.Username, x.CreatedAt.Format(time.DateOnly), status)
			}
			return tw.Flush()
		})
	}
	return errors.New(adminUsage)
}

func printCredentials(c admin.Credentials) {
	fmt.Printf(`Admin account: %s

  Password:           %s
  Authenticator key:  %s

Add the key to an authenticator app ("enter a setup key", time-based), or
import this link:

  %s

These are shown only now. Sign in at the admin app with the password and a
6-digit code, then change the password under "Your account".
`, c.Username, c.Password, groupKey(c.Secret), c.OTPAuthURI)
}

// groupKey splits a base32 key into blocks of four for typing.
func groupKey(k string) string {
	var b strings.Builder
	for i, r := range k {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func withAccounts(fn func(context.Context, *admin.Accounts) error) error {
	ctx := context.Background()
	mfa, err := mfaKey()
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, os.Getenv("PARLEY_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		return err
	}
	a, err := admin.NewAccounts(pool, mfa, identity.DefaultHashParams)
	if err != nil {
		return err
	}
	return fn(ctx, a)
}

func adminAddr() string { return envOr("PARLEY_ADMIN_LISTEN_ADDR", "127.0.0.1:8090") }

// loopbackOnly refuses to serve the admin app where anything but this
// machine (or, in Kubernetes, this pod) could reach it.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("PARLEY_ADMIN_LISTEN_ADDR: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("PARLEY_ADMIN_LISTEN_ADDR must be a loopback address such as 127.0.0.1:8090, got %q; "+
		"reach the admin app through a tunnel instead of exposing it", addr)
}

func adminServe() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := adminAddr()
	if err := loopbackOnly(addr); err != nil {
		return err
	}
	dbURL := os.Getenv("PARLEY_DATABASE_URL")
	if dbURL == "" {
		return errors.New("PARLEY_DATABASE_URL is required")
	}
	mfa, err := mfaKey()
	if err != nil {
		return err
	}
	lockout, err := lockoutConfig()
	if err != nil {
		return err
	}
	recovery, err := recoveryConfig()
	if err != nil {
		return err
	}
	accessTTL, err := durationEnv("PARLEY_ACCESS_TTL", 15*time.Minute)
	if err != nil {
		return err
	}
	refreshTTL, err := durationEnv("PARLEY_REFRESH_TTL", 30*24*time.Hour)
	if err != nil {
		return err
	}

	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		return err
	}
	// The admin app never issues user tokens, so it gets a throwaway key
	// instead of the real signing key.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	ident, err := identity.NewService(pool, identity.Config{
		Signer: auth.NewSigner(priv, accessTTL), Verifier: auth.NewVerifier(pub),
		MFAKey: mfa, RefreshTTL: refreshTTL, Hash: identity.DefaultHashParams,
		Lockout: lockout, Recovery: recovery,
	})
	if err != nil {
		return err
	}
	store, err := settings.NewStore(pool, mfa)
	if err != nil {
		return err
	}
	base := ident.Runtime()
	if err := store.Apply(ctx, base, ident.SetRuntime); err != nil {
		slog.Error("loading stored settings; using the environment's", "err", err)
	}
	go store.Watch(ctx, base, ident.SetRuntime, settingsPoll)

	accounts, err := admin.NewAccounts(pool, mfa, identity.DefaultHashParams)
	if err != nil {
		return err
	}
	if list, err := accounts.List(ctx); err == nil && len(list) == 0 {
		slog.Warn("no admin accounts yet; create one with: api admin create <username>")
	}
	var hosts []string
	for _, h := range strings.Split(os.Getenv("PARLEY_ADMIN_ALLOWED_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	srv := &admin.Server{
		Accounts: accounts, Identity: ident, Settings: store, Pool: pool, Base: base,
		AllowedHosts: hosts, InsecureCookie: os.Getenv("PARLEY_ADMIN_INSECURE_COOKIE") == "true",
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // test sends wait on SMTP/Twilio
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	slog.Info("admin app listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// adminHealthcheck is for an exec probe: the server listens on loopback, which
// kubelet's HTTP probes can't reach.
func adminHealthcheck() error {
	_, port, err := net.SplitHostPort(adminAddr())
	if err != nil {
		return err
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin app answered %s", resp.Status)
	}
	return nil
}
