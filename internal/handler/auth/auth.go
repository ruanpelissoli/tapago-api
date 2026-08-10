// Package auth implements the email/password credential exchange:
// registration, login, and the authenticated /me lookup.
//
// It handles credentials only. Enforcing authentication on other routes is
// the job of internal/middleware.RequireAuth; this package never inspects an
// Authorization header itself.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tapago/tapago-api/internal/handler"
	"github.com/tapago/tapago-api/internal/middleware"
	"github.com/tapago/tapago-api/internal/password"
)

// Client-facing messages. They are constants because the acceptance
// criteria pin the exact strings, and because a typo in an error body is
// the kind of thing clients end up matching on.
const (
	msgInvalidBody     = "request body must be valid JSON"
	msgInvalidEmail    = "a valid email address is required"
	msgInvalidName     = "name is required"
	msgInvalidPassword = "password must be between 8 and 72 characters"
	msgMissingCreds    = "email and password are required"
	msgDuplicateEmail  = "email already registered"
	msgBadCredentials  = "invalid credentials"
	msgUnauthorized    = "unauthorized"
	msgInternal        = "internal server error"
)

const (
	minPasswordLen = 8
	// bcrypt derives its key from at most 72 bytes and returns an error above
	// that, so this ceiling is the algorithm's limit rather than a policy
	// choice. Validating it here turns what would otherwise surface as a 500
	// out of password.Hash into a 400 the client can act on.
	maxPasswordLen = password.MaxLength
	maxEmailLen    = 254 // RFC 5321 section 4.5.3.1.3
	maxNameLen     = 200
	maxBodyBytes   = 64 << 10
)

// uniqueViolation is the SQLSTATE Postgres raises for a duplicate key.
//
// The race between "does this email exist?" and "insert it" cannot be
// closed by checking first, so the unique index is the source of truth and
// this is how we read its verdict.
const uniqueViolation = "23505"

// DB is the slice of the pgx pool these handlers use. Depending on an
// interface rather than *pgxpool.Pool is what lets the tests run without a
// live database.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TokenIssuer mints an access token for a user id.
type TokenIssuer interface {
	Issue(subject string) (string, error)
}

// Handler holds the dependencies of the auth endpoints.
type Handler struct {
	db     DB
	tokens TokenIssuer
	social SocialVerifiers
}

// NewHandler wires an auth handler.
//
// A zero SocialVerifiers is valid: the social routes then fail closed with a
// 503 rather than accepting tokens nobody has configured an audience for.
func NewHandler(db DB, tokens TokenIssuer, verifiers SocialVerifiers) *Handler {
	return &Handler{db: db, tokens: tokens, social: verifiers}
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// userResponse is the only shape a user is ever serialised in. It has no
// password field at all, which is a stronger guarantee than `json:"-"`:
// there is nothing to accidentally un-hide.
type userResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type authResponse struct {
	Token string       `json:"token"`
	User  userResponse `json:"user"`
}

const insertUserSQL = `
INSERT INTO users (email, password_hash, name)
VALUES ($1, $2, $3)
RETURNING id::text, email, name`

const selectUserByEmailSQL = `
SELECT id::text, email, name, password_hash
FROM users
WHERE email = $1`

const selectUserByIDSQL = `
SELECT id::text, email, name
FROM users
WHERE id = $1::uuid`

// Register creates an account and returns a token for it.
//
// POST /auth/register  {"email": ..., "password": ..., "name": ...}
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	email, ok := normaliseEmail(req.Email)
	if !ok {
		handler.Error(w, http.StatusBadRequest, msgInvalidEmail)
		return
	}
	if len(req.Password) < minPasswordLen || len(req.Password) > maxPasswordLen {
		handler.Error(w, http.StatusBadRequest, msgInvalidPassword)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxNameLen {
		handler.Error(w, http.StatusBadRequest, msgInvalidName)
		return
	}

	hash, err := password.Hash(req.Password)
	if err != nil {
		slog.Error("register: hash password", "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	var user userResponse
	err = h.db.QueryRow(r.Context(), insertUserSQL, email, hash, name).
		Scan(&user.ID, &user.Email, &user.Name)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			handler.Error(w, http.StatusConflict, msgDuplicateEmail)
			return
		}
		slog.Error("register: insert user", "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	h.respondWithToken(w, http.StatusCreated, user, "register")
}

// Login exchanges credentials for a token.
//
// POST /auth/login  {"email": ..., "password": ...}
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || req.Password == "" {
		handler.Error(w, http.StatusBadRequest, msgMissingCreds)
		return
	}
	// An over-long password cannot match anything we stored, and hashing it
	// would be free CPU for an attacker. Answer as a failed credential
	// rather than a bad request so login stays a single opaque outcome.
	if len(email) > maxEmailLen || len(req.Password) > maxPasswordLen {
		handler.Error(w, http.StatusUnauthorized, msgBadCredentials)
		return
	}

	var user userResponse
	// A pointer, not a string: password_hash is NULL for an account created
	// through social sign-in, and scanning that into a string errors out as
	// a 500 instead of the 401 it should be.
	var storedHash *string
	err := h.db.QueryRow(r.Context(), selectUserByEmailSQL, email).
		Scan(&user.ID, &user.Email, &user.Name, &storedHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Spend the same CPU a real verification would. Returning early
		// here turns login into a fast account-enumeration oracle.
		password.VerifyDummy(req.Password)
		handler.Error(w, http.StatusUnauthorized, msgBadCredentials)
		return
	case err != nil:
		slog.Error("login: select user", "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	if storedHash == nil {
		// A social-only account: there is no password to be right. Spend the
		// same CPU a real verification would so that "this address exists but
		// signs in with Google" is not observable from response timing.
		password.VerifyDummy(req.Password)
		handler.Error(w, http.StatusUnauthorized, msgBadCredentials)
		return
	}

	if err := password.Verify(*storedHash, req.Password); err != nil {
		if errors.Is(err, password.ErrMalformedHash) {
			// A corrupt row, not a wrong password. The client still gets
			// the generic answer, but this has to be visible to us.
			slog.Error("login: stored password hash is unreadable", "user_id", user.ID)
		}
		handler.Error(w, http.StatusUnauthorized, msgBadCredentials)
		return
	}

	h.respondWithToken(w, http.StatusOK, user, "login")
}

// Me returns the authenticated user.
//
// It exists to prove the bearer middleware end to end: reaching this
// handler at all means the token was valid and its subject made it onto the
// request context.
//
// GET /me
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.UserID(r.Context())
	if !ok {
		// Only reachable if the route is mounted outside RequireAuth.
		handler.Error(w, http.StatusUnauthorized, msgUnauthorized)
		return
	}
	// The subject is signed by us, but it still arrives as a client-supplied
	// string. Checking the shape here keeps a malformed value from reaching
	// Postgres as an invalid uuid cast (SQLSTATE 22P02 -> a 500).
	if !isUUID(userID) {
		handler.Error(w, http.StatusUnauthorized, msgUnauthorized)
		return
	}

	var user userResponse
	err := h.db.QueryRow(r.Context(), selectUserByIDSQL, userID).
		Scan(&user.ID, &user.Email, &user.Name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A valid token for a user that no longer exists: the credential is
		// unusable, so 401 rather than 404.
		handler.Error(w, http.StatusUnauthorized, msgUnauthorized)
		return
	case err != nil:
		slog.Error("me: select user", "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	handler.JSON(w, http.StatusOK, user)
}

func (h *Handler) respondWithToken(w http.ResponseWriter, status int, user userResponse, op string) {
	accessToken, err := h.tokens.Issue(user.ID)
	if err != nil {
		slog.Error(op+": issue token", "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	handler.JSON(w, status, authResponse{Token: accessToken, User: user})
}

// decodeJSON reads the request body into dst, writing a 400 and returning
// false if it cannot. The body is capped so a large upload cannot exhaust
// memory before validation ever runs.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		// A decode error can quote the body, which holds the password. Only
		// the fact that decoding failed is safe to record.
		slog.Debug("auth: decode request body failed")
		handler.Error(w, http.StatusBadRequest, msgInvalidBody)
		return false
	}
	return true
}

// normaliseEmail lower-cases and trims an address and checks that it parses.
//
// Storing the normalised form is what makes the unique index behave the way
// users expect: Foo@Example.com and foo@example.com are one account.
func normaliseEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > maxEmailLen {
		return "", false
	}

	addr, err := mail.ParseAddress(email)
	// Reject anything carrying a display name ("Foo <a@b.com>"): the stored
	// address must be exactly what the user will type at login.
	if err != nil || addr.Name != "" || addr.Address != email {
		return "", false
	}

	return email, true
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID.
//
// Hand-rolled rather than pulling in a UUID module: nothing here needs to
// parse, generate, or compare UUIDs — Postgres does all of that. This is a
// syntax gate, not a parser.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
