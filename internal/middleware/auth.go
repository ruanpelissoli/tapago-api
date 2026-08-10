package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/tapago/tapago-api/internal/handler"
)

// bearerScheme is the only Authorization scheme this API accepts. RFC 7235
// makes the scheme name case-insensitive, so it is compared with EqualFold.
const bearerScheme = "bearer"

// unauthorizedMessage is returned for every rejection reason — absent
// header, wrong scheme, bad signature, expired token. Telling a caller
// *why* its token failed only helps someone probing the endpoint.
const unauthorizedMessage = "unauthorized"

// ctxKey is an unexported type so no other package can collide with (or
// forge) the value stored under it. A plain string key would be reachable
// from anywhere.
type ctxKey struct{}

var userIDKey ctxKey

// TokenVerifier turns a raw bearer token into the user id it identifies.
// It is the narrow half of *token.Issuer that this package needs; taking an
// interface keeps middleware tests free of real signing keys.
type TokenVerifier interface {
	// Subject returns the token's subject claim, or an error if the token
	// is missing, malformed, unsigned, forged, or expired.
	Subject(raw string) (string, error)
}

// RequireAuth rejects any request without a valid bearer token and puts the
// authenticated user id on the request context for downstream handlers.
//
// It is deliberately a route-group middleware rather than a global one:
// /health and the auth endpoints themselves must stay reachable without a
// token, and an allow-list of public paths inside a global middleware is the
// kind of thing that silently grows a hole.
func RequireAuth(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w)
				return
			}

			userID, err := verifier.Subject(raw)
			if err != nil || userID == "" {
				unauthorized(w)
				return
			}

			ctx := context.WithValue(r.Context(), userIDKey, userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserID returns the authenticated user id placed on the context by
// RequireAuth. The boolean is false on any request that did not pass
// through it, so a handler can never mistake "no auth" for "empty user".
func UserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userIDKey).(string)
	return id, ok && id != ""
}

// bearerToken extracts the credentials from an Authorization header value.
//
// Exactly two space-separated fields are required: a header such as
// "Bearer a b" is rejected rather than having its tail silently dropped.
func bearerToken(header string) (string, bool) {
	scheme, credentials, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found {
		return "", false
	}
	if !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}

	credentials = strings.TrimSpace(credentials)
	if credentials == "" || strings.ContainsAny(credentials, " \t") {
		return "", false
	}

	return credentials, true
}

func unauthorized(w http.ResponseWriter) {
	// RFC 7235 requires a challenge on a 401.
	w.Header().Set("WWW-Authenticate", "Bearer")
	handler.Error(w, http.StatusUnauthorized, unauthorizedMessage)
}
