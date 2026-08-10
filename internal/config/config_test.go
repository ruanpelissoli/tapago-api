package config_test

import (
	"errors"
	"testing"

	"github.com/tapago/tapago-api/internal/config"
)

const (
	validURL    = "postgres://user:pass@localhost:5432/tapago"
	validSecret = "test-signing-secret"
)

// Every case sets both required variables explicitly. Relying on whatever
// the developer happens to have exported would make these tests pass or fail
// depending on the machine.
func TestLoadDefaultsPort(t *testing.T) {
	t.Setenv("DATABASE_URL", validURL)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("PORT", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != config.DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, config.DefaultPort)
	}
	if cfg.Addr() != ":8080" {
		t.Errorf("Addr() = %q, want %q", cfg.Addr(), ":8080")
	}
}

func TestLoadReadsPort(t *testing.T) {
	t.Setenv("DATABASE_URL", validURL)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("PORT", "9090")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"not-a-number", "0", "70000", "-1"} {
		t.Run(port, func(t *testing.T) {
			t.Setenv("DATABASE_URL", validURL)
			t.Setenv("JWT_SECRET", validSecret)
			t.Setenv("PORT", port)

			if _, err := config.Load(); err == nil {
				t.Fatalf("Load() with PORT=%q: expected an error", port)
			}
		})
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	for name, value := range map[string]string{"unset": "", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", value)
			t.Setenv("JWT_SECRET", validSecret)

			_, err := config.Load()
			if !errors.Is(err, config.ErrMissingDatabaseURL) {
				t.Fatalf("err = %v, want ErrMissingDatabaseURL", err)
			}
		})
	}
}

// Auth is not optional any more: booting without a signing secret would
// either mint forgeable tokens or fail every request at runtime.
func TestLoadRequiresJWTSecret(t *testing.T) {
	for name, value := range map[string]string{"unset": "", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", validURL)
			t.Setenv("JWT_SECRET", value)

			_, err := config.Load()
			if !errors.Is(err, config.ErrMissingJWTSecret) {
				t.Fatalf("err = %v, want ErrMissingJWTSecret", err)
			}
		})
	}
}

func TestLoadTrimsDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "  "+validURL+"  ")
	t.Setenv("JWT_SECRET", validSecret)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DatabaseURL != validURL {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, validURL)
	}
}
