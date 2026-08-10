// Command api is the tapago HTTP API server entry point.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tapago/tapago-api/internal/config"
	"github.com/tapago/tapago-api/internal/db"
	"github.com/tapago/tapago-api/internal/router"
	"github.com/tapago/tapago-api/internal/token"
)

const (
	// Startup must not hang forever on an unreachable database host.
	dbConnectTimeout = 10 * time.Second
	// Time allowed for in-flight requests to finish on shutdown.
	shutdownTimeout = 15 * time.Second

	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// run returns an error instead of calling os.Exit directly so that
	// deferred cleanup (closing the pool) actually runs.
	if err := run(); err != nil {
		slog.Error("server startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Built before the database is touched: a bad signing secret is a
	// configuration error, and there is no reason to open a connection pool
	// only to fail on it a moment later.
	tokens, err := token.New(cfg.JWTSecret)
	if err != nil {
		return err
	}

	// Cancelled on SIGINT/SIGTERM; this is the signal to start draining.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(ctx, dbConnectTimeout)
	defer cancel()

	pool, err := db.Connect(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	slog.Info("connected to database")

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           router.New(router.Deps{DB: pool, Tokens: tokens}),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	}

	// Detached from ctx: ctx is already cancelled by the signal, so reusing
	// it would abort in-flight requests immediately.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	slog.Info("server stopped cleanly")
	return nil
}
