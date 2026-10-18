package config

import (
	"testing"
	"time"
)

// longSecret is a passphrase long enough to pass validation.
const longSecret = "reallyreallyreallyreallyverysafe!"

// inertParameters are accepted for PostgREST configuration compatibility but
// currently have no effect. Listing them here keeps the gap explicit: a
// parameter that silently does nothing is worse than one that is missing,
// because a deployment can appear configured when it is not.
//
// When one of these is implemented, move it out of this list and give it a test
// in the suite that proves it changes behavior.
var inertParameters = map[string]string{
	"db-config":                   "true",
	"db-plan-enabled":             "true",
	"db-pre-config":               "postgrest.pre_config",
	"jwt-cache-max-entries":       "1000",
	"server-cors-allowed-origins": "https://example.com",
	"server-timing-enabled":       "true",
}

func TestInertParametersAreStillAccepted(t *testing.T) {
	for name, value := range inertParameters {
		cfg := Default()
		if err := cfg.apply(name, value); err != nil {
			t.Errorf("apply(%q) rejected a parameter PostgREST accepts: %v", name, err)
		}
	}
}

// TestImplementedParametersAreNotInert asserts the two lists cannot overlap:
// a parameter must not be both documented as inert and actually honoured.
func TestImplementedParametersAreNotInert(t *testing.T) {
	implemented := []struct {
		name   string
		value  string
		change func(*Config) bool
	}{
		{"db-uri", "postgres://x", func(c *Config) bool { return c.DBURI == "postgres://x" }},
		{"db-schemas", "a,b", func(c *Config) bool { return len(c.DBSchemas) == 2 }},
		{"db-anon-role", "anon", func(c *Config) bool { return c.DBAnonRole == "anon" }},
		{"db-extra-search-path", "ext", func(c *Config) bool { return len(c.DBExtraSearchPath) == 1 }},
		{"db-pre-request", "s.f", func(c *Config) bool { return c.DBPreRequest == "s.f" }},
		{"db-tx-end", "rollback", func(c *Config) bool { return c.DBTxEnd == TxRollback }},
		{"db-prepared-statements", "false", func(c *Config) bool { return !c.DBPreparedStatements }},
		{"db-channel", "other", func(c *Config) bool { return c.DBChannel == "other" }},
		{"db-channel-enabled", "false", func(c *Config) bool { return !c.DBChannelEnabled }},
		{"db-pool", "3", func(c *Config) bool { return c.DBPool == 3 }},
		{"db-request-timeout", "5", func(c *Config) bool { return c.DBRequestTimeout == 5*time.Second }},
		{"db-max-rows", "9", func(c *Config) bool { return c.DBMaxRows == 9 }},
		{"db-aggregates-enabled", "true", func(c *Config) bool { return c.DBAggregatesEnabled }},
		{"jwt-secret", longSecret, func(c *Config) bool { return c.JWTSecret == longSecret }},
		{"jwt-aud", "api", func(c *Config) bool { return c.JWTAud == "api" }},
		{"jwt-role-claim-key", "$.r", func(c *Config) bool { return c.JWTRoleClaimKey == "$.r" }},
		{"server-host", "127.0.0.1", func(c *Config) bool { return c.ServerHost == "127.0.0.1" }},
		{"server-port", "8080", func(c *Config) bool { return c.ServerPort == 8080 }},
		{"server-trace-header", "X-Trace-Id", func(c *Config) bool { return c.ServerTraceHeader == "X-Trace-Id" }},
		{"log-level", "debug", func(c *Config) bool { return c.LogLevel == LogDebug }},
		{"log-query", "true", func(c *Config) bool { return c.LogQuery }},
		{"client-error-verbosity", "minimal", func(c *Config) bool { return c.ClientErrorVerbosity == ErrorMinimal }},
		{"openapi-mode", "disabled", func(c *Config) bool { return c.OpenAPIMode == OpenAPIDisabled }},
		{"openapi-server-proxy-uri", "https://x", func(c *Config) bool { return c.OpenAPIServerProxyURI == "https://x" }},
	}

	inert := make(map[string]bool, len(inertParameters))
	for name := range inertParameters {
		inert[name] = true
	}

	for _, tc := range implemented {
		if inert[tc.name] {
			t.Errorf("%s is documented as inert but is also implemented", tc.name)
		}
		cfg := Default()
		if err := cfg.apply(tc.name, tc.value); err != nil {
			t.Errorf("apply(%q): %v", tc.name, err)
			continue
		}
		if !tc.change(cfg) {
			t.Errorf("apply(%q, %q) did not change the configuration", tc.name, tc.value)
		}
	}
}
