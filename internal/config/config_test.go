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

func TestLoadReadsSocialClientIDs(t *testing.T) {
	t.Setenv("DATABASE_URL", validURL)
	t.Setenv("JWT_SECRET", validSecret)
	// Several client ids per provider is the normal case: one per platform.
	t.Setenv("GOOGLE_CLIENT_IDS", " ios.apps.googleusercontent.com , android.apps.googleusercontent.com ")
	t.Setenv("APPLE_CLIENT_IDS", "com.tapago.app")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"ios.apps.googleusercontent.com", "android.apps.googleusercontent.com"}
	if len(cfg.GoogleClientIDs) != len(want) {
		t.Fatalf("GoogleClientIDs = %v, want %v", cfg.GoogleClientIDs, want)
	}
	for i, w := range want {
		if cfg.GoogleClientIDs[i] != w {
			t.Errorf("GoogleClientIDs[%d] = %q, want %q", i, cfg.GoogleClientIDs[i], w)
		}
	}
	if len(cfg.AppleClientIDs) != 1 || cfg.AppleClientIDs[0] != "com.tapago.app" {
		t.Errorf("AppleClientIDs = %v", cfg.AppleClientIDs)
	}
}

// Social sign-in is optional: an environment that does not configure it must
// still boot, and must not end up with a blank client id that would look
// configured to the verifier.
func TestLoadWithoutSocialClientIDs(t *testing.T) {
	for name, value := range map[string]string{"unset": "", "blank": "  ,  ,"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", validURL)
			t.Setenv("JWT_SECRET", validSecret)
			t.Setenv("GOOGLE_CLIENT_IDS", value)
			t.Setenv("APPLE_CLIENT_IDS", value)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.GoogleClientIDs) != 0 || len(cfg.AppleClientIDs) != 0 {
				t.Errorf("client ids = %v / %v, want none", cfg.GoogleClientIDs, cfg.AppleClientIDs)
			}
		})
	}
}
