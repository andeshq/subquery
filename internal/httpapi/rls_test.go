package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/andeshq/subquery/internal/config"
)

const testJWTSecret = "reallyreallyreallyreallyverysafe!"

// token mints an HS256 token with the given claims.
func token(t *testing.T, claims map[string]any) string {
	t.Helper()
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims(claims)).SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func bearer(value string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + value}
}

// TestLiveRLSIsolation proves that the impersonated role and the
// request.jwt.claims GUC reach Postgres, and that row-level security uses them.
func TestLiveRLSIsolation(t *testing.T) {
	s := newLiveServer(t)

	// Anonymous: no claims, so the policy matches nothing.
	rec := get(t, s, "/secrets", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anon status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 0 {
		t.Fatalf("anonymous request saw %d rows, want 0", len(rows))
	}

	// A token for user_123 sees only user_123's row.
	alice := token(t, map[string]any{"role": "webuser", "sub": "user_123"})
	rec = get(t, s, "/secrets?select=owner,payload", bearer(alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["owner"] != "user_123" || rows[0]["payload"] != "alice secret" {
		t.Fatalf("alice rows = %#v", rows)
	}

	// A different subject sees a different row.
	bob := token(t, map[string]any{"role": "webuser", "sub": "user_999"})
	rec = get(t, s, "/secrets?select=owner", bearer(bob))
	rows = decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["owner"] != "user_999" {
		t.Fatalf("bob rows = %#v", rows)
	}
}

// TestLivePreparedStatementsAcrossRoles forces a single pooled connection so
// every request reuses the same prepared statements, and interleaves roles and
// claim sets. A cached plan or a leaked setting would surface as a wrong status
// or a leaked row.
func TestLivePreparedStatementsAcrossRoles(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) { c.DBPool = 1 })

	viewer := token(t, map[string]any{"role": "viewer", "sub": "user_v"})
	alice := token(t, map[string]any{"role": "webuser", "sub": "user_123"})
	bob := token(t, map[string]any{"role": "webuser", "sub": "user_999"})

	for range 40 {
		// viewer holds no grant on secrets.
		if rec := get(t, s, "/secrets?select=owner", bearer(viewer)); rec.Code != http.StatusForbidden {
			t.Fatalf("viewer status = %d, body = %s", rec.Code, rec.Body.String())
		}
		// webuser is granted, but RLS still restricts rows by the sub claim.
		rec := get(t, s, "/secrets?select=owner", bearer(alice))
		if rec.Code != http.StatusOK {
			t.Fatalf("alice status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if rows := decodeRows(t, rec); len(rows) != 1 || rows[0]["owner"] != "user_123" {
			t.Fatalf("alice rows = %#v", rows)
		}
		rec = get(t, s, "/secrets?select=owner", bearer(bob))
		if rec.Code != http.StatusOK {
			t.Fatalf("bob status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if rows := decodeRows(t, rec); len(rows) != 1 || rows[0]["owner"] != "user_999" {
			t.Fatalf("bob rows = %#v", rows)
		}
		// Anonymous carries no claims, so the policy matches nothing.
		rec = get(t, s, "/secrets?select=owner", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("anon status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if rows := decodeRows(t, rec); len(rows) != 0 {
			t.Fatalf("anonymous saw %#v", rows)
		}
	}
}

// TestLiveRLSRoleSwitch proves the JWT role claim actually switches the
// database role: viewer holds no grant on secrets.
func TestLiveRLSRoleSwitch(t *testing.T) {
	s := newLiveServer(t)

	restricted := token(t, map[string]any{"role": "viewer", "sub": "user_123"})
	rec := get(t, s, "/secrets", bearer(restricted))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "42501" {
		t.Errorf("code = %v, want the PostgreSQL insufficient_privilege SQLSTATE", code)
	}

	// The same subject with a role that holds the grant succeeds.
	allowed := token(t, map[string]any{"role": "webuser", "sub": "user_123"})
	if rec := get(t, s, "/secrets", bearer(allowed)); rec.Code != http.StatusOK {
		t.Fatalf("webuser status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestLivePreRequest proves db-pre-request runs with request.headers populated
// and can reject a request.
func TestLivePreRequest(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) {
		c.DBPreRequest = "postgrest.check_request"
	})

	if rec := get(t, s, "/todos", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec := get(t, s, "/todos", map[string]string{"X-Block": "yes"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeError(t, rec)
	if body["code"] != "P0001" {
		t.Errorf("code = %v, want P0001", body["code"])
	}
	if message, _ := body["message"].(string); !strings.Contains(message, "blocked by pre-request") {
		t.Errorf("message = %v", body["message"])
	}
}

// TestLiveClaimsGUCIsAbsentForAnonymous documents that request.jwt.claims is not
// set without a token, so current_setting(..., true) is NULL.
func TestLiveClaimsGUCIsAbsentForAnonymous(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/secrets?select=owner", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The policy compares owner to NULL, which is never true.
	if rows := decodeRows(t, rec); len(rows) != 0 {
		t.Fatalf("rows = %#v", rows)
	}
}
