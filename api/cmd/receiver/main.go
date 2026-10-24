package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/relayforge/relayforge/api/internal/db"
	"github.com/relayforge/relayforge/api/internal/receiver"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		target := "http://127.0.0.1:8090/healthz"
		if len(os.Args) > 2 {
			target = os.Args[2]
		}
		os.Exit(runHealthcheck(target))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	addr := strings.TrimSpace(os.Getenv("RELAYFORGE_RECEIVER_ADDR"))
	if addr == "" {
		addr = ":8090"
	}

	var secrets []string
	if raw := strings.TrimSpace(os.Getenv("RELAYFORGE_RECEIVER_SECRETS")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(part); s != "" {
				secrets = append(secrets, s)
			}
		}
	}

	var pool *pgxpool.Pool
	dsn := strings.TrimSpace(os.Getenv("RELAYFORGE_RECEIVER_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("RELAYFORGE_DATABASE_URL"))
	}
	if dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		p, err := db.Open(ctx, dsn, 5)
		cancel()
		if err != nil {
			logger.Error("connect receiver database", "error", err)
			os.Exit(1)
		}
		pool = p
		defer pool.Close()
	}

	srv, err := receiver.New(secrets, pool)
	if err != nil {
		logger.Error("init receiver", "error", err)
		os.Exit(1)
	}
	if err := srv.EnsureSchema(context.Background()); err != nil {
		logger.Error("ensure receiver schema", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      35 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("starting demo receiver", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("receiver listen failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	logger.Info("demo receiver stopped")
}

func runHealthcheck(target string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
