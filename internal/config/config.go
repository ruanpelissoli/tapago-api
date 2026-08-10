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
	// GoogleClientIDs are the OAuth client ids allowed in the "aud" claim of
	// a Google ID token — usually one per mobile platform. Optional: with
	// none configured, POST /auth/google answers 503 instead of trusting
	// tokens minted for some other application.
	GoogleClientIDs []string
	// AppleClientIDs are the Services IDs / bundle identifiers allowed in the
	// "aud" claim of an Apple ID token. Optional, same failure mode.
	AppleClientIDs []string
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
		Port:            DefaultPort,
		DatabaseURL:     strings.TrimSpace(os.Getenv("DATABASE_URL")),
		JWTSecret:       strings.TrimSpace(os.Getenv("JWT_SECRET")),
		GoogleClientIDs: splitList(os.Getenv("GOOGLE_CLIENT_IDS")),
		AppleClientIDs:  splitList(os.Getenv("APPLE_CLIENT_IDS")),
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

// splitList parses a comma-separated environment variable into its non-empty
// entries. Empty input yields nil rather than a one-element slice holding "",
// which downstream would read as a configured-but-blank client id.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Addr returns the listen address for the HTTP server.
func (c Config) Addr() string {
	return ":" + strconv.Itoa(c.Port)
}
