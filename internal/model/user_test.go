package model_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tapago/tapago-api/internal/model"
)

// The upsert is pure SQL orchestration, so these tests stub the row source
// and assert on which statements ran, with which arguments, and how the
// results are interpreted. What the statements do inside Postgres is the
// migration's job, not this package's.

// row is one canned answer from the stub.
type row struct {
	user *model.User
	err  error
}

func (r row) Scan(dest ...any) error {
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

// stubDB answers QueryRow from a script, recording what it was asked.
type stubDB struct {
	answers []row
	calls   []call
}

type call struct {
	sql  string
	args []any
}

func (s *stubDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.calls = append(s.calls, call{sql: sql, args: args})
	if len(s.calls) > len(s.answers) {
		return row{err: fmt.Errorf("unexpected query %d: %s", len(s.calls), sql)}
	}
	return s.answers[len(s.calls)-1]
}

func (s *stubDB) call(t *testing.T, i int) call {
	t.Helper()
	if i >= len(s.calls) {
		t.Fatalf("expected at least %d queries, got %d", i+1, len(s.calls))
	}
	return s.calls[i]
}

func noRows() row { return row{err: pgx.ErrNoRows} }

func ptr(s string) *string { return &s }

func existing(id, email, name string, google, apple *string) row {
	return row{user: &model.User{
		ID: id, Email: email, Name: name,
		GoogleID: google, AppleID: apple,
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
	}}
}

func googleIdentity() model.SocialIdentity {
	return model.SocialIdentity{
		Provider: model.ProviderGoogle,
		Subject:  "google-sub-1",
		Email:    "person@example.com",
		Name:     "A Person",
	}
}

// The steady state: every sign-in after the first resolves on the provider
// subject alone and must not depend on the email, which the user can change
// at the provider.
func TestUpsertSocialUserFindsExistingLink(t *testing.T) {
	db := &stubDB{answers: []row{existing("user-1", "person@example.com", "A Person", ptr("google-sub-1"), nil)}}

	user, err := model.UpsertSocialUser(context.Background(), db, googleIdentity())
	if err != nil {
		t.Fatalf("UpsertSocialUser: %v", err)
	}
	if user.ID != "user-1" {
		t.Errorf("ID = %q, want user-1", user.ID)
	}
	if len(db.calls) != 1 {
		t.Fatalf("queries = %d, want 1: a linked account must not be re-upserted", len(db.calls))
	}
	if !strings.Contains(db.call(t, 0).sql, "google_id = $1") {
		t.Errorf("first query looks up the wrong column:\n%s", db.call(t, 0).sql)
	}
}

func TestUpsertSocialUserCreatesAccount(t *testing.T) {
	db := &stubDB{answers: []row{
		noRows(),
		existing("user-2", "person@example.com", "A Person", ptr("google-sub-1"), nil),
	}}

	user, err := model.UpsertSocialUser(context.Background(), db, googleIdentity())
	if err != nil {
		t.Fatalf("UpsertSocialUser: %v", err)
	}
	if user.ID != "user-2" {
		t.Errorf("ID = %q, want user-2", user.ID)
	}

	upsert := db.call(t, 1)
	if !strings.Contains(upsert.sql, "ON CONFLICT (email)") {
		t.Errorf("second query is not the email upsert:\n%s", upsert.sql)
	}
	want := []any{"google-sub-1", "person@example.com", "A Person"}
	for i, w := range want {
		if upsert.args[i] != w {
			t.Errorf("arg %d = %v, want %v", i+1, upsert.args[i], w)
		}
	}
}

// The acceptance criterion for linking: the row already exists because the
// user registered with a password, and the upsert fills in the provider id.
func TestUpsertSocialUserLinksExistingEmailAccount(t *testing.T) {
	db := &stubDB{answers: []row{
		noRows(),
		// What ON CONFLICT DO UPDATE returns: the original row, now linked.
		existing("user-3", "person@example.com", "Chosen Name", ptr("google-sub-1"), nil),
	}}

	user, err := model.UpsertSocialUser(context.Background(), db, googleIdentity())
	if err != nil {
		t.Fatalf("UpsertSocialUser: %v", err)
	}
	if user.ID != "user-3" {
		t.Errorf("ID = %q, want the pre-existing account user-3", user.ID)
	}
	if user.Name != "Chosen Name" {
		t.Errorf("Name = %q: the provider must not overwrite a name the user set", user.Name)
	}
}

// COALESCE keeps whatever link the row already had, so a second Google
// account presenting the same address gets a conflict rather than the user.
func TestUpsertSocialUserRejectsHijackingALinkedAccount(t *testing.T) {
	db := &stubDB{answers: []row{
		noRows(),
		existing("user-4", "person@example.com", "A Person", ptr("someone-elses-sub"), nil),
	}}

	_, err := model.UpsertSocialUser(context.Background(), db, googleIdentity())
	if !errors.Is(err, model.ErrProviderConflict) {
		t.Fatalf("error = %v, want ErrProviderConflict", err)
	}
}

// A concurrent first sign-in for the same social account trips the provider
// unique index. The row the other transaction wrote is the answer.
func TestUpsertSocialUserRereadsAfterUniqueViolation(t *testing.T) {
	db := &stubDB{answers: []row{
		noRows(),
		{err: &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}},
		existing("user-5", "person@example.com", "A Person", ptr("google-sub-1"), nil),
	}}

	user, err := model.UpsertSocialUser(context.Background(), db, googleIdentity())
	if err != nil {
		t.Fatalf("UpsertSocialUser: %v", err)
	}
	if user.ID != "user-5" {
		t.Errorf("ID = %q, want user-5", user.ID)
	}
}

// Apple omits the name; the display name must not end up blank.
func TestUpsertSocialUserFallsBackToEmailLocalPartForName(t *testing.T) {
	db := &stubDB{answers: []row{
		noRows(),
		existing("user-6", "someone@example.com", "someone", nil, ptr("apple-sub-1")),
	}}

	_, err := model.UpsertSocialUser(context.Background(), db, model.SocialIdentity{
		Provider: model.ProviderApple,
		Subject:  "apple-sub-1",
		Email:    "someone@example.com",
	})
	if err != nil {
		t.Fatalf("UpsertSocialUser: %v", err)
	}

	if got := db.call(t, 1).args[2]; got != "someone" {
		t.Errorf("name argument = %v, want the email local part", got)
	}
	if !strings.Contains(db.call(t, 1).sql, "apple_id") {
		t.Errorf("apple identity did not target apple_id:\n%s", db.call(t, 1).sql)
	}
}

func TestUpsertSocialUserRejectsUnusableIdentities(t *testing.T) {
	cases := map[string]struct {
		identity model.SocialIdentity
		answers  []row
		want     error
	}{
		"unknown provider": {
			identity: model.SocialIdentity{Provider: "facebook", Subject: "x", Email: "a@b.com"},
			want:     model.ErrUnknownProvider,
		},
		"no subject": {
			identity: model.SocialIdentity{Provider: model.ProviderGoogle, Subject: "  ", Email: "a@b.com"},
			want:     model.ErrMissingSubject,
		},
		// No link yet and no verified email: there is nothing to create an
		// account from, and guessing one would be worse.
		"no email on first sign-in": {
			identity: model.SocialIdentity{Provider: model.ProviderApple, Subject: "apple-sub-2"},
			answers:  []row{noRows()},
			want:     model.ErrEmailRequired,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{answers: tc.answers}

			if _, err := model.UpsertSocialUser(context.Background(), db, tc.identity); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(db.calls) != len(tc.answers) {
				t.Errorf("queries = %d, want %d", len(db.calls), len(tc.answers))
			}
		})
	}
}

// A database failure must surface as an error, never as a silently created
// second account.
func TestUpsertSocialUserPropagatesQueryErrors(t *testing.T) {
	boom := errors.New("connection reset")
	db := &stubDB{answers: []row{{err: boom}}}

	if _, err := model.UpsertSocialUser(context.Background(), db, googleIdentity()); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
}
