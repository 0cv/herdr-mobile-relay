// Command herdr-gateway runs the blind WebSocket rendezvous gateway that pairs
// Herdr computer relays with their phones. It holds no secrets, sees only
// ciphertext, and is configured entirely through the environment so a container
// needs no command line.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/gateway"
)

// drainTimeout bounds how long the HTTP server is given to finish in-flight
// requests after the relay links have been dropped.
const drainTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	addr := envString("HERDR_GATEWAY_ADDR", ":8443")

	logger, err := newLogger(envString("HERDR_GATEWAY_LOG_FORMAT", "text"))
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	opts, err := loadOptions()
	if err != nil {
		return err
	}
	opts.Logger = logger

	srv, err := gateway.New(opts)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// Read and write deadlines are deliberately absent: both WebSocket
		// routes are long lived. Only the header read is bounded.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", addr)
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = srv.Close()
			return fmt.Errorf("gateway: listen on %s: %w", addr, err)
		}
	case <-ctx.Done():
		logger.Info("gateway shutting down")
	}

	// Drop the relay links first so their handlers return, then let the HTTP
	// server drain whatever plain requests are still in flight.
	closeErr := srv.Close()

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := httpServer.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("gateway: shutdown: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("gateway: close: %w", closeErr)
	}
	logger.Info("gateway stopped")
	return nil
}

func loadOptions() (gateway.Options, error) {
	var opts gateway.Options
	var err error

	if opts.MaxRelays, err = envInt("HERDR_GATEWAY_MAX_RELAYS"); err != nil {
		return opts, err
	}
	if opts.MaxClients, err = envInt("HERDR_GATEWAY_MAX_CLIENTS"); err != nil {
		return opts, err
	}
	if opts.MaxClientsPerRelay, err = envInt("HERDR_GATEWAY_MAX_CLIENTS_PER_RELAY"); err != nil {
		return opts, err
	}
	if opts.ConnectRatePerMinute, err = envInt("HERDR_GATEWAY_CONNECT_RATE_PER_MINUTE"); err != nil {
		return opts, err
	}
	if opts.QuotaWarnPercent, err = envInt("HERDR_GATEWAY_QUOTA_WARN_PERCENT"); err != nil {
		return opts, err
	}

	// HERDR_GATEWAY_MONTHLY_BYTES=0 means unlimited, which Options spells as a
	// negative limit because zero selects the default there.
	monthly, set, err := envInt64("HERDR_GATEWAY_MONTHLY_BYTES")
	if err != nil {
		return opts, err
	}
	if set {
		opts.MonthlyBytes = monthly
		if monthly == 0 {
			opts.MonthlyBytes = -1
		}
	}

	idleSeconds, err := envInt("HERDR_GATEWAY_IDLE_TIMEOUT")
	if err != nil {
		return opts, err
	}
	if idleSeconds < 0 {
		opts.IdleTimeout = -1
	} else {
		opts.IdleTimeout = time.Duration(idleSeconds) * time.Second
	}

	opts.StatePath = envString("HERDR_GATEWAY_STATE", "")
	// Address discovery listens on its own UDP port. gateway.Options treats an
	// empty address as disabled, so the ":3478" default lives here — and unlike
	// every other string variable an explicitly empty value is meaningful, since
	// setting HERDR_GATEWAY_STUN_ADDR= is how an operator who cannot open UDP
	// switches discovery off. envString folds empty into its fallback, so this
	// reads the variable directly.
	opts.STUNAddr = ":3478"
	if raw, ok := os.LookupEnv("HERDR_GATEWAY_STUN_ADDR"); ok {
		opts.STUNAddr = strings.TrimSpace(raw)
	}
	if opts.TrustProxyHeaders, err = envBool("HERDR_GATEWAY_TRUSTED_PROXY"); err != nil {
		return opts, err
	}
	return opts, nil
}

func newLogger(format string) (*slog.Logger, error) {
	options := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, options)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, options)), nil
	default:
		return nil, fmt.Errorf("gateway: HERDR_GATEWAY_LOG_FORMAT must be text or json, got %q", format)
	}
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// envInt returns zero when the variable is unset, letting gateway.Options apply
// its own default.
func envInt(key string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("gateway: %s must be an integer, got %q", key, raw)
	}
	return value, nil
}

func envInt64(key string) (int64, bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("gateway: %s must be an integer, got %q", key, raw)
	}
	return value, true, nil
}

func envBool(key string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("gateway: %s must be a boolean, got %q", key, raw)
	}
	return value, nil
}
