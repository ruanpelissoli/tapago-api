// Package config loads runtime configuration from environment variables.
//
// Configuration is read once at startup and passed explicitly down the
// dependency graph — no global state, no config file library.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultPort is used when PORT is unset or empty.
const DefaultPort = 8080

// Config holds every environment-provided setting the API needs.
type Config struct {
	// Port is the TCP port the HTTP server listens on.
	Port int
	// DatabaseURL is the PostgreSQL connection string. Required.
	DatabaseURL string
	// JWTSecret signs and verifies access tokens. Required.
	JWTSecret string
}

// ErrMissingDatabaseURL is returned when DATABASE_URL is absent or blank.
var ErrMissingDatabaseURL = errors.New("DATABASE_URL is required but was not set")

// ErrMissingJWTSecret is returned when JWT_SECRET is absent or blank.
//
// This is validated at startup rather than at signing time on purpose: a
// process that boots without a secret would either mint tokens anyone can
// forge or fail every request at runtime. Both are worse than not starting.
var ErrMissingJWTSecret = errors.New("JWT_SECRET is required but was not set")

// Load reads configuration from the process environment.
//
// It returns an error rather than exiting so callers control the failure
// mode; main is responsible for logging and setting the exit code.
func Load() (Config, error) {
	cfg := Config{
		Port:        DefaultPort,
		DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		JWTSecret:   strings.TrimSpace(os.Getenv("JWT_SECRET")),
	}

	if raw := strings.TrimSpace(os.Getenv("PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("PORT %q is not a number: %w", raw, err)
		}
		if port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("PORT %d is outside the valid range 1-65535", port)
		}
		cfg.Port = port
	}

	if cfg.DatabaseURL == "" {
		return Config{}, ErrMissingDatabaseURL
	}
	if cfg.JWTSecret == "" {
		return Config{}, ErrMissingJWTSecret
	}

	return cfg, nil
}

// Addr returns the listen address for the HTTP server.
func (c Config) Addr() string {
	return ":" + strconv.Itoa(c.Port)
}
