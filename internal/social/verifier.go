package social

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// signingAlg is the only algorithm accepted from either provider. Both sign
// ID tokens with RS256; pinning it (rather than trusting the token header)
// is what blocks "alg: none" and the RS256->HS256 key-confusion attack,
// where a token is signed with the *public* key treated as an HMAC secret.
const signingAlg = "RS256"

// Provider endpoints and issuers.
const (
	googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
	appleJWKSURL  = "https://appleid.apple.com/auth/keys"
	appleIssuer   = "https://appleid.apple.com"
)

// googleIssuers holds both spellings Google has minted tokens with. The
// bare-host form predates the OIDC spec requiring a URL and is still emitted,
// so accepting only one of the two rejects real tokens.
var googleIssuers = []string{"https://accounts.google.com", "accounts.google.com"}

// clockSkew tolerates a small difference between our clock and the
// provider's when checking exp/iat. Without it, a correctly issued token can
// look "not yet valid" on a host whose clock runs a second slow.
const clockSkew = 60 * time.Second

// httpTimeout bounds a JWKS fetch. It is short on purpose: the fetch happens
// while a user waits on a sign-in request.
const httpTimeout = 10 * time.Second

// ProviderVerifier verifies ID tokens for one provider against its published
// key set. It is safe for concurrent use.
type ProviderVerifier struct {
	provider  string
	issuers   []string
	audiences []string
	keys      *keySet
	now       func() time.Time
}

// Option customises a verifier. Options exist mainly so tests can supply a
// local key server and a fixed clock instead of reaching the internet.
type Option func(*ProviderVerifier)

// WithHTTPClient overrides the client used to fetch the provider's keys.
func WithHTTPClient(c *http.Client) Option {
	return func(v *ProviderVerifier) {
		if c != nil {
			v.keys.client = c
		}
	}
}

// WithClock overrides time.Now for claim validation and key-cache expiry.
func WithClock(now func() time.Time) Option {
	return func(v *ProviderVerifier) {
		if now != nil {
			v.now = now
			v.keys.now = now
		}
	}
}

// WithJWKSURL points the verifier at a different key endpoint. This exists
// for tests; production code must use the provider defaults.
func WithJWKSURL(url string) Option {
	return func(v *ProviderVerifier) {
		if url != "" {
			v.keys.url = url
		}
	}
}

// WithKeyTTL overrides how long a fetched key set is trusted.
func WithKeyTTL(d time.Duration) Option {
	return func(v *ProviderVerifier) {
		if d > 0 {
			v.keys.ttl = d
		}
	}
}

// NewGoogle builds a verifier for Google ID tokens.
//
// audiences are the OAuth client ids of the apps allowed to sign in — a
// mobile app typically has one per platform, and Google's ID tokens carry
// the client id that requested them in "aud".
func NewGoogle(audiences []string, opts ...Option) (*ProviderVerifier, error) {
	return newVerifier(ProviderGoogle, googleIssuers, googleJWKSURL, audiences, opts...)
}

// NewApple builds a verifier for Apple ID tokens.
//
// audiences are the Services IDs / bundle identifiers of the apps allowed to
// sign in.
func NewApple(audiences []string, opts ...Option) (*ProviderVerifier, error) {
	return newVerifier(ProviderApple, []string{appleIssuer}, appleJWKSURL, audiences, opts...)
}

func newVerifier(provider string, issuers []string, jwksURL string, audiences []string, opts ...Option) (*ProviderVerifier, error) {
	cleaned := make([]string, 0, len(audiences))
	for _, a := range audiences {
		if a = strings.TrimSpace(a); a != "" {
			cleaned = append(cleaned, a)
		}
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("%w (provider %s)", ErrNoAudience, provider)
	}

	v := &ProviderVerifier{
		provider:  provider,
		issuers:   issuers,
		audiences: cleaned,
		now:       time.Now,
	}
	v.keys = newKeySet(jwksURL, &http.Client{Timeout: httpTimeout}, func() time.Time { return v.now() })

	for _, opt := range opts {
		opt(v)
	}
	return v, nil
}

// idTokenClaims is the subset of the OIDC claim set this API uses.
type idTokenClaims struct {
	jwt.RegisteredClaims
	Email         string       `json:"email"`
	EmailVerified flexibleBool `json:"email_verified"`
	Name          string       `json:"name"`
}

// Verify checks the token's signature and claims and returns the identity it
// asserts.
//
// Every rejection returns ErrInvalidToken with no detail for the caller to
// forward; the reason is preserved in the wrapped error for logging only. A
// failure to reach the provider's keys returns ErrKeysUnavailable instead,
// because "we could not check" is not "this token is bad".
func (v *ProviderVerifier) Verify(ctx context.Context, idToken string) (Identity, error) {
	if v == nil {
		return Identity{}, ErrKeysUnavailable
	}

	// Set when key lookup fails for an infrastructure reason. The JWT
	// library folds every keyfunc error into its own parse error, which
	// would otherwise make an outage indistinguishable from a forged token.
	var keyErr error

	keyfunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, err := v.keys.key(ctx, kid)
		if err != nil {
			if !errors.Is(err, errUnknownKey) {
				keyErr = err
			}
			return nil, err
		}
		return key, nil
	}

	var claims idTokenClaims
	_, err := jwt.ParseWithClaims(idToken, &claims, keyfunc,
		jwt.WithValidMethods([]string{signingAlg}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(func() time.Time { return v.now().UTC() }),
	)
	if keyErr != nil {
		return Identity{}, keyErr
	}
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %s: %v", ErrInvalidToken, v.provider, err)
	}

	// Issuer and audience are checked here rather than through
	// jwt.WithIssuer/WithAudience because both are sets: Google mints two
	// issuer spellings, and one deployment accepts several client ids.
	if !contains(v.issuers, claims.Issuer) {
		return Identity{}, fmt.Errorf("%w: %s: unexpected issuer %q", ErrInvalidToken, v.provider, claims.Issuer)
	}
	if !containsAny(v.audiences, claims.Audience) {
		// The audience is the anti-replay control: a token minted for
		// another app must never authenticate a user here.
		return Identity{}, fmt.Errorf("%w: %s: audience is not this application", ErrInvalidToken, v.provider)
	}
	if claims.Subject == "" {
		return Identity{}, fmt.Errorf("%w: %s: token has no subject", ErrInvalidToken, v.provider)
	}

	return Identity{
		Provider:      v.provider,
		Subject:       claims.Subject,
		Email:         strings.ToLower(strings.TrimSpace(claims.Email)),
		EmailVerified: bool(claims.EmailVerified),
		Name:          strings.TrimSpace(claims.Name),
	}, nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func containsAny(allowed []string, values []string) bool {
	for _, v := range values {
		if contains(allowed, v) {
			return true
		}
	}
	return false
}

// flexibleBool decodes a JSON boolean that a provider may send as a string.
//
// Apple sends "email_verified" as the string "true" on some tokens and as a
// real boolean on others; a plain bool field silently fails to unmarshal the
// string form, which would downgrade a verified address to unverified and
// block the account-linking path.
type flexibleBool bool

func (b *flexibleBool) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	switch value := raw.(type) {
	case bool:
		*b = flexibleBool(value)
	case string:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("social: email_verified: %q is not a boolean", value)
		}
		*b = flexibleBool(parsed)
	case nil:
		*b = false
	default:
		return fmt.Errorf("social: email_verified: unexpected JSON type %T", raw)
	}
	return nil
}
