package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andeshq/subquery/internal/config"
)

func ecJWK(t *testing.T, kid string, crv string, key *ecdsa.PrivateKey) map[string]any {
	t.Helper()
	size := (key.Curve.Params().BitSize + 7) / 8
	return map[string]any{
		"kty": "EC", "kid": kid, "alg": "ES256", "use": "sig", "crv": crv,
		"x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, size))),
		"y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, size))),
	}
}

func TestVerifyEllipticCurveKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		crv  string
		key  func() *ecdsa.PrivateKey
	}{
		{"P-256", "P-256", func() *ecdsa.PrivateKey { k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader); return k }},
		{"P-384", "P-384", func() *ecdsa.PrivateKey { k, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader); return k }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.key()
			js := newJWKSServer(t, jwksDoc(t, ecJWK(t, "ec1", tc.crv, key)))
			source := newRemoteSource(js.srv.URL)
			keys, err := source.keys(t.Context(), false)
			if err != nil {
				t.Fatalf("keys: %v", err)
			}
			if len(keys) != 1 || keys[0].kind != keyEC {
				t.Fatalf("keys = %+v", keys)
			}
			if _, ok := keys[0].key.(*ecdsa.PublicKey); !ok {
				t.Fatalf("key type = %T, want *ecdsa.PublicKey", keys[0].key)
			}
			if methods := methodsFor(keys); len(methods) == 0 || methods[0] != "ES256" {
				t.Errorf("methods = %v", methods)
			}
		})
	}
}

func TestVerifyEd25519Key(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	jwk := map[string]any{
		"kty": "OKP", "kid": "okp1", "alg": "EdDSA", "use": "sig", "crv": "Ed25519",
		"x": base64.RawURLEncoding.EncodeToString(public),
	}
	keys, err := parseJWKS(jwksDoc(t, jwk))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	if len(keys) != 1 || keys[0].kind != keyEd25519 {
		t.Fatalf("keys = %+v", keys)
	}
	if methods := methodsFor(keys); len(methods) != 1 || methods[0] != "EdDSA" {
		t.Errorf("methods = %v", methods)
	}
}

func TestVerifySymmetricJWKS(t *testing.T) {
	secret := []byte("reallyreallyreallyreallyverysafe!")
	jwk := map[string]any{
		"kty": "oct", "kid": "sym1", "use": "sig",
		"k": base64.RawURLEncoding.EncodeToString(secret),
	}
	keys, err := parseJWKS(jwksDoc(t, jwk))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	if len(keys) != 1 || keys[0].kind != keyHMAC {
		t.Fatalf("keys = %+v", keys)
	}
}

func TestParseJWKSRejectsMalformedDocuments(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"not json", `{`},
		{"a scalar", `1`},
		{"no keys", `{"keys":[]}`},
		{"keys not objects", `{"keys":[1]}`},
		{"array of non-objects", `[1]`},
		{"unsupported kty", `{"kty":"XYZ"}`},
		{"unsupported curve", `{"kty":"EC","crv":"P-999","x":"AA","y":"AA"}`},
		{"unsupported OKP curve", `{"kty":"OKP","crv":"X25519","x":"AA"}`},
		{"RSA without n", `{"kty":"RSA","e":"AQAB"}`},
		{"RSA without e", `{"kty":"RSA","n":"AQAB"}`},
		{"RSA with a small exponent", `{"kty":"RSA","n":"AQAB","e":"AQ"}`},
		{"non-base64 modulus", `{"kty":"RSA","n":"!!!","e":"AQAB"}`},
		{"Ed25519 with the wrong length", `{"kty":"OKP","crv":"Ed25519","x":"AAAA"}`},
		{"an encryption key", `{"kty":"oct","use":"enc","k":"AAAA"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseJWKS([]byte(tc.doc)); err == nil {
				t.Fatalf("parseJWKS accepted %s", tc.doc)
			}
		})
	}
}

func TestParseJWKSAcceptsASingleKeyAndABareArray(t *testing.T) {
	key := newRSAKey(t)
	single, err := json.Marshal(rsaJWK(t, "k1", &key.PublicKey))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if keys, err := parseJWKS(single); err != nil || len(keys) != 1 {
		t.Fatalf("a bare JWK should parse: %v, %v", keys, err)
	}
	array := []byte(`[` + string(single) + `]`)
	if keys, err := parseJWKS(array); err != nil || len(keys) != 1 {
		t.Fatalf("a bare array should parse: %v, %v", keys, err)
	}
}

func TestRemoteSourceHandlesFailures(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()
		source := newRemoteSource(server.URL)
		if _, err := source.keys(t.Context(), false); err == nil {
			t.Fatal("a non-200 response should fail")
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer server.Close()
		source := newRemoteSource(server.URL)
		if _, err := source.keys(t.Context(), false); err == nil {
			t.Fatal("an invalid body should fail")
		}
	})

	t.Run("stale keys survive an outage", func(t *testing.T) {
		key := newRSAKey(t)
		var fail bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if fail {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(jwksDoc(t, rsaJWK(t, "k1", &key.PublicKey)))
		}))
		defer server.Close()

		source := newRemoteSource(server.URL)
		source.ttl = time.Nanosecond
		if _, err := source.keys(t.Context(), false); err != nil {
			t.Fatalf("first fetch: %v", err)
		}
		fail = true
		keys, err := source.keys(t.Context(), true)
		if err != nil {
			t.Fatalf("a refresh failure should fall back to cached keys: %v", err)
		}
		if len(keys) != 1 {
			t.Fatalf("keys = %+v", keys)
		}
	})

	t.Run("connection refused with no cache", func(t *testing.T) {
		source := newRemoteSource("http://127.0.0.1:1/jwks")
		if _, err := source.keys(t.Context(), false); err == nil {
			t.Fatal("an unreachable host should fail")
		}
	})
}

func TestCacheTTLParsing(t *testing.T) {
	fallback := time.Minute
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"", fallback},
		{"max-age=60", time.Minute},
		{"public, max-age=30", 30 * time.Second},
		{"MAX-AGE=90", 90 * time.Second},
		{"max-age=0", 0},
		{"no-store", fallback},
		{"max-age=nonsense", fallback},
	}
	for _, tc := range tests {
		if got := cacheTTL(tc.header, fallback); got != tc.want {
			t.Errorf("cacheTTL(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestErrorTypesUnwrap(t *testing.T) {
	cause := errors.New("cause")
	invalid := &InvalidTokenError{Err: cause}
	if !errors.Is(invalid, cause) {
		t.Error("InvalidTokenError should unwrap")
	}
	if !strings.Contains(invalid.Error(), "cause") {
		t.Errorf("InvalidTokenError.Error() = %q", invalid.Error())
	}
	claims := &ClaimsError{Err: cause}
	if !errors.Is(claims, cause) {
		t.Error("ClaimsError should unwrap")
	}
	if !strings.Contains(claims.Error(), "cause") {
		t.Errorf("ClaimsError.Error() = %q", claims.Error())
	}
	// errors.AsType reaches the typed error through a wrapper.
	if _, ok := errors.AsType[*InvalidTokenError](fmt.Errorf("outer: %w", invalid)); !ok {
		t.Error("AsType should find a wrapped InvalidTokenError")
	}
	if _, ok := errors.AsType[*ClaimsError](fmt.Errorf("outer: %w", claims)); !ok {
		t.Error("AsType should find a wrapped ClaimsError")
	}
}

func TestKeySourceFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr bool
		wantNil bool
	}{
		{"none", func(c *config.Config) { c.JWTSecret = "" }, false, true},
		{"plain", func(c *config.Config) { c.JWTSecret = hmacSecret }, false, false},
		{"short plain", func(c *config.Config) { c.JWTSecret = "short" }, true, false},
		{"base64", func(c *config.Config) {
			c.JWTSecret = base64.StdEncoding.EncodeToString([]byte(hmacSecret))
			c.JWTSecretIsBase64 = true
		}, false, false},
		{"bad base64", func(c *config.Config) {
			c.JWTSecret = "!!!not-base64!!!"
			c.JWTSecretIsBase64 = true
		}, true, false},
		{"literal jwks", func(c *config.Config) { c.JWTSecret = `{"kty":"oct","k":"AAAA"}` }, false, false},
		{"bad literal jwks", func(c *config.Config) { c.JWTSecret = `{"kty":"XYZ"}` }, true, false},
		{"missing file", func(c *config.Config) { c.JWTSecret = "@/nonexistent/jwks.json" }, true, false},
		{"remote url", func(c *config.Config) { c.JWTSecret = "https://example.invalid/jwks" }, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(cfg)
			source, err := keySourceFromConfig(cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("keySourceFromConfig: %v", err)
			}
			if (source == nil) != tc.wantNil {
				t.Errorf("source = %v, want nil %v", source, tc.wantNil)
			}
		})
	}
}

func TestBuildKeyMethods(t *testing.T) {
	methods := methodsFor([]key{{kind: keyHMAC}, {kind: keyRSA}, {kind: keyEC}, {kind: keyEd25519}})
	for _, want := range []string{"HS256", "RS256", "PS512", "ES384", "EdDSA"} {
		if !containsString(methods, want) {
			t.Errorf("methods %v is missing %s", methods, want)
		}
	}
	if got := methodsFor(nil); len(got) != 0 {
		t.Errorf("methodsFor(nil) = %v", got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestPublicKeyFromJWKProducesUsableRSA(t *testing.T) {
	key := newRSAKey(t)
	keys, err := parseJWKS(jwksDoc(t, rsaJWK(t, "k1", &key.PublicKey)))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	public, ok := keys[0].key.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("key type = %T, want *rsa.PublicKey", keys[0].key)
	}
	if public.N.Cmp(key.N) != 0 || public.E != key.E {
		t.Error("the decoded public key does not match")
	}
}
