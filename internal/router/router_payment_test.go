package router_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tapago/tapago-api/internal/router"
	"github.com/tapago/tapago-api/internal/token"
)

// These tests assert the *wiring* of the payment-method routes: that both
// exist, and that both sit inside the RequireAuth group. As with the auth
// wiring tests, Deps.DB is deliberately nil — every case here must be
// rejected before any database access, and a panic would prove otherwise.
// The nil-Mercado-Pago 503 is covered in internal/handler/payment, where a
// stub database can stand in for the pool.

func paymentIssuer(t *testing.T) *token.Issuer {
	t.Helper()

	issuer, err := token.New("router-payment-test-secret")
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}
	return issuer
}

func authorized(t *testing.T, req *http.Request, issuer *token.Issuer) *http.Request {
	t.Helper()

	raw, err := issuer.Issue("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	return req
}

// A body that fails to decode is rejected at the boundary, before the
// handler touches the pool. Reaching a 400 therefore proves the route is
// registered without needing a database.
func TestCreatePaymentMethodIsMounted(t *testing.T) {
	issuer := paymentIssuer(t)

	rec := httptest.NewRecorder()
	req := authorized(t, httptest.NewRequest(http.MethodPost, "/v1/payment-methods", strings.NewReader("not json")), issuer)

	router.New(router.Deps{Tokens: issuer}).ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d: route is not mounted", rec.Code)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
}

// GET has no body to reject on, so mounting is proven the other way round: a
// request without a token must stop at RequireAuth with a 401 rather than
// falling through to the 404 handler.
func TestListPaymentMethodsIsMountedBehindAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/payment-methods", nil)

	router.New(router.Deps{Tokens: paymentIssuer(t)}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
	}
}

// The acceptance criterion: neither route is reachable without a valid
// bearer token, and every rejection is the same opaque 401 with a challenge.
func TestPaymentMethodRoutesRequireBearerToken(t *testing.T) {
	issuer := paymentIssuer(t)

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for name, header := range map[string]string{
			"missing":          "",
			"blank bearer":     "Bearer ",
			"wrong scheme":     "Basic dXNlcjpwYXNz",
			"not a token":      "Bearer not-a-jwt",
			"signed elsewhere": "Bearer " + tokenFrom(t, "a-different-secret"),
		} {
			t.Run(method+"/"+name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(method, "/v1/payment-methods", strings.NewReader(`{}`))
				if header != "" {
					req.Header.Set("Authorization", header)
				}

				router.New(router.Deps{Tokens: issuer}).ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
				}

				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				// The reason must not leak: an attacker probing tokens should
				// not learn whether one was forged, expired, or malformed.
				if body["error"] != "unauthorized" {
					t.Errorf("error = %q, want a generic %q", body["error"], "unauthorized")
				}
			})
		}
	}
}

func TestPaymentMethodRoutesRejectWrongMethod(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/payment-methods", nil)

			router.New(router.Deps{Tokens: paymentIssuer(t)}).ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}
