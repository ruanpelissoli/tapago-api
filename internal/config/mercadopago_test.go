package config_test

import (
	"testing"

	"github.com/tapago/tapago-api/internal/config"
)

// Lives in its own file rather than in config_test.go so the Mercado Pago
// cases sit together; the constants it needs are declared there.

func TestLoadReadsMercadoPagoAccessToken(t *testing.T) {
	t.Setenv("DATABASE_URL", validURL)
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("MERCADOPAGO_ACCESS_TOKEN", "  TEST-1234567890-abcdef  ")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Trimmed: a token with a stray newline from a copy-paste or a secret
	// manager would go out in the Authorization header and be rejected.
	if cfg.MercadoPagoAccessToken != "TEST-1234567890-abcdef" {
		t.Errorf("MercadoPagoAccessToken = %q, want the trimmed token", cfg.MercadoPagoAccessToken)
	}
}

// Optional, like the social client ids: a dev box or a test stack must boot
// without payments configured. The payment routes answer 503 while it is
// empty rather than the process refusing to start.
func TestLoadWithoutMercadoPagoAccessToken(t *testing.T) {
	for _, value := range []string{"", "   "} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", validURL)
			t.Setenv("JWT_SECRET", validSecret)
			t.Setenv("MERCADOPAGO_ACCESS_TOKEN", value)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.MercadoPagoAccessToken != "" {
				t.Errorf("MercadoPagoAccessToken = %q, want empty", cfg.MercadoPagoAccessToken)
			}
		})
	}
}
