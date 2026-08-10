package model

import "time"

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
	PasswordHash string

	CreatedAt time.Time
	UpdatedAt time.Time
}
