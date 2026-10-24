// Command api runs the RelayForge control plane.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/relayforge/relayforge/api/internal/config"
	"github.com/relayforge/relayforge/api/internal/db"
	"github.com/relayforge/relayforge/api/internal/httpapi"
	"github.com/relayforge/relayforge/api/internal/jsonschema"
	"github.com/relayforge/relayforge/api/internal/observability"
	"github.com/relayforge/relayforge/api/internal/secrets"
	"github.com/relayforge/relayforge/api/internal/ssrf"
	"github.com/relayforge/relayforge/api/internal/store"
	"github.com/relayforge/relayforge/api/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("starting",
		"service", "relayforge-api",
		"version", version.Version,
		"commit", version.Commit,
		"environment", cfg.Env,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := observability.SetupTracing(ctx, cfg.OTLPEndpoint, cfg.Env, version.Version)
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(tctx); err != nil {
			logger.Warn("tracing_shutdown_failed", "error", err)
		}
	}()

	pool, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	readinessStore := &db.Store{Pool: pool, ExpectedSchemaVersion: cfg.ExpectedSchemaVersion}
	metrics := observability.NewMetrics(version.Version, version.Commit)

	box, err := secrets.New(cfg.KEK, cfg.KEKVersion)
	if err != nil {
		return fmt.Errorf("initialise secret encryption: %w", err)
	}
	control := &store.Store{
		Pool:                  pool,
		ExpectedSchemaVersion: cfg.ExpectedSchemaVersion,
		Box:                   box,
		EndpointLimit:         cfg.EndpointLimit,
		IdempotencyTTL:        cfg.IdempotencyTTL,
		EventFutureSkew:       cfg.FutureSkew,
		EventPastWindow:       cfg.OccurredPast,
	}

	ssrfValidator := ssrf.New()
	if cfg.Env != "production" && strings.EqualFold(strings.TrimSpace(os.Getenv("RELAYFORGE_ALLOW_INSECURE_LOCAL_DELIVERY")), "true") {
		ssrfValidator.AllowLocalReceiver = true
	}

	deps := httpapi.Deps{
		Config: cfg, Checker: readinessStore, Metrics: metrics, Logger: logger,
		Store: control, Schemas: jsonschema.New(), SSRF: ssrfValidator,
	}
	publicSrv := newServer(cfg.HTTPAddr, httpapi.NewRouter(deps), 30*time.Second)
	adminSrv := newServer(cfg.AdminAddr, httpapi.NewAdminRouter(deps), 15*time.Second)

	errCh := make(chan error, 2)
	go serve(publicSrv, "public", logger, errCh)
	go serve(adminSrv, "admin", logger, errCh)
	if cfg.RetentionEnabled && cfg.RetentionInterval > 0 {
		go runRetentionLoop(ctx, control, metrics, logger, cfg.RetentionInterval, cfg.RetentionEvents)
	}

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown_requested")
	case err := <-errCh:
		runErr = err
		logger.Error("listener_failed", "error", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	var shutdownErr error
	for name, srv := range map[string]*http.Server{"public": publicSrv, "admin": adminSrv} {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown %s listener: %w", name, err))
		}
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	logger.Info("stopped")
	return runErr
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func newServer(addr string, handler http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func serve(srv *http.Server, name string, logger *slog.Logger, errCh chan<- error) {
	logger.Info("listening", "listener", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("%s listener on %s: %w", name, srv.Addr, err)
	}
}

func runRetentionLoop(
	ctx context.Context,
	st *store.Store,
	metrics *observability.Metrics,
	logger *slog.Logger,
	interval, eventRetention time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			summary, err := st.RunRetentionPass(pctx, eventRetention)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("retention_pass_failed", "error", err)
				}
				continue
			}
			if metrics != nil {
				if summary.IdempotencyDeleted > 0 {
					metrics.RetentionDeleted.WithLabelValues("idempotency_records").Add(float64(summary.IdempotencyDeleted))
				}
				if summary.RateLimitsDeleted > 0 {
					metrics.RetentionDeleted.WithLabelValues("rate_limit_counters").Add(float64(summary.RateLimitsDeleted))
				}
				if summary.EventsDeleted > 0 {
					metrics.RetentionDeleted.WithLabelValues("events").Add(float64(summary.EventsDeleted))
				}
			}
		}
	}
}

// healthcheck is used by the container runtime, which has no shell or curl.
// Usage: api healthcheck http://127.0.0.1:8080/healthz
func healthcheck(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: api healthcheck <url>")
		return 2
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
