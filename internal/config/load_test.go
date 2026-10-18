package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestLoadReadsFileThenEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subquery.conf")
	content := `
# a comment
db-uri = "postgres://authenticator:secret@localhost:5432/app"
db-anon-role = anon
server-port = 4321
db-schemas = "public, api"
db-max-rows = 250
jwt-secret = "reallyreallyreallyreallyverysafe"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBURI != "postgres://authenticator:secret@localhost:5432/app" {
		t.Errorf("DBURI = %q", cfg.DBURI)
	}
	if cfg.DBAnonRole != "anon" || cfg.ServerPort != 4321 {
		t.Errorf("DBAnonRole = %q, ServerPort = %d", cfg.DBAnonRole, cfg.ServerPort)
	}
	if cfg.DBMaxRows != 250 {
		t.Errorf("DBMaxRows = %d, want 250", cfg.DBMaxRows)
	}
	if want := []string{"public", "api"}; !slices.Equal(cfg.DBSchemas, want) {
		t.Errorf("DBSchemas = %v, want %v", cfg.DBSchemas, want)
	}
	// A file with defaults applied must still validate.
	if cfg.DBPool != 10 {
		t.Errorf("DBPool = %d, want the default 10", cfg.DBPool)
	}
}

func TestLoadRejectsMissingFileAndBadValues(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.conf")); err == nil {
		t.Error("a missing config file should be an error")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(path, []byte("db-pool = 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("db-pool = 0 should be rejected")
	}
}

func TestLoadWithNoPathUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.ServerPort != 3000 || len(cfg.DBSchemas) != 1 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestSearchPathOrdersExposedBeforeExtra(t *testing.T) {
	cfg := Default()
	cfg.DBSchemas = []string{"api", "public"}
	cfg.DBExtraSearchPath = []string{"extensions"}
	want := []string{"api", "public", "extensions"}
	if got := cfg.SearchPath(); !slices.Equal(got, want) {
		t.Errorf("SearchPath() = %v, want %v", got, want)
	}
	// The result must not alias the configuration.
	got := cfg.SearchPath()
	got[0] = "mutated"
	if cfg.DBSchemas[0] != "api" {
		t.Error("SearchPath must return a copy")
	}
}

func TestParseInt64Param(t *testing.T) {
	cfg := Default()
	if err := cfg.apply("db-max-rows", "1234"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cfg.DBMaxRows != 1234 {
		t.Errorf("DBMaxRows = %d", cfg.DBMaxRows)
	}
	if err := cfg.apply("db-max-rows", "not-a-number"); err == nil {
		t.Error("a non-numeric db-max-rows should be rejected")
	}
}

func TestIsQualifiedName(t *testing.T) {
	valid := []string{"f", "public.f", "_f", "f$1", "a1.b2"}
	for _, name := range valid {
		if !isQualifiedName(name) {
			t.Errorf("isQualifiedName(%q) = false, want true", name)
		}
	}
	invalid := []string{"", ".", "a.", ".b", "a.b.c", "a-b", "a b", "f()", "a;b", `"a"`}
	for _, name := range invalid {
		if isQualifiedName(name) {
			t.Errorf("isQualifiedName(%q) = true, want false", name)
		}
	}
}

func TestDurationsParseAsSeconds(t *testing.T) {
	cfg := Default()
	for param, value := range map[string]string{
		"db-pool-acquisition-timeout": "5",
		"db-pool-max-idletime":        "15",
		"db-pool-max-lifetime":        "60",
	} {
		if err := cfg.apply(param, value); err != nil {
			t.Fatalf("apply(%s): %v", param, err)
		}
	}
	if cfg.DBPoolAcquisitionAfter != 5*time.Second {
		t.Errorf("acquisition timeout = %v, want 5s", cfg.DBPoolAcquisitionAfter)
	}
	if cfg.DBPoolMaxIdleTime != 15*time.Second {
		t.Errorf("max idletime = %v, want 15s", cfg.DBPoolMaxIdleTime)
	}
	if cfg.DBPoolMaxLifetime != 60*time.Second {
		t.Errorf("max lifetime = %v, want 60s", cfg.DBPoolMaxLifetime)
	}
	if err := cfg.apply("db-pool-max-lifetime", "-1"); err == nil {
		t.Error("a negative duration should be rejected")
	}

	// db-pool-timeout is a backwards-compatible alias for max idletime.
	alias := Default()
	if err := alias.apply("db-pool-timeout", "7"); err != nil {
		t.Fatalf("apply alias: %v", err)
	}
	if alias.DBPoolMaxIdleTime != 7*time.Second {
		t.Errorf("alias max idletime = %v, want 7s", alias.DBPoolMaxIdleTime)
	}
}

func TestProxyURIAndAggregatesParams(t *testing.T) {
	cfg := Default()
	if err := cfg.apply("openapi-server-proxy-uri", "https://api.example.com/v1"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cfg.OpenAPIServerProxyURI != "https://api.example.com/v1" {
		t.Errorf("proxy URI = %q", cfg.OpenAPIServerProxyURI)
	}
	if err := cfg.apply("db-aggregates-enabled", "true"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !cfg.DBAggregatesEnabled {
		t.Error("DBAggregatesEnabled = false")
	}
}
