package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tapago/tapago-api/internal/handler"
	"github.com/tapago/tapago-api/internal/model"
	"github.com/tapago/tapago-api/internal/social"
)

// Client-facing messages for the social endpoints. The first is pinned by
// the acceptance criteria and clients match on it.
const (
	msgInvalidSocialToken = "invalid social token"
	msgMissingIDToken     = "id_token is required"
	msgSocialUnavailable  = "social login is unavailable"
	msgAlreadyLinked      = "email is already linked to a different account"
)

// maxIDTokenBytes bounds the token before any parsing work. Real Google and
// Apple tokens are well under 2 KB; this only stops a client from making us
// base64-decode a megabyte per request.
const maxIDTokenBytes = 8 << 10

// SocialVerifier turns a provider ID token into a verified identity. It is
// the narrow half of *social.ProviderVerifier this package needs, so the
// handler tests never touch the network.
type SocialVerifier interface {
	Verify(ctx context.Context, idToken string) (social.Identity, error)
}

// SocialVerifiers carries one verifier per supported provider. A nil member
// means the provider was not configured; its route stays mounted and fails
// closed rather than disappearing from the URL surface, so a client gets a
// clear server error instead of a 404 it would read as "wrong path".
type SocialVerifiers struct {
	Google SocialVerifier
	Apple  SocialVerifier
}

type socialRequest struct {
	IDToken string `json:"id_token"`
}

// Google exchanges a Google ID token for an API token.
//
// POST /auth/google  {"id_token": ...}
func (h *Handler) Google(w http.ResponseWriter, r *http.Request) {
	h.socialLogin(w, r, social.ProviderGoogle, h.social.Google)
}

// Apple exchanges an Apple ID token for an API token.
//
// POST /auth/apple  {"id_token": ...}
func (h *Handler) Apple(w http.ResponseWriter, r *http.Request) {
	h.socialLogin(w, r, social.ProviderApple, h.social.Apple)
}

// socialLogin is the whole flow both providers share: verify the token,
// resolve it to an account, return the same {token, user} envelope the
// email/password endpoints return. Everything provider-specific lives behind
// the verifier.
func (h *Handler) socialLogin(w http.ResponseWriter, r *http.Request, provider string, verifier SocialVerifier) {
	var req socialRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	idToken := strings.TrimSpace(req.IDToken)
	if idToken == "" {
		// An absent field is a malformed request, not a rejected credential:
		// answering 401 here would send the app into a re-authentication
		// loop over what is a client bug.
		handler.Error(w, http.StatusBadRequest, msgMissingIDToken)
		return
	}
	if len(idToken) > maxIDTokenBytes {
		handler.Error(w, http.StatusUnauthorized, msgInvalidSocialToken)
		return
	}

	// Nil verifier: the deployment has no client id for this provider, so no
	// token can be trusted. That is our misconfiguration, and reporting it as
	// a bad token would send clients chasing a problem they cannot fix.
	if verifier == nil {
		slog.Error("social login: provider is not configured", "provider", provider)
		handler.Error(w, http.StatusServiceUnavailable, msgSocialUnavailable)
		return
	}

	identity, err := verifier.Verify(r.Context(), idToken)
	if err != nil {
		if errors.Is(err, social.ErrInvalidToken) {
			// Logged at debug: a wrong or expired token is routine, and the
			// reason must not travel to the client.
			slog.Debug("social login: token rejected", "provider", provider, "error", err)
			handler.Error(w, http.StatusUnauthorized, msgInvalidSocialToken)
			return
		}
		// We could not reach the provider's keys, so we do not know whether
		// the token is good. 503 tells the client to retry; 401 would tell it
		// to throw away a credential that is probably fine.
		slog.Error("social login: verification unavailable", "provider", provider, "error", err)
		handler.Error(w, http.StatusServiceUnavailable, msgSocialUnavailable)
		return
	}

	// An unverified address is not proof of ownership: anyone who can set a
	// victim's address on a provider account would otherwise be handed the
	// matching local account by the email branch of the upsert. Dropping the
	// email leaves linking by provider subject, which is always safe.
	email := identity.Email
	if !identity.EmailVerified {
		email = ""
	}

	user, err := model.UpsertSocialUser(r.Context(), h.db, model.SocialIdentity{
		Provider: provider,
		Subject:  identity.Subject,
		Email:    email,
		Name:     identity.Name,
	})
	if err != nil {
		switch {
		case errors.Is(err, model.ErrEmailRequired):
			// First sign-in with a token carrying no usable email: there is
			// nothing to create the account from. The token itself was valid,
			// but it is not a credential we can act on, so the client gets the
			// same opaque answer.
			slog.Warn("social login: identity has no verified email",
				"provider", provider, "email_present", identity.Email != "")
			handler.Error(w, http.StatusUnauthorized, msgInvalidSocialToken)
		case errors.Is(err, model.ErrProviderConflict):
			slog.Warn("social login: email linked to another provider account", "provider", provider)
			handler.Error(w, http.StatusConflict, msgAlreadyLinked)
		default:
			slog.Error("social login: upsert user", "provider", provider, "error", err)
			handler.Error(w, http.StatusInternalServerError, msgInternal)
		}
		return
	}

	// 200 for both a new account and an existing one. The client cannot act
	// on the difference, and a 201 would leak whether the address was already
	// registered to anyone able to mint a token for it.
	h.respondWithToken(w, http.StatusOK, userResponse{
		ID:    user.ID,
		Email: user.Email,
		Name:  user.Name,
	}, provider+" login")
}
