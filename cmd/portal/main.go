// Command portal aggregates the OpenAPI specs of the services running in a
// cluster into one place.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"swaggeragg/internal/api"
	"swaggeragg/internal/collector"
	"swaggeragg/internal/config"
	"swaggeragg/internal/k8s"
	"swaggeragg/internal/store"
)

func main() {
	healthMode := flag.Bool("health-check", false,
		"probe this portal's own /healthz and exit 0/1; the container image has no shell, so HEALTHCHECK uses this")
	flag.Parse()

	if *healthMode {
		os.Exit(healthCheck())
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Postgres may still be starting when the pod is scheduled, so the first
	// connection is retried rather than treated as fatal.
	st, err := openStore(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()

	var kube *k8s.Client
	if cfg.Kubernetes.Enabled {
		kube, err = k8s.New(cfg.Kubernetes)
		if err != nil {
			return fmt.Errorf("kubernetes discovery: %w", err)
		}
		log.Info("kubernetes discovery enabled",
			"apiServer", cfg.Kubernetes.APIServer, "target", cfg.Kubernetes.Target,
			"syncInterval", cfg.Kubernetes.SyncInterval)
	} else {
		log.Warn("kubernetes discovery is off; the registry will stay empty " +
			"unless services are added another way (PORTAL_K8S_ENABLED)")
	}

	col := collector.New(cfg, st, kube, log)
	go col.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(cfg, st, col, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("portal listening", "addr", cfg.Listen, "proxy", cfg.HTTP.ProxyEnabled)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func openStore(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	const attempts = 30
	var lastErr error
	for i := 0; i < attempts; i++ {
		st, err := store.Open(ctx, dsn)
		if err == nil {
			return st, nil
		}
		lastErr = err
		log.Warn("waiting for postgres", "attempt", i+1, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("postgres unreachable after %d attempts: %w", attempts, lastErr)
}

func healthCheck() int {
	addr := os.Getenv("PORTAL_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "health-check:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health-check: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
