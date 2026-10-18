// Package auth authenticates bearer tokens and resolves the Postgres role and
// claim set for a request.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/ohler55/ojg/jp"

	"github.com/andeshq/subquery/internal/config"
)

// clockSkew mirrors PostgREST's 30-second allowance on exp/iat/nbf.
const clockSkew = 30 * time.Second

// ErrSecretMissing is returned when a bearer token is presented but no
// jwt-secret is configured (PostgREST PGRST300).
var ErrSecretMissing = errors.New("jwt secret is missing from the configuration")

// InvalidTokenError reports a malformed, unverifiable, or expired token
// (PGRST301).
type InvalidTokenError struct{ Err error }

func (e *InvalidTokenError) Error() string { return "invalid token: " + e.Err.Error() }
func (e *InvalidTokenError) Unwrap() error { return e.Err }

// ClaimsError reports a token whose claims failed validation (PGRST303).
type ClaimsError struct{ Err error }

func (e *ClaimsError) Error() string { return "invalid claims: " + e.Err.Error() }
func (e *ClaimsError) Unwrap() error { return e.Err }

// Claims is the verified result of a request token.
type Claims struct {
	// Raw is the exact base64url-decoded JWT payload, passed to Postgres as
	// request.jwt.claims without re-serialization.
	Raw    []byte
	Values map[string]any
	Role   string
}

// Verifier validates bearer tokens and extracts the role claim.
type Verifier struct {
	source       keySource
	aud          string
	rolePath     jp.Expr
	rolePathText string
	now          func() time.Time
}

// NewVerifier builds a verifier from configuration. A nil jwt-secret yields a
// verifier that only accepts unauthenticated requests.
func NewVerifier(cfg *config.Config) (*Verifier, error) {
	src, err := keySourceFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	expr, err := jp.ParseString(cfg.JWTRoleClaimKey)
	if err != nil {
		return nil, fmt.Errorf("jwt-role-claim-key %q: %w", cfg.JWTRoleClaimKey, err)
	}
	return &Verifier{
		source:       src,
		aud:          cfg.JWTAud,
		rolePath:     expr,
		rolePathText: cfg.JWTRoleClaimKey,
		now:          time.Now,
	}, nil
}

// Verify validates raw (the token without the "Bearer " prefix).
func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	if v.source == nil {
		return nil, ErrSecretMissing
	}
	raw = strings.TrimSpace(raw)

	kid, err := tokenKid(raw)
	if err != nil {
		return nil, &InvalidTokenError{Err: err}
	}

	ks, err := v.source.keys(ctx, false)
	if err != nil {
		return nil, &InvalidTokenError{Err: err}
	}
	if kid != "" {
		matched := filterByKid(ks, kid)
		if len(matched) == 0 {
			// The signing key may have rotated; refresh once.
			if refreshed, refreshErr := v.source.keys(ctx, true); refreshErr == nil {
				ks = refreshed
				matched = filterByKid(ks, kid)
			}
		}
		if len(matched) == 0 {
			return nil, &InvalidTokenError{Err: fmt.Errorf("no key matches kid %q", kid)}
		}
		ks = matched
	}
	methods := methodsFor(ks)
	if len(methods) == 0 {
		return nil, &InvalidTokenError{Err: errors.New("no usable verification keys")}
	}

	parser := jwt.NewParser(
		jwt.WithValidMethods(methods),
		jwt.WithLeeway(clockSkew),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.now),
	)
	claims := jwt.MapClaims{}
	if _, err := parser.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return jwt.VerificationKeySet{Keys: verificationKeys(ks)}, nil
	}); err != nil {
		return nil, &InvalidTokenError{Err: err}
	}

	payload, err := tokenPayload(raw)
	if err != nil {
		return nil, &InvalidTokenError{Err: err}
	}
	if v.aud != "" {
		if err := validateAudience(claims["aud"], v.aud); err != nil {
			return nil, &ClaimsError{Err: err}
		}
	}
	role, err := v.role(claims)
	if err != nil {
		return nil, &ClaimsError{Err: err}
	}
	return &Claims{Raw: payload, Values: claims, Role: role}, nil
}

func (v *Verifier) role(values map[string]any) (string, error) {
	found := v.rolePath.Get(values)
	if len(found) == 0 {
		// A token without a role claim falls back to the anonymous role.
		return "", nil
	}
	role, ok := found[0].(string)
	if !ok {
		return "", fmt.Errorf("role claim at %s is not a string", v.rolePathText)
	}
	return role, nil
}

func filterByKid(ks []key, kid string) []key {
	out := make([]key, 0, len(ks))
	for _, k := range ks {
		if k.kid == kid {
			out = append(out, k)
		}
	}
	return out
}

func methodsFor(ks []key) []string {
	seen := make(map[string]struct{}, len(ks))
	for _, k := range ks {
		switch k.kind {
		case keyHMAC:
			seen["HS256"], seen["HS384"], seen["HS512"] = struct{}{}, struct{}{}, struct{}{}
		case keyRSA:
			seen["RS256"], seen["RS384"], seen["RS512"] = struct{}{}, struct{}{}, struct{}{}
			seen["PS256"], seen["PS384"], seen["PS512"] = struct{}{}, struct{}{}, struct{}{}
		case keyEC:
			seen["ES256"], seen["ES384"], seen["ES512"] = struct{}{}, struct{}{}, struct{}{}
		case keyEd25519:
			seen["EdDSA"] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func verificationKeys(ks []key) []jwt.VerificationKey {
	out := make([]jwt.VerificationKey, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.key)
	}
	return out
}

func tokenKid(raw string) (string, error) {
	header, err := tokenPart(raw, 0)
	if err != nil {
		return "", err
	}
	var h struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(header, &h); err != nil {
		return "", fmt.Errorf("malformed token header: %w", err)
	}
	return h.Kid, nil
}

func tokenPayload(raw string) ([]byte, error) {
	payload, err := tokenPart(raw, 1)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func tokenPart(raw string, index int) ([]byte, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token: expected three segments")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[index])
	if err != nil {
		return nil, fmt.Errorf("malformed token segment %d: %w", index, err)
	}
	return b, nil
}

// validateAudience follows PostgREST: an absent or empty aud permits any
// audience; a string must match; an array must contain the wanted value.
func validateAudience(claim any, want string) error {
	switch aud := claim.(type) {
	case nil:
		return nil
	case string:
		if aud == want {
			return nil
		}
	case []any:
		if len(aud) == 0 {
			return nil
		}
		for _, item := range aud {
			if s, ok := item.(string); ok && s == want {
				return nil
			}
		}
	default:
		return errors.New("aud claim must be a string or an array of strings")
	}
	return fmt.Errorf("aud claim does not include %q", want)
}
