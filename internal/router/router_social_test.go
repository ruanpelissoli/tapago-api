package router_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tapago/tapago-api/internal/router"
)

// Wiring only: the social routes exist, are public, and are POST-only. What
// they do with a token belongs to internal/handler/auth. Deps.DB is nil on
// purpose — every case here must be answered before any database access.

var socialPaths = []string{"/auth/google", "/auth/apple"}

func TestSocialRoutesAreMounted(t *testing.T) {
	for _, path := range socialPaths {
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

func TestSocialRoutesRejectWrongMethod(t *testing.T) {
	for _, path := range socialPaths {
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

// The routes must not require a bearer token — they are how a client gets
// one — and with no verifier configured they must fail closed with a server
// error rather than a rejection the client would act on.
func TestSocialRoutesArePublicAndFailClosed(t *testing.T) {
	for _, path := range socialPaths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"id_token":"anything"}`))

			router.New(router.Deps{}).ServeHTTP(rec, req)

			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("status = 401: the route must not sit behind RequireAuth")
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
			}
		})
	}
}
