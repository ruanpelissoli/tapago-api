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

// These tests assert the *wiring* of POST /v1/bets: that it exists, and that
// it sits inside the RequireAuth group. Deps.DB is deliberately nil — every
// case here must be rejected before any database access, and a panic would
// prove otherwise. The handler's own behaviour (the outcome table, the
// nil-Mercado-Pago 503) is covered in internal/handler/bet, where a stub
// database can stand in for the pool.
//
// authorized and tokenFrom come from the sibling wiring tests in this
// package.

func betIssuer(t *testing.T) *token.Issuer {
	t.Helper()

	issuer, err := token.New("router-bet-test-secret")
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}
	return issuer
}

// A body that fails to decode is rejected at the boundary, before the handler
// touches the pool. Reaching a 400 therefore proves the route is registered
// without needing a database.
func TestCreateBetIsMounted(t *testing.T) {
	issuer := betIssuer(t)

	rec := httptest.NewRecorder()
	req := authorized(t, httptest.NewRequest(http.MethodPost, "/v1/bets", strings.NewReader("not json")), issuer)

	router.New(router.Deps{Tokens: issuer}).ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d: route is not mounted", rec.Code)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
}

// The acceptance criterion: the route is not reachable without a valid bearer
// token, and every rejection is the same opaque 401 with a challenge.
func TestCreateBetRequiresBearerToken(t *testing.T) {
	issuer := betIssuer(t)

	for name, header := range map[string]string{
		"missing":          "",
		"blank bearer":     "Bearer ",
		"wrong scheme":     "Basic dXNlcjpwYXNz",
		"not a token":      "Bearer not-a-jwt",
		"signed elsewhere": "Bearer " + tokenFrom(t, "a-different-secret"),
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/bets", strings.NewReader(`{}`))
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
			// The reason must not leak: an attacker probing tokens should not
			// learn whether one was forged, expired, or malformed.
			if body["error"] != "unauthorized" {
				t.Errorf("error = %q, want a generic %q", body["error"], "unauthorized")
			}
		})
	}
}

// Listing and cancelling bets are out of scope for this milestone, so only
// POST is mounted on this path.
func TestBetRouteRejectsWrongMethod(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/bets", nil)

			router.New(router.Deps{Tokens: betIssuer(t)}).ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body)
			}
		})
	}
}
