package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// User is a registered account.
//
// PasswordHash is part of the domain because storage owns it, but it must
// never reach a response body: handlers build an explicit DTO with only the
// public fields rather than serialising this struct. There are deliberately
// no json tags here so that accidentally marshalling a User is a visible
// mistake in review rather than a silent leak.
type User struct {
	// ID is the primary key, a Postgres uuid rendered as its canonical
	// 36-character text form.
	ID string
	// Email is stored lower-cased and trimmed; it is unique across users.
	Email string
	// Name is the display name supplied at registration.
	Name string
	// PasswordHash is the encoded hash produced by internal/password.
	// It is never logged and never serialised.
	//
	// nil means the account has no password at all — it was created through
	// social sign-in. That is not the same as an empty hash, and the login
	// path must treat it as "cannot authenticate this way" rather than
	// feeding it to a verifier.
	PasswordHash *string
	// GoogleID and AppleID are the provider "sub" claims this account is
	// linked to, or nil when it is not linked to that provider. An account
	// can carry both.
	GoogleID *string
	AppleID  *string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// SocialIdentity is a verified assertion from Google or Apple, reduced to
// what account linking needs. Producing one is the job of internal/social;
// this package trusts it.
type SocialIdentity struct {
	// Provider is social.ProviderGoogle or social.ProviderApple.
	Provider string
	// Subject is the provider's stable user id.
	Subject string
	// Email is lower-cased, or empty when the token carried none. It must
	// only be set when the provider says the address is verified — matching
	// an existing account on an unverified address hands that account to
	// whoever controls the provider profile.
	Email string
	// Name is a display name, or empty.
	Name string
}

// Errors returned by UpsertSocialUser.
var (
	// ErrUnknownProvider means the identity names a provider this package
	// has no column for. It is a programming error, not user input.
	ErrUnknownProvider = errors.New("model: unknown social provider")
	// ErrMissingSubject means the identity has no provider user id, so there
	// is nothing stable to link the account on.
	ErrMissingSubject = errors.New("model: social identity has no subject")
	// ErrEmailRequired means a new account would have to be created but the
	// identity carries no usable email. Every users row needs one.
	ErrEmailRequired = errors.New("model: cannot create an account without a verified email")
	// ErrProviderConflict means the email already belongs to an account
	// linked to a *different* account at the same provider. Silently
	// re-pointing the link would let the second provider account take over
	// the first one's user.
	ErrProviderConflict = errors.New("model: email is already linked to a different provider account")
)

// uniqueViolation is the SQLSTATE Postgres raises for a duplicate key.
const uniqueViolation = "23505"

// Querier is the slice of a pgx pool this package uses. Taking an interface
// keeps the model free of a concrete pool type and lets callers test the
// upsert without a live database.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// socialColumns maps a provider to the column its subject is stored in.
//
// The column name is interpolated into the SQL below. That is safe — and the
// only way to write it without duplicating both statements per provider —
// precisely because the value never comes from a request: it is looked up in
// this fixed map, and an unknown provider is refused before any SQL is built.
// User-supplied values still go through placeholders, always.
var socialColumns = map[string]string{
	ProviderGoogle: "google_id",
	ProviderApple:  "apple_id",
}

// Provider names. They are spelled the same as social.ProviderGoogle and
// social.ProviderApple; they are duplicated rather than imported so the model
// keeps its one-way dependency direction and does not pull an HTTP-facing
// package in behind it.
const (
	ProviderGoogle = "google"
	ProviderApple  = "apple"
)

const returningColumns = `id::text, email, name, password_hash, google_id, apple_id, created_at, updated_at`

// selectBySocialSQL finds the account already linked to a provider subject.
var selectBySocialSQL = buildPerProvider(`
SELECT ` + returningColumns + `
FROM users
WHERE %[1]s = $1`)

// upsertByEmailSQL creates the account or links an existing one.
//
// The ON CONFLICT arm is what satisfies "an account created with
// email/password can be linked to a social provider": the row already exists,
// so the insert becomes an update that fills in the provider id. COALESCE
// keeps any id already stored — the caller compares what comes back with what
// it asked for and treats a mismatch as a conflict, so a second Google
// account can never steal a user by presenting the same address.
//
// The name is only filled in when the row has none; a display name the user
// set here must not be overwritten by whatever the provider currently holds.
var upsertByEmailSQL = buildPerProvider(`
INSERT INTO users (email, name, %[1]s)
VALUES ($2, $3, $1)
ON CONFLICT (email) DO UPDATE
SET %[1]s    = COALESCE(users.%[1]s, EXCLUDED.%[1]s),
    name       = CASE WHEN users.name = '' THEN EXCLUDED.name ELSE users.name END,
    updated_at = now()
RETURNING ` + returningColumns)

func buildPerProvider(template string) map[string]string {
	out := make(map[string]string, len(socialColumns))
	for provider, column := range socialColumns {
		out[provider] = fmt.Sprintf(template, column)
	}
	return out
}

// UpsertSocialUser returns the account behind a verified social identity,
// creating or linking it as needed.
//
// The resolution order is deliberate:
//
//  1. Already linked to this provider subject? Return it. This is the steady
//     state — every sign-in after the first — and it must not depend on the
//     email, which a user can change at the provider.
//  2. Otherwise create the account, or link the existing one with the same
//     email. Matching on email is what merges a social sign-in with an
//     account that was registered with a password.
//
// The caller must only pass an Email the provider reported as verified.
func UpsertSocialUser(ctx context.Context, db Querier, identity SocialIdentity) (*User, error) {
	column, ok := socialColumns[identity.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, identity.Provider)
	}
	subject := strings.TrimSpace(identity.Subject)
	if subject == "" {
		return nil, ErrMissingSubject
	}
	email := strings.ToLower(strings.TrimSpace(identity.Email))

	user, err := scanUser(db.QueryRow(ctx, selectBySocialSQL[identity.Provider], subject))
	switch {
	case err == nil:
		return user, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("model: select user by %s: %w", column, err)
	}

	if email == "" {
		return nil, ErrEmailRequired
	}

	user, err = scanUser(db.QueryRow(ctx, upsertByEmailSQL[identity.Provider], subject, email, displayName(identity.Name, email)))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			// The provider-id index fired, not the email one: a concurrent
			// first sign-in for the same social account won the race. The
			// row it wrote is the answer, so read it back rather than
			// reporting a conflict the user cannot act on.
			user, retryErr := scanUser(db.QueryRow(ctx, selectBySocialSQL[identity.Provider], subject))
			if retryErr == nil {
				return user, nil
			}
			return nil, fmt.Errorf("model: upsert social user: %w", err)
		}
		return nil, fmt.Errorf("model: upsert social user: %w", err)
	}

	// COALESCE above preserves an existing link, so a value other than ours
	// means this email belongs to someone else's provider account.
	if linked := user.providerID(identity.Provider); linked == nil || *linked != subject {
		return nil, ErrProviderConflict
	}

	return user, nil
}

func (u *User) providerID(provider string) *string {
	switch provider {
	case ProviderGoogle:
		return u.GoogleID
	case ProviderApple:
		return u.AppleID
	default:
		return nil
	}
}

// displayName falls back to the local part of the email when the provider
// sends no name. Apple never puts a name in its ID token (it is delivered
// once, in the authorization response), so without this every Apple sign-up
// would land with a blank display name.
func displayName(name, email string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" {
		return local
	}
	return email
}

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.GoogleID, &u.AppleID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}
