package middleware_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tapago/tapago-api/internal/middleware"
)

// stubVerifier accepts exactly one token so the tests never need a real key.
type stubVerifier struct {
	accept  string
	subject string
	calls   int
}

func (s *stubVerifier) Subject(raw string) (string, error) {
	s.calls++
	if raw != s.accept {
		return "", errors.New("invalid token")
	}
	return s.subject, nil
}

func TestRequireAuthAllowsValidToken(t *testing.T) {
	verifier := &stubVerifier{accept: "good-token", subject: "user-123"}

	var seen string
	var found bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, found = middleware.UserID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer good-token")

	middleware.RequireAuth(verifier)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !found {
		t.Fatal("UserID reported no user on an authenticated request")
	}
	if seen != "user-123" {
		t.Errorf("UserID = %q, want %q", seen, "user-123")
	}
}

// The scheme name is case-insensitive per RFC 7235.
func TestRequireAuthAcceptsAnySchemeCasing(t *testing.T) {
	for _, header := range []string{"Bearer good-token", "bearer good-token", "BEARER good-token"} {
		t.Run(header, func(t *testing.T) {
			verifier := &stubVerifier{accept: "good-token", subject: "user-123"}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			req.Header.Set("Authorization", header)

			middleware.RequireAuth(verifier)(okHandler()).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
		})
	}
}

func TestRequireAuthRejectsBadHeaders(t *testing.T) {
	headers := map[string]string{
		"missing":            "",
		"no scheme":          "good-token",
		"wrong scheme":       "Basic good-token",
		"scheme only":        "Bearer",
		"empty credentials":  "Bearer ",
		"extra field":        "Bearer good-token extra",
		"unknown token":      "Bearer some-other-token",
		"token with a space": "Bearer good token",
	}

	for name, header := range headers {
		t.Run(name, func(t *testing.T) {
			verifier := &stubVerifier{accept: "good-token", subject: "user-123"}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}

			called := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
			middleware.RequireAuth(verifier)(next).ServeHTTP(rec, req)

			if called {
				t.Fatal("the protected handler ran despite a rejected token")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if body["error"] == "" {
				t.Errorf("body = %s, want a non-empty error field", rec.Body.String())
			}
		})
	}
}

// A verifier that returns no error but an empty subject must not be treated
// as an authenticated request.
func TestRequireAuthRejectsEmptySubject(t *testing.T) {
	verifier := &stubVerifier{accept: "good-token", subject: ""}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer good-token")

	middleware.RequireAuth(verifier)(okHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestUserIDOnUnauthenticatedContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/me", nil)

	if id, ok := middleware.UserID(req.Context()); ok || id != "" {
		t.Fatalf("UserID = (%q, %v), want (\"\", false)", id, ok)
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}
