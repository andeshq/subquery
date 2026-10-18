package config

import (
	"slices"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg := Default()
	if cfg.ServerPort != 3000 {
		t.Errorf("ServerPort = %d, want 3000", cfg.ServerPort)
	}
	if cfg.DBPool != 10 {
		t.Errorf("DBPool = %d, want 10", cfg.DBPool)
	}
	if cfg.DBTxEnd != TxCommit {
		t.Errorf("DBTxEnd = %q, want commit", cfg.DBTxEnd)
	}
	if got := cfg.JWTRoleClaimKey; got != "$.role" {
		t.Errorf("JWTRoleClaimKey = %q, want $.role", got)
	}
	if !cfg.DBPreparedStatements {
		t.Error("DBPreparedStatements = false, want true")
	}
	if cfg.DBChannel != "pgrst" {
		t.Errorf("DBChannel = %q, want pgrst", cfg.DBChannel)
	}
}

func TestParseFile(t *testing.T) {
	data := []byte(`
# connection
db-uri       = "postgres://authenticator:secret@localhost:5432/app"
db-anon-role = 'anon'
db-schemas   = "public, api"
db-pool      = 20
db-prepared-statements = false
jwt-role-claim-key = "$$.postgrest.roles[1]"
`)
	cfg := Default()
	if err := parseFile(cfg, data, func(string) string { return "" }); err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if cfg.DBURI != "postgres://authenticator:secret@localhost:5432/app" {
		t.Errorf("DBURI = %q", cfg.DBURI)
	}
	if cfg.DBAnonRole != "anon" {
		t.Errorf("DBAnonRole = %q", cfg.DBAnonRole)
	}
	if want := []string{"public", "api"}; !slices.Equal(cfg.DBSchemas, want) {
		t.Errorf("DBSchemas = %v, want %v", cfg.DBSchemas, want)
	}
	if cfg.DBPool != 20 {
		t.Errorf("DBPool = %d, want 20", cfg.DBPool)
	}
	if cfg.DBPreparedStatements {
		t.Error("DBPreparedStatements = true, want false")
	}
	if cfg.JWTRoleClaimKey != "$.postgrest.roles[1]" {
		t.Errorf("JWTRoleClaimKey = %q ($$ escape not applied)", cfg.JWTRoleClaimKey)
	}
}

func TestParseFileExpandsEnv(t *testing.T) {
	cfg := Default()
	env := map[string]string{"PGPASSWORD": "hunter2"}
	if err := parseFile(cfg, []byte(`db-uri = "postgres://user:$(PGPASSWORD)@db/app"`), func(k string) string { return env[k] }); err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if cfg.DBURI != "postgres://user:hunter2@db/app" {
		t.Errorf("DBURI = %q", cfg.DBURI)
	}
}

func TestApplyEnvOverridesFile(t *testing.T) {
	cfg := Default()
	if err := parseFile(cfg, []byte("server-port = 3001\nlog-level = \"warn\"\n"), func(string) string { return "" }); err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	environ := []string{
		"PGRST_SERVER_PORT=4000",
		"PATH=/usr/bin",
		"PGRST_APP_SETTINGS_MY_KEY=abc",
	}
	if err := applyEnv(cfg, environ); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if cfg.ServerPort != 4000 {
		t.Errorf("ServerPort = %d, want env override 4000", cfg.ServerPort)
	}
	if cfg.LogLevel != LogWarn {
		t.Errorf("LogLevel = %q, want warn from file", cfg.LogLevel)
	}
	if got := cfg.AppSettings["my_key"]; got != "abc" {
		t.Errorf("AppSettings[my_key] = %q, want abc", got)
	}
}

func TestUnknownParameterRejected(t *testing.T) {
	cfg := Default()
	if err := cfg.apply("not-a-param", "x"); err == nil {
		t.Fatal("expected an error for an unknown parameter")
	}
}

func TestJWTSecretKind(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		b64    bool
		want   JWTSecretKind
	}{
		{"none", "", false, JWTSecretNone},
		{"plain", "reallyreallyreallyreallyverysafe", false, JWTSecretPlain},
		{"remote", "https://issuer.example/.well-known/jwks.json", false, JWTSecretURL},
		{"file", "@/etc/jwks.json", false, JWTSecretFile},
		{"jwks", `{"keys":[{"kty":"oct","k":"AAAA"}]}`, false, JWTSecretJWKS},
		{"base64", "cmVhbGx5cmVhbGx5cmVhbGx5dmVyeXNhZmU=", true, JWTSecretBase64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.JWTSecret = tc.secret
			cfg.JWTSecretIsBase64 = tc.b64
			if got := cfg.JWTSecretKind(); got != tc.want {
				t.Errorf("JWTSecretKind() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestValidateRejectsShortPlainSecret(t *testing.T) {
	cfg := Default()
	cfg.JWTSecret = "tooshort"
	if err := cfg.normalize(); err == nil {
		t.Fatal("expected short jwt-secret to be rejected")
	}
}

func TestValidateRejectsBadEnums(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"log-level", func(c *Config) { c.LogLevel = "loud" }},
		{"tx-end", func(c *Config) { c.DBTxEnd = "maybe" }},
		{"verbosity", func(c *Config) { c.ClientErrorVerbosity = "loud" }},
		{"openapi-mode", func(c *Config) { c.OpenAPIMode = "sometimes" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			if err := cfg.normalize(); err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
		})
	}
}

func TestDurations(t *testing.T) {
	cfg := Default()
	if err := cfg.apply("db-pool-max-lifetime", "600"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cfg.DBPoolMaxLifetime != 600*time.Second {
		t.Errorf("DBPoolMaxLifetime = %v, want 10m", cfg.DBPoolMaxLifetime)
	}
}

func TestPreRequestValidation(t *testing.T) {
	cfg := Default()
	cfg.DBPreRequest = "postgrest.check_request"
	if err := cfg.normalize(); err != nil {
		t.Errorf("qualified name rejected: %v", err)
	}
	for _, bad := range []string{"check_request(); drop table x", "a.b.c", "public.", ".x", "a-b"} {
		cfg := Default()
		cfg.DBPreRequest = bad
		if err := cfg.normalize(); err == nil {
			t.Errorf("db-pre-request %q was accepted", bad)
		}
	}
}

func TestListenAddr(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"!4", "0.0.0.0:3000"},
		{"*", ":3000"},
		{"127.0.0.1", "127.0.0.1:3000"},
		{"!6", "[::]:3000"},
	}
	for _, tc := range tests {
		cfg := Default()
		cfg.ServerHost = tc.host
		if got := cfg.ListenAddr(); got != tc.want {
			t.Errorf("ListenAddr(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}
