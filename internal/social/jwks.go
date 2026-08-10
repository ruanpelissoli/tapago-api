package social

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// errUnknownKey means the token names a key id the provider does not
// publish. It is a property of the token, not of our connectivity, so it
// resolves to ErrInvalidToken rather than ErrKeysUnavailable.
var errUnknownKey = errors.New("social: signing key not published by the provider")

const (
	// defaultKeyTTL is how long a fetched key set is trusted without
	// re-reading it. Both providers rotate keys on the order of days and
	// publish the new key well before signing with it, so an hour is
	// conservative while keeping the endpoint essentially free.
	defaultKeyTTL = time.Hour
	// minRefreshInterval throttles refetches triggered by an unrecognised
	// key id. Without it, a client sending random "kid" headers would turn
	// every request into an outbound HTTP call to the provider — a free
	// amplification vector against both us and them.
	minRefreshInterval = time.Minute
	// maxJWKSBytes caps the response body. The real documents are ~1 KB;
	// this only exists so a hostile or broken endpoint cannot stream us out
	// of memory.
	maxJWKSBytes = 1 << 20
	// maxJWKSKeys caps how many keys are kept from one document.
	maxJWKSKeys = 32
	// minRSABits rejects a key too small to be taken seriously. Both
	// providers publish 2048-bit keys.
	minRSABits = 2048
)

// keySet is a lazily-populated, TTL-refreshed cache of a provider's JWKS.
//
// It is safe for concurrent use. The mutex is held across the HTTP fetch,
// which serialises concurrent misses into a single request rather than a
// stampede; the fetch is bounded by the request context and the client
// timeout, so a slow provider delays sign-ins but cannot wedge the process.
type keySet struct {
	url        string
	client     *http.Client
	ttl        time.Duration
	minRefresh time.Duration
	now        func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

func newKeySet(url string, client *http.Client, now func() time.Time) *keySet {
	return &keySet{
		url:        url,
		client:     client,
		ttl:        defaultKeyTTL,
		minRefresh: minRefreshInterval,
		now:        now,
	}
}

// key returns the public key published under kid, fetching or refreshing the
// key set when needed.
func (s *keySet) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	// Both providers always set a kid, and without one there is nothing to
	// select on: trying every published key would let a token be validated
	// against a key its header never claimed.
	if kid == "" {
		return nil, errUnknownKey
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cached, hit := s.keys[kid]
	if hit && !s.expiredLocked() {
		return cached, nil
	}

	// A miss on a fresh cache means either a rotation we have not seen yet
	// or a bogus kid. Both look identical here, so the refetch is throttled.
	if s.now().Sub(s.lastAttempt) < s.minRefresh {
		if hit {
			// Stale but almost certainly still correct; serving it beats
			// failing sign-ins while the refresh window closes.
			return cached, nil
		}
		return nil, errUnknownKey
	}
	s.lastAttempt = s.now()

	if err := s.refreshLocked(ctx); err != nil {
		if hit {
			// The provider is unreachable but we still hold this key. An
			// expired cache is not a reason to reject a valid token.
			return cached, nil
		}
		return nil, err
	}

	if k, ok := s.keys[kid]; ok {
		return k, nil
	}
	return nil, errUnknownKey
}

func (s *keySet) expiredLocked() bool {
	return s.now().Sub(s.fetchedAt) >= s.ttl
}

// refreshLocked replaces the cache from the provider's JWKS endpoint. The
// caller must hold s.mu.
//
// The cache is only swapped on full success: a partial or malformed document
// leaves the previous keys in place rather than emptying the cache and
// failing every sign-in until the next refresh.
func (s *keySet) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return fmt.Errorf("%w: build request: %v", ErrKeysUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: get %s: %v", ErrKeysUnavailable, s.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: get %s: status %d", ErrKeysUnavailable, s.url, resp.StatusCode)
	}

	var doc struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return fmt.Errorf("%w: decode %s: %v", ErrKeysUnavailable, s.url, err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for i, jwk := range doc.Keys {
		if i >= maxJWKSKeys {
			break
		}
		key, ok := jwk.rsaPublicKey()
		if !ok {
			// Unusable entries are skipped, not fatal: a provider is free to
			// publish an EC key or an encryption key alongside the signing
			// keys, and rejecting the whole document would break sign-in.
			continue
		}
		keys[jwk.Kid] = key
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w: %s published no usable RSA signing keys", ErrKeysUnavailable, s.url)
	}

	s.keys = keys
	s.fetchedAt = s.now()
	return nil
}

// jsonWebKey is the subset of RFC 7517 both providers actually publish.
type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// rsaPublicKey converts a JWK to an RSA public key, reporting whether the
// entry is a usable RS256 signing key.
func (j jsonWebKey) rsaPublicKey() (*rsa.PublicKey, bool) {
	if j.Kty != "RSA" || j.Kid == "" {
		return nil, false
	}
	// "use" and "alg" are optional; when present they must say this key is
	// for RS256 signature verification.
	if j.Use != "" && j.Use != "sig" {
		return nil, false
	}
	if j.Alg != "" && j.Alg != signingAlg {
		return nil, false
	}

	nBytes, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil || len(nBytes) == 0 {
		return nil, false
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(j.E)
	// The exponent is a handful of bytes (65537 is three); anything larger is
	// malformed and would overflow the int conversion below.
	if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
		return nil, false
	}

	key := &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}
	if key.N.BitLen() < minRSABits || key.E < 3 || key.E%2 == 0 {
		return nil, false
	}
	return key, true
}
