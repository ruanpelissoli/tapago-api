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

// These tests assert the *wiring*: that the auth routes exist, that /me sits
// behind RequireAuth, and that a request without a usable token never reaches
// a handler. They deliberately use a nil Deps.DB — every case here must be
// rejected before any database access, and a panic would prove otherwise.

func TestAuthRoutesAreMounted(t *testing.T) {
	// A body that fails to decode is rejected at the boundary, before the
	// handler touches the pool. Reaching a 400 therefore proves the route is
	// registered without needing a database.
	for _, path := range []string{"/auth/register", "/auth/login"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("not json"))

			router.New(router.Deps{}).ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
				t.Fatalf("status = %d: route is not mounted", rec.Code)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestAuthRoutesRejectWrongMethod(t *testing.T) {
	for _, path := range []string{"/auth/register", "/auth/login"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)

			router.New(router.Deps{}).ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// The core of the acceptance criterion: /me is unreachable without a valid
// bearer token. Every one of these must stop at the middleware.
func TestMeRequiresBearerToken(t *testing.T) {
	issuer, err := token.New("router-test-secret")
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}

	for name, header := range map[string]string{
		"missing":            "",
		"empty bearer":       "Bearer",
		"blank credentials":  "Bearer ",
		"wrong scheme":       "Basic dXNlcjpwYXNz",
		"not a token":        "Bearer not-a-jwt",
		"signed elsewhere":   "Bearer " + tokenFrom(t, "a-different-secret"),
		"extra header parts": "Bearer a b",
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}

			router.New(router.Deps{Tokens: issuer}).ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["error"] == "" {
				t.Errorf("body = %s, want a non-empty error field", rec.Body.String())
			}
			// The rejection reason must not leak: an attacker probing tokens
			// should not learn whether one was expired, forged, or malformed.
			if body["error"] != "unauthorized" {
				t.Errorf("error = %q, want a generic %q", body["error"], "unauthorized")
			}
		})
	}
}

// A nil issuer means auth was never configured. The route surface must not
// change, and it must fail closed rather than letting requests through.
func TestMeFailsClosedWithoutIssuer(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+tokenFrom(t, "any-secret"))

	router.New(router.Deps{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// A 401 must carry a challenge, per RFC 7235.
func TestUnauthorizedResponseCarriesChallenge(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)

	router.New(router.Deps{}).ServeHTTP(rec, req)

	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
	}
}

func tokenFrom(t *testing.T, secret string) string {
	t.Helper()

	issuer, err := token.New(secret)
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}
	raw, err := issuer.Issue("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return raw
}
