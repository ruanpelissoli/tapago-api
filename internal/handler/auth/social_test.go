package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	authhandler "github.com/tapago/tapago-api/internal/handler/auth"
	"github.com/tapago/tapago-api/internal/model"
	"github.com/tapago/tapago-api/internal/social"
)

// These tests cover the social sign-in handlers end to end within the
// process: a stub verifier stands in for Google/Apple, and a stub row source
// stands in for Postgres. The point is the mapping from what the provider
// and the database say to what the client sees — especially that only a
// genuinely rejected token produces a 401.

type stubVerifier struct {
	identity social.Identity
	err      error
	calls    int
}

func (s *stubVerifier) Verify(_ context.Context, _ string) (social.Identity, error) {
	s.calls++
	if s.err != nil {
		return social.Identity{}, s.err
	}
	return s.identity, nil
}

type stubRow struct {
	user *model.User
	err  error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}

	values := []any{
		r.user.ID, r.user.Email, r.user.Name,
		r.user.PasswordHash, r.user.GoogleID, r.user.AppleID,
		r.user.CreatedAt, r.user.UpdatedAt,
	}
	if len(dest) != len(values) {
		return fmt.Errorf("scan: %d destinations, want %d", len(dest), len(values))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = values[i].(string)
		case **string:
			*d, _ = values[i].(*string)
		case *time.Time:
			*d = values[i].(time.Time)
		default:
			return fmt.Errorf("scan: unsupported destination type %T", d)
		}
	}
	return nil
}

type stubDB struct {
	answers []stubRow
	calls   int
}

func (s *stubDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	s.calls++
	if s.calls > len(s.answers) {
		return stubRow{err: fmt.Errorf("unexpected query %d: %s", s.calls, sql)}
	}
	return s.answers[s.calls-1]
}

type stubIssuer struct {
	token string
	err   error
}

func (s stubIssuer) Issue(string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.token == "" {
		return "issued-token", nil
	}
	return s.token, nil
}

func ptr(s string) *string { return &s }

func linkedUser(google, apple *string) stubRow {
	return stubRow{user: &model.User{
		ID:       "11111111-1111-4111-8111-111111111111",
		Email:    "person@example.com",
		Name:     "A Person",
		GoogleID: google,
		AppleID:  apple,
	}}
}

func post(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/auth/google", strings.NewReader(body)))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body
}

func googleIdentity() social.Identity {
	return social.Identity{
		Provider:      social.ProviderGoogle,
		Subject:       "google-sub-1",
		Email:         "person@example.com",
		EmailVerified: true,
		Name:          "A Person",
	}
}

func TestGoogleReturnsTokenAndUser(t *testing.T) {
	db := &stubDB{answers: []stubRow{linkedUser(ptr("google-sub-1"), nil)}}
	verifier := &stubVerifier{identity: googleIdentity()}
	h := authhandler.NewHandler(db, stubIssuer{token: "api-token"}, authhandler.SocialVerifiers{Google: verifier})

	rec := post(t, h.Google, `{"id_token":"whatever"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if verifier.calls != 1 {
		t.Errorf("verifier calls = %d, want 1", verifier.calls)
	}

	body := decode(t, rec)
	if body["token"] != "api-token" {
		t.Errorf("token = %v, want the issued API token", body["token"])
	}

	user, ok := body["user"].(map[string]any)
	if !ok {
		t.Fatalf("user = %v, want an object", body["user"])
	}
	if user["email"] != "person@example.com" || user["name"] != "A Person" {
		t.Errorf("user = %v", user)
	}
	// The envelope must be identical to the email/password one — no password
	// field, no provider internals.
	if _, leaked := user["password_hash"]; leaked {
		t.Error("response leaked password_hash")
	}
	if _, leaked := user["google_id"]; leaked {
		t.Error("response leaked google_id")
	}
}

func TestAppleReturnsTokenAndUser(t *testing.T) {
	db := &stubDB{answers: []stubRow{linkedUser(nil, ptr("apple-sub-1"))}}
	verifier := &stubVerifier{identity: social.Identity{
		Provider:      social.ProviderApple,
		Subject:       "apple-sub-1",
		Email:         "person@example.com",
		EmailVerified: true,
	}}
	h := authhandler.NewHandler(db, stubIssuer{}, authhandler.SocialVerifiers{Apple: verifier})

	rec := httptest.NewRecorder()
	h.Apple(rec, httptest.NewRequest(http.MethodPost, "/auth/apple", strings.NewReader(`{"id_token":"whatever"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if decode(t, rec)["token"] != "issued-token" {
		t.Errorf("body = %s, want a token", rec.Body)
	}
}

// The pinned acceptance criterion: a rejected token is a 401 carrying exactly
// this message, and nothing about why.
func TestSocialLoginRejectsInvalidToken(t *testing.T) {
	db := &stubDB{}
	h := authhandler.NewHandler(db, stubIssuer{}, authhandler.SocialVerifiers{
		Google: &stubVerifier{err: fmt.Errorf("%w: expired", social.ErrInvalidToken)},
	})

	rec := post(t, h.Google, `{"id_token":"expired-token"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := decode(t, rec)["error"]; got != "invalid social token" {
		t.Errorf("error = %q, want %q", got, "invalid social token")
	}
	if db.calls != 0 {
		t.Errorf("database queries = %d, want 0: a rejected token must not reach the database", db.calls)
	}
}

func TestSocialLoginRejectsMalformedRequests(t *testing.T) {
	cases := map[string]struct {
		body string
		want int
	}{
		"not json":         {body: "not json", want: http.StatusBadRequest},
		"no id_token":      {body: `{}`, want: http.StatusBadRequest},
		"blank id_token":   {body: `{"id_token":"   "}`, want: http.StatusBadRequest},
		"oversized token":  {body: `{"id_token":"` + strings.Repeat("a", 9000) + `"}`, want: http.StatusUnauthorized},
		"wrong json shape": {body: `{"id_token":123}`, want: http.StatusBadRequest},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			verifier := &stubVerifier{identity: googleIdentity()}
			h := authhandler.NewHandler(&stubDB{}, stubIssuer{}, authhandler.SocialVerifiers{Google: verifier})

			rec := post(t, h.Google, tc.body)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if verifier.calls != 0 {
				t.Errorf("verifier calls = %d, want 0: malformed input must not reach the provider", verifier.calls)
			}
		})
	}
}

// An unconfigured provider is our problem, not a bad credential: answering
// 401 would send the app into a re-authentication loop it cannot escape.
func TestSocialLoginWithoutVerifierIsUnavailable(t *testing.T) {
	h := authhandler.NewHandler(&stubDB{}, stubIssuer{}, authhandler.SocialVerifiers{})

	for name, call := range map[string]http.HandlerFunc{"google": h.Google, "apple": h.Apple} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, call, `{"id_token":"whatever"}`)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
		})
	}
}

// Same reasoning when the provider's key endpoint is unreachable: we do not
// know that the token is bad, so we must not say that it is.
func TestSocialLoginWhenProviderKeysAreUnavailable(t *testing.T) {
	h := authhandler.NewHandler(&stubDB{}, stubIssuer{}, authhandler.SocialVerifiers{
		Google: &stubVerifier{err: fmt.Errorf("%w: dial tcp: timeout", social.ErrKeysUnavailable)},
	})

	rec := post(t, h.Google, `{"id_token":"whatever"}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := decode(t, rec)["error"]; got == "invalid social token" {
		t.Error("a provider outage must not be reported as an invalid token")
	}
}

// The account-takeover guard: an unverified address must never be used to
// match an existing account. With no prior link, that leaves nothing to sign
// in as.
func TestSocialLoginIgnoresUnverifiedEmail(t *testing.T) {
	identity := googleIdentity()
	identity.EmailVerified = false

	db := &stubDB{answers: []stubRow{{err: pgx.ErrNoRows}}}
	h := authhandler.NewHandler(db, stubIssuer{}, authhandler.SocialVerifiers{
		Google: &stubVerifier{identity: identity},
	})

	rec := post(t, h.Google, `{"id_token":"whatever"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// One query — the lookup by google_id. Reaching the email upsert would
	// mean the unverified address was trusted.
	if db.calls != 1 {
		t.Errorf("database queries = %d, want 1", db.calls)
	}
}

// An unverified address on an *already linked* account is harmless: the link
// resolves on the provider subject, and the stored email is untouched.
func TestSocialLoginAllowsUnverifiedEmailForLinkedAccount(t *testing.T) {
	identity := googleIdentity()
	identity.EmailVerified = false

	db := &stubDB{answers: []stubRow{linkedUser(ptr("google-sub-1"), nil)}}
	h := authhandler.NewHandler(db, stubIssuer{}, authhandler.SocialVerifiers{
		Google: &stubVerifier{identity: identity},
	})

	if rec := post(t, h.Google, `{"id_token":"whatever"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestSocialLoginReportsProviderConflict(t *testing.T) {
	db := &stubDB{answers: []stubRow{
		{err: pgx.ErrNoRows},
		linkedUser(ptr("a-different-google-sub"), nil),
	}}
	h := authhandler.NewHandler(db, stubIssuer{}, authhandler.SocialVerifiers{
		Google: &stubVerifier{identity: googleIdentity()},
	})

	rec := post(t, h.Google, `{"id_token":"whatever"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
}

func TestSocialLoginReportsDatabaseFailureAsInternal(t *testing.T) {
	db := &stubDB{answers: []stubRow{{err: errors.New("connection reset")}}}
	h := authhandler.NewHandler(db, stubIssuer{}, authhandler.SocialVerifiers{
		Google: &stubVerifier{identity: googleIdentity()},
	})

	rec := post(t, h.Google, `{"id_token":"whatever"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// The client must not learn anything about the database.
	if got := fmt.Sprint(decode(t, rec)["error"]); strings.Contains(got, "connection reset") {
		t.Errorf("error = %q leaks the underlying failure", got)
	}
}
