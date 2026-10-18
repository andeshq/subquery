package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/andeshq/subquery/internal/config"
)

const hmacSecret = "reallyreallyreallyreallyverysafe!"

func verifierFor(t testing.TB, mutate func(*config.Config)) *Verifier {
	t.Helper()
	cfg := config.Default()
	cfg.JWTSecret = hmacSecret
	cfg.DBAnonRole = "anon"
	if mutate != nil {
		mutate(cfg)
	}
	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func signHS(t testing.TB, secret string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestVerifyHMAC(t *testing.T) {
	v := verifierFor(t, nil)
	token := signHS(t, hmacSecret, jwt.MapClaims{
		"role": "webuser",
		"sub":  "user_123",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	claims, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Role != "webuser" {
		t.Errorf("Role = %q, want webuser", claims.Role)
	}
	var raw map[string]any
	if err := json.Unmarshal(claims.Raw, &raw); err != nil {
		t.Fatalf("raw payload is not JSON: %v", err)
	}
	if raw["sub"] != "user_123" {
		t.Errorf("raw sub = %v, want user_123", raw["sub"])
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	v := verifierFor(t, nil)
	token := signHS(t, "anotherreallyreallyreallylongsecret!!", jwt.MapClaims{"role": "webuser"})
	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("expected verification to fail with the wrong secret")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	v := verifierFor(t, nil)
	now := time.Now()
	v.now = func() time.Time { return now }
	token := signHS(t, hmacSecret, jwt.MapClaims{
		"role": "webuser",
		"exp":  now.Add(-2 * time.Minute).Unix(),
	})
	_, err := v.Verify(t.Context(), token)
	if err == nil {
		t.Fatal("expected expired token to be rejected")
	}
	if _, ok := errors.AsType[*InvalidTokenError](err); !ok {
		t.Fatalf("error type = %T, want *InvalidTokenError", err)
	}
}

func TestVerifyAllowsClockSkew(t *testing.T) {
	v := verifierFor(t, nil)
	now := time.Now()
	v.now = func() time.Time { return now }
	// Expired 10 seconds ago: within the 30-second allowance.
	token := signHS(t, hmacSecret, jwt.MapClaims{
		"role": "webuser",
		"exp":  now.Add(-10 * time.Second).Unix(),
	})
	if _, err := v.Verify(t.Context(), token); err != nil {
		t.Fatalf("expected exp within skew to be accepted, got %v", err)
	}
}

func TestVerifyRoleClaimJSONPath(t *testing.T) {
	v := verifierFor(t, func(c *config.Config) {
		c.JWTRoleClaimKey = "$.postgrest.roles[1]"
	})
	token := signHS(t, hmacSecret, jwt.MapClaims{
		"postgrest": map[string]any{"roles": []any{"other", "author"}},
	})
	claims, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Role != "author" {
		t.Errorf("Role = %q, want author", claims.Role)
	}
}

func TestVerifyMissingRoleFallsBackToAnon(t *testing.T) {
	v := verifierFor(t, nil)
	token := signHS(t, hmacSecret, jwt.MapClaims{"sub": "user_123"})
	claims, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Role != "" {
		t.Errorf("Role = %q, want empty (caller falls back to anon)", claims.Role)
	}
}

func TestAudienceValidation(t *testing.T) {
	v := verifierFor(t, func(c *config.Config) { c.JWTAud = "api" })
	tests := []struct {
		name    string
		aud     any
		wantErr bool
	}{
		{"absent", nil, false},
		{"matching string", "api", false},
		{"string list contains", []any{"web", "api"}, false},
		{"empty list", []any{}, false},
		{"mismatch", "other", true},
		{"list without match", []any{"web"}, true},
		{"wrong type", 7, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"role": "webuser"}
			if tc.aud != nil {
				claims["aud"] = tc.aud
			}
			_, err := v.Verify(t.Context(), signHS(t, hmacSecret, claims))
			if tc.wantErr && err == nil {
				t.Fatal("expected a claims error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr {
				if _, ok := errors.AsType[*ClaimsError](err); !ok {
					t.Fatalf("error type = %T, want *ClaimsError", err)
				}
			}
		})
	}
}

func TestVerifyWithoutSecret(t *testing.T) {
	cfg := config.Default()
	cfg.JWTSecret = ""
	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	_, err = v.Verify(t.Context(), "a.b.c")
	if !errors.Is(err, ErrSecretMissing) {
		t.Fatalf("error = %v, want ErrSecretMissing", err)
	}
}

// jwksServer serves a mutable JWKS document and counts fetches.
type jwksServer struct {
	mu    sync.Mutex
	body  []byte
	count int
	srv   *httptest.Server
}

func newJWKSServer(t *testing.T, body []byte) *jwksServer {
	t.Helper()
	js := &jwksServer{body: body}
	js.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		js.mu.Lock()
		defer js.mu.Unlock()
		js.count++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		_, _ = w.Write(js.body)
	}))
	t.Cleanup(js.srv.Close)
	return js
}

func (js *jwksServer) set(body []byte) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.body = body
}

func (js *jwksServer) fetches() int {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.count
}

func rsaJWK(t *testing.T, kid string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func jwksDoc(t *testing.T, keys ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return data
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return key
}

func signRS(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestVerifyRemoteJWKS(t *testing.T) {
	key := newRSAKey(t)
	js := newJWKSServer(t, jwksDoc(t, rsaJWK(t, "k1", &key.PublicKey)))

	v := verifierFor(t, func(c *config.Config) { c.JWTSecret = js.srv.URL })
	token := signRS(t, key, "k1", jwt.MapClaims{"role": "webuser"})

	claims, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Role != "webuser" {
		t.Errorf("Role = %q, want webuser", claims.Role)
	}
	if got := js.fetches(); got != 1 {
		t.Errorf("fetches = %d, want 1", got)
	}

	// A second request must be served from cache.
	if _, err := v.Verify(t.Context(), token); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if got := js.fetches(); got != 1 {
		t.Errorf("fetches after cached verify = %d, want 1", got)
	}
}

func TestVerifyRemoteJWKSRotatesOnUnknownKid(t *testing.T) {
	oldKey := newRSAKey(t)
	newKey := newRSAKey(t)
	js := newJWKSServer(t, jwksDoc(t, rsaJWK(t, "k1", &oldKey.PublicKey)))

	v := verifierFor(t, func(c *config.Config) { c.JWTSecret = js.srv.URL })
	if _, err := v.Verify(t.Context(), signRS(t, oldKey, "k1", jwt.MapClaims{"role": "webuser"})); err != nil {
		t.Fatalf("initial Verify: %v", err)
	}

	// The identity provider rotates keys; the next token has an unknown kid.
	js.set(jwksDoc(t, rsaJWK(t, "k2", &newKey.PublicKey)))
	claims, err := v.Verify(t.Context(), signRS(t, newKey, "k2", jwt.MapClaims{"role": "author"}))
	if err != nil {
		t.Fatalf("Verify after rotation: %v", err)
	}
	if claims.Role != "author" {
		t.Errorf("Role = %q, want author", claims.Role)
	}
	if got := js.fetches(); got < 2 {
		t.Errorf("fetches = %d, want >= 2 after rotation", got)
	}
}

func TestVerifyJWKSFromFile(t *testing.T) {
	key := newRSAKey(t)
	dir := t.TempDir()
	path := dir + "/jwks.json"
	if err := os.WriteFile(path, jwksDoc(t, rsaJWK(t, "k1", &key.PublicKey)), 0o600); err != nil {
		t.Fatalf("write jwks file: %v", err)
	}
	v := verifierFor(t, func(c *config.Config) { c.JWTSecret = "@" + path })
	if _, err := v.Verify(t.Context(), signRS(t, key, "k1", jwt.MapClaims{"role": "webuser"})); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}
