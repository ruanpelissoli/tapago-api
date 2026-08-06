// Package db owns the PostgreSQL connection pool.
//
// Queries elsewhere in the codebase are written as raw SQL against the pool
// returned by Connect. There is deliberately no ORM: explicit SQL keeps the
// generated queries reviewable and the hot paths predictable.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrEmptyDatabaseURL is returned by Connect when given a blank URL.
var ErrEmptyDatabaseURL = errors.New("db: database URL is empty")

// Pool tuning defaults. These are conservative values suited to a single
// API instance; revisit them once we have real traffic numbers.
const (
	defaultMaxConns          = 10
	defaultMinConns          = 2
	defaultMaxConnLifetime   = time.Hour
	defaultMaxConnIdleTime   = 30 * time.Minute
	defaultHealthCheckPeriod = time.Minute
)

// Connect opens a pgx connection pool and verifies it with a ping.
//
// The pool is lazy by default, so the ping is what actually surfaces bad
// credentials or an unreachable host at startup instead of on the first
// request. The caller owns the returned pool and must Close it.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, ErrEmptyDatabaseURL
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Never wrap the URL itself into the error — it carries credentials.
		return nil, fmt.Errorf("db: invalid database URL: %w", err)
	}

	cfg.MaxConns = defaultMaxConns
	cfg.MinConns = defaultMinConns
	cfg.MaxConnLifetime = defaultMaxConnLifetime
	cfg.MaxConnIdleTime = defaultMaxConnIdleTime
	cfg.HealthCheckPeriod = defaultHealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping database: %w", err)
	}

	return pool, nil
}
