package pg

import (
	"testing"
)

func TestQualifiedIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{"check_request", `"check_request"`, false},
		{"postgrest.check_request", `"postgrest"."check_request"`, false},
		{" postgrest . check_request ", `"postgrest"."check_request"`, false},
		{`Weird Name.Tricky"Name`, `"Weird Name"."Tricky""Name"`, false},
		{"a.b.c", "", true},
		{"", "", true},
		{"a.", "", true},
		{".b", "", true},
		{"a..b", "", true},
	}
	for _, tc := range tests {
		got, err := qualifiedIdentifier(tc.name)
		if tc.wantErr {
			if err == nil {
				t.Errorf("qualifiedIdentifier(%q) succeeded, want an error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("qualifiedIdentifier(%q): %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("qualifiedIdentifier(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestBeginRejectsUnknownRole asserts the transaction is rolled back when the
// request settings cannot be applied.
func TestBeginRejectsUnknownRole(t *testing.T) {
	uri := testDatabaseURI(t)
	cfg := testConfig(uri)

	pool, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	tx, err := Begin(t.Context(), pool, RequestSettings{Role: "no_such_role_exists"})
	if err == nil {
		_ = tx.Rollback(t.Context())
		t.Fatal("an unknown role should be rejected")
	}

	// The pool must still be usable: the failed transaction was rolled back.
	tx, err = Begin(t.Context(), pool, RequestSettings{Role: "anon", SearchPath: []string{"public"}})
	if err != nil {
		t.Fatalf("a later transaction failed: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	var role string
	if err := tx.QueryRow(t.Context(), "select current_user").Scan(&role); err != nil {
		t.Fatalf("select current_user: %v", err)
	}
	if role != "anon" {
		t.Errorf("current_user = %q, want anon", role)
	}
}

func TestBeginRejectsMalformedPreRequest(t *testing.T) {
	uri := testDatabaseURI(t)
	cfg := testConfig(uri)

	pool, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	tx, err := Begin(t.Context(), pool, RequestSettings{
		Role:       "anon",
		PreRequest: "a.b.c",
	})
	if err == nil {
		_ = tx.Rollback(t.Context())
		t.Fatal("a malformed db-pre-request should be rejected")
	}
}

// TestNewPoolRejectsBadURI asserts a configuration error surfaces at startup
// rather than on the first request.
func TestNewPoolRejectsBadURI(t *testing.T) {
	cfg := testConfig("postgres://user:pass@127.0.0.1:1/absent?connect_timeout=1")
	cfg.DBPoolAcquisitionAfter = 0
	pool, err := NewPool(t.Context(), cfg)
	if err == nil {
		pool.Close()
		t.Fatal("an unreachable database should fail pool creation")
	}
}
