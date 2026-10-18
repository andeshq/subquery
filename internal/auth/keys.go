package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andeshq/subquery/internal/config"
)

// keyKind describes how a verification key can be used.
type keyKind int

const (
	keyHMAC keyKind = iota
	keyRSA
	keyEC
	keyEd25519
)

type key struct {
	kid  string
	alg  string
	use  string
	kind keyKind
	key  any // []byte, *rsa.PublicKey, *ecdsa.PublicKey, or ed25519.PublicKey
}

// keySource supplies verification keys, optionally refreshing a remote set.
// force ignores any cached freshness and re-fetches.
type keySource interface {
	keys(ctx context.Context, force bool) ([]key, error)
}

type staticSource struct{ ks []key }

func (s staticSource) keys(context.Context, bool) ([]key, error) { return s.ks, nil }

const (
	defaultJWKSTTL = 5 * time.Minute
	maxJWKSBytes   = 1 << 20
)

// remoteSource fetches a JWKS over HTTP and caches it according to the
// response's Cache-Control max-age.
type remoteSource struct {
	url    string
	client *http.Client
	ttl    time.Duration

	// mu guards ks and expires.
	mu      sync.Mutex
	ks      []key
	expires time.Time

	// fetchMu serializes refreshes so a burst of requests triggers one fetch.
	// It is held across the network call, but never by a reader that finds a
	// fresh cache, so a refresh does not stall unrelated verifications.
	fetchMu sync.Mutex
}

func newRemoteSource(url string) *remoteSource {
	return &remoteSource{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
		ttl:    defaultJWKSTTL,
	}
}

func (r *remoteSource) keys(ctx context.Context, force bool) ([]key, error) {
	if !force {
		if ks, ok := r.fresh(); ok {
			return ks, nil
		}
	}

	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	// Another goroutine may have refreshed while we waited for the fetch lock.
	if !force {
		if ks, ok := r.fresh(); ok {
			return ks, nil
		}
	}

	ks, ttl, err := r.fetch(ctx)
	if err != nil {
		// Serve stale keys if the identity provider is temporarily unavailable.
		if stale, ok := r.stale(); ok {
			return stale, nil
		}
		return nil, err
	}
	r.mu.Lock()
	r.ks = ks
	r.expires = time.Now().Add(ttl)
	r.mu.Unlock()
	return ks, nil
}

// fresh reports whether the cached set exists and is still within its TTL.
func (r *remoteSource) fresh() ([]key, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ks) == 0 || !time.Now().Before(r.expires) {
		return nil, false
	}
	return r.ks, true
}

// stale returns the cached set regardless of age.
func (r *remoteSource) stale() ([]key, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ks) == 0 {
		return nil, false
	}
	return r.ks, true
}

func (r *remoteSource) fetch(ctx context.Context) ([]key, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("fetch jwks: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("fetch jwks: %w", err)
	}
	ks, err := parseJWKS(body)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch jwks: %w", err)
	}
	return ks, cacheTTL(resp.Header.Get("Cache-Control"), r.ttl), nil
}

func cacheTTL(header string, fallback time.Duration) time.Duration {
	for part := range strings.SplitSeq(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(name, "max-age") {
			if secs, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && secs >= 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return fallback
}

// keySourceFromConfig interprets jwt-secret as a plain secret, base64 secret,
// @file, literal JWK/JWKS, or remote JWKS URL.
func keySourceFromConfig(cfg *config.Config) (keySource, error) {
	switch cfg.JWTSecretKind() {
	case config.JWTSecretNone:
		return nil, nil

	case config.JWTSecretPlain:
		secret := strings.TrimSpace(cfg.JWTSecret)
		if len(secret) < 32 {
			return nil, errors.New("jwt-secret must be at least 32 characters")
		}
		return staticSource{ks: []key{{kind: keyHMAC, key: []byte(secret)}}}, nil

	case config.JWTSecretBase64:
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.JWTSecret))
		if err != nil {
			return nil, fmt.Errorf("jwt-secret-is-base64: %w", err)
		}
		return staticSource{ks: []key{{kind: keyHMAC, key: raw}}}, nil

	case config.JWTSecretFile:
		path, _ := strings.CutPrefix(strings.TrimSpace(cfg.JWTSecret), "@")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read jwt-secret file: %w", err)
		}
		return staticSourceFromBytes(data)

	case config.JWTSecretJWKS:
		ks, err := parseJWKS([]byte(cfg.JWTSecret))
		if err != nil {
			return nil, fmt.Errorf("jwt-secret: %w", err)
		}
		return staticSource{ks: ks}, nil

	case config.JWTSecretURL:
		return newRemoteSource(strings.TrimSpace(cfg.JWTSecret)), nil

	default:
		return nil, errors.New("unsupported jwt-secret")
	}
}

func staticSourceFromBytes(data []byte) (keySource, error) {
	trimmed := strings.TrimSpace(string(data))
	if isJSONDoc(trimmed) {
		ks, err := parseJWKS([]byte(trimmed))
		if err != nil {
			return nil, fmt.Errorf("jwt-secret file: %w", err)
		}
		return staticSource{ks: ks}, nil
	}
	if len(trimmed) < 32 {
		return nil, errors.New("jwt-secret file must contain at least 32 characters or a JWK/JWKS")
	}
	return staticSource{ks: []key{{kind: keyHMAC, key: []byte(trimmed)}}}, nil
}

func isJSONDoc(s string) bool {
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}

// parseJWKS accepts a single JWK, a JWKS document ({"keys":[...]}), or a bare
// array of JWKs.
func parseJWKS(data []byte) ([]key, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var entries []map[string]any
	switch v := doc.(type) {
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("jwks entry is not an object")
			}
			entries = append(entries, m)
		}
	case map[string]any:
		if set, ok := v["keys"].([]any); ok {
			for _, item := range set {
				m, ok := item.(map[string]any)
				if !ok {
					return nil, errors.New("jwks key is not an object")
				}
				entries = append(entries, m)
			}
		} else {
			entries = append(entries, v)
		}
	default:
		return nil, errors.New("jwks must be an object or array")
	}
	if len(entries) == 0 {
		return nil, errors.New("jwks contains no keys")
	}
	ks := make([]key, 0, len(entries))
	for _, m := range entries {
		k, err := parseJWK(m)
		if err != nil {
			return nil, err
		}
		ks = append(ks, k)
	}
	return ks, nil
}

func parseJWK(m map[string]any) (key, error) {
	k := key{
		kid: str(m, "kid"),
		alg: str(m, "alg"),
		use: str(m, "use"),
	}
	if k.use != "" && k.use != "sig" {
		return key{}, fmt.Errorf("jwks key %q is not a signing key", k.kid)
	}
	switch kty := str(m, "kty"); kty {
	case "RSA":
		n, err := b64Field(m, "n")
		if err != nil {
			return key{}, err
		}
		e, err := b64Field(m, "e")
		if err != nil {
			return key{}, err
		}
		var exp int
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		if exp < 3 {
			return key{}, errors.New("jwks RSA exponent is invalid")
		}
		k.kind = keyRSA
		k.key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	case "EC":
		curve, err := curveFor(str(m, "crv"))
		if err != nil {
			return key{}, err
		}
		x, err := b64Field(m, "x")
		if err != nil {
			return key{}, err
		}
		y, err := b64Field(m, "y")
		if err != nil {
			return key{}, err
		}
		k.kind = keyEC
		k.key = &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	case "oct":
		raw, err := b64Field(m, "k")
		if err != nil {
			return key{}, err
		}
		k.kind = keyHMAC
		k.key = raw
	case "OKP":
		if crv := str(m, "crv"); crv != "Ed25519" {
			return key{}, fmt.Errorf("unsupported OKP curve %q", crv)
		}
		raw, err := b64Field(m, "x")
		if err != nil {
			return key{}, err
		}
		if len(raw) != ed25519.PublicKeySize {
			return key{}, errors.New("jwks Ed25519 key has invalid length")
		}
		k.kind = keyEd25519
		k.key = ed25519.PublicKey(raw)
	default:
		return key{}, fmt.Errorf("unsupported jwks kty %q", kty)
	}
	return k, nil
}

func curveFor(name string) (elliptic.Curve, error) {
	switch name {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", name)
	}
}

func b64Field(m map[string]any, name string) ([]byte, error) {
	s := str(m, name)
	if s == "" {
		return nil, fmt.Errorf("jwks key is missing %q", name)
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("jwks key %q is not base64url: %w", name, err)
	}
	return b, nil
}

func str(m map[string]any, name string) string {
	s, _ := m[name].(string)
	return s
}
