// Package config loads Subquery configuration from PostgREST-style config
// files and PGRST_* environment variables.
//
// The reference surface is PostgREST 16.4. Environment variables override file
// values, matching PostgREST's documented precedence.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TxEnd controls how a request transaction is terminated.
type TxEnd string

const (
	TxCommit                TxEnd = "commit"
	TxCommitAllowOverride   TxEnd = "commit-allow-override"
	TxRollback              TxEnd = "rollback"
	TxRollbackAllowOverride TxEnd = "rollback-allow-override"
)

// LogLevel mirrors PostgREST's log-level values.
type LogLevel string

const (
	LogCrit  LogLevel = "crit"
	LogError LogLevel = "error"
	LogWarn  LogLevel = "warn"
	LogInfo  LogLevel = "info"
	LogDebug LogLevel = "debug"
)

// ErrorVerbosity controls how much detail client-facing errors carry.
type ErrorVerbosity string

const (
	ErrorVerbose ErrorVerbosity = "verbose"
	ErrorMinimal ErrorVerbosity = "minimal"
)

// OpenAPIMode controls whether and how the API root is documented.
type OpenAPIMode string

const (
	OpenAPIFollowPrivileges OpenAPIMode = "follow-privileges"
	OpenAPIIgnorePrivileges OpenAPIMode = "ignore-privileges"
	OpenAPIDisabled         OpenAPIMode = "disabled"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Database
	DBURI                  string
	DBSchemas              []string
	DBAnonRole             string
	DBExtraSearchPath      []string
	DBPreRequest           string
	DBTxEnd                TxEnd
	DBPreparedStatements   bool
	DBChannel              string
	DBChannelEnabled       bool
	DBPool                 int
	DBPoolAcquisitionAfter time.Duration
	DBPoolMaxIdleTime      time.Duration
	DBPoolMaxLifetime      time.Duration
	// DBRequestTimeout bounds one request's transaction, including its
	// statement. Zero disables the bound.
	DBRequestTimeout    time.Duration
	DBMaxRows           int64
	DBAggregatesEnabled bool
	DBPlanEnabled       bool
	DBConfig            bool
	DBPreConfig         string

	// Authentication
	JWTSecret          string
	JWTSecretIsBase64  bool
	JWTAud             string
	JWTRoleClaimKey    string
	JWTCacheMaxEntries int

	// HTTP server
	ServerHost               string
	ServerPort               int
	ServerCORSAllowedOrigins []string
	ServerTimingEnabled      bool
	ServerTraceHeader        string

	// Behavior
	LogLevel              LogLevel
	LogQuery              bool
	ClientErrorVerbosity  ErrorVerbosity
	OpenAPIMode           OpenAPIMode
	OpenAPIServerProxyURI string

	// AppSettings is exposed to Postgres as app.settings.* per request.
	AppSettings map[string]string
}

// Default returns PostgREST's documented defaults for the parameters Subquery
// implements.
func Default() *Config {
	return &Config{
		DBURI:                  "postgresql://",
		DBSchemas:              []string{"public"},
		DBExtraSearchPath:      []string{"public"},
		DBTxEnd:                TxCommit,
		DBPreparedStatements:   true,
		DBChannel:              "pgrst",
		DBChannelEnabled:       true,
		DBPool:                 10,
		DBPoolAcquisitionAfter: 10 * time.Second,
		DBPoolMaxIdleTime:      30 * time.Second,
		DBPoolMaxLifetime:      30 * time.Minute,
		DBRequestTimeout:       30 * time.Second,
		DBConfig:               true,

		JWTRoleClaimKey:    "$.role",
		JWTCacheMaxEntries: 1000,

		ServerHost: "!4",
		ServerPort: 3000,

		LogLevel:             LogError,
		ClientErrorVerbosity: ErrorVerbose,
		OpenAPIMode:          OpenAPIFollowPrivileges,

		AppSettings: make(map[string]string),
	}
}

// Load reads path (optional; may be empty) and then applies PGRST_*
// environment variables from the current process.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config file: %w", err)
		}
		if err := parseFile(cfg, data, os.Getenv); err != nil {
			return nil, fmt.Errorf("parse config file %s: %w", path, err)
		}
	}
	if err := applyEnv(cfg, os.Environ()); err != nil {
		return nil, err
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// parseFile reads PostgREST's `key = value` format. Lines beginning with '#'
// are comments. Values may be single- or double-quoted. In file configuration
// only, "$$" is an escape for a literal "$" (used by jwt-role-claim-key) and
// "$(NAME)" expands an environment variable.
func parseFile(cfg *Config, data []byte, getenv func(string) string) error {
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, raw, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("line %d: expected key = value", i+1)
		}
		name = strings.TrimSpace(name)
		value := unquote(strings.TrimSpace(raw))
		value = expandEnv(value, getenv)
		value = strings.ReplaceAll(value, "$$", "$")
		if err := cfg.apply(name, value); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
	}
	return nil
}

func unquote(s string) string {
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			if v, err := strconv.Unquote(s); err == nil {
				return v
			}
			return s[1 : len(s)-1]
		}
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func expandEnv(value string, getenv func(string) string) string {
	if !strings.Contains(value, "$(") {
		return value
	}
	var b strings.Builder
	for {
		open := strings.Index(value, "$(")
		if open < 0 {
			b.WriteString(value)
			return b.String()
		}
		end := strings.IndexByte(value[open:], ')')
		if end < 0 {
			b.WriteString(value)
			return b.String()
		}
		end += open
		b.WriteString(value[:open])
		b.WriteString(getenv(value[open+2 : end]))
		value = value[end+1:]
	}
}

// applyEnv applies every PGRST_* variable, translating names back to their
// dashed config form. PGRST_APP_SETTINGS_* is collected separately.
func applyEnv(cfg *Config, environ []string) error {
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(key, "PGRST_") {
			continue
		}
		if key == "PGRST_APP_SETTINGS" || strings.HasPrefix(key, "PGRST_APP_SETTINGS_") {
			name := strings.ToLower(strings.TrimPrefix(key, "PGRST_APP_SETTINGS_"))
			if name != "" {
				cfg.AppSettings[name] = value
			}
			continue
		}
		name := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, "PGRST_"), "_", "-"))
		if err := cfg.apply(name, value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

func (c *Config) apply(name, value string) error {
	switch name {
	case "db-uri":
		c.DBURI = value
	case "db-schemas", "db-schema":
		c.DBSchemas = splitList(value)
	case "db-anon-role":
		c.DBAnonRole = value
	case "db-extra-search-path":
		c.DBExtraSearchPath = splitList(value)
	case "db-pre-request", "pre-request":
		c.DBPreRequest = value
	case "db-tx-end":
		c.DBTxEnd = TxEnd(value)
	case "db-prepared-statements":
		return parseBool(name, value, &c.DBPreparedStatements)
	case "db-channel":
		c.DBChannel = value
	case "db-channel-enabled":
		return parseBool(name, value, &c.DBChannelEnabled)
	case "db-pool":
		return parseInt(name, value, &c.DBPool)
	case "db-pool-acquisition-timeout":
		return parseSeconds(name, value, &c.DBPoolAcquisitionAfter)
	case "db-pool-max-idletime", "db-pool-timeout":
		return parseSeconds(name, value, &c.DBPoolMaxIdleTime)
	case "db-pool-max-lifetime":
		return parseSeconds(name, value, &c.DBPoolMaxLifetime)
	case "db-request-timeout":
		return parseSeconds(name, value, &c.DBRequestTimeout)
	case "db-max-rows", "max-rows":
		return parseInt64(name, value, &c.DBMaxRows)
	case "db-aggregates-enabled":
		return parseBool(name, value, &c.DBAggregatesEnabled)
	case "db-plan-enabled":
		return parseBool(name, value, &c.DBPlanEnabled)
	case "db-config":
		return parseBool(name, value, &c.DBConfig)
	case "db-pre-config":
		c.DBPreConfig = value

	case "jwt-secret":
		c.JWTSecret = value
	case "jwt-secret-is-base64":
		return parseBool(name, value, &c.JWTSecretIsBase64)
	case "jwt-aud":
		c.JWTAud = value
	case "jwt-role-claim-key", "role-claim-key":
		c.JWTRoleClaimKey = value
	case "jwt-cache-max-entries":
		return parseInt(name, value, &c.JWTCacheMaxEntries)

	case "server-host":
		c.ServerHost = value
	case "server-port":
		return parseInt(name, value, &c.ServerPort)
	case "server-cors-allowed-origins":
		c.ServerCORSAllowedOrigins = splitList(value)
	case "server-timing-enabled":
		return parseBool(name, value, &c.ServerTimingEnabled)
	case "server-trace-header":
		c.ServerTraceHeader = value

	case "log-level":
		c.LogLevel = LogLevel(value)
	case "log-query":
		return parseBool(name, value, &c.LogQuery)
	case "client-error-verbosity":
		c.ClientErrorVerbosity = ErrorVerbosity(value)
	case "openapi-mode":
		c.OpenAPIMode = OpenAPIMode(value)
	case "openapi-server-proxy-uri":
		c.OpenAPIServerProxyURI = value

	default:
		return fmt.Errorf("unknown configuration parameter %q", name)
	}
	return nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBool(name, value string, dst *bool) error {
	v, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s: %q is not a boolean", name, value)
	}
	*dst = v
	return nil
}

func parseInt(name, value string, dst *int) error {
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s: %q is not an integer", name, value)
	}
	*dst = v
	return nil
}

func parseInt64(name, value string, dst *int64) error {
	v, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %q is not an integer", name, value)
	}
	*dst = v
	return nil
}

func parseSeconds(name, value string, dst *time.Duration) error {
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || v < 0 {
		return fmt.Errorf("%s: %q is not a non-negative number of seconds", name, value)
	}
	*dst = time.Duration(v) * time.Second
	return nil
}

func (c *Config) normalize() error {
	if len(c.DBSchemas) == 0 {
		return errors.New("db-schemas must not be empty")
	}
	if c.DBPool < 1 {
		return fmt.Errorf("db-pool must be at least 1, got %d", c.DBPool)
	}
	if c.ServerPort < 0 || c.ServerPort > 65535 {
		return fmt.Errorf("server-port out of range: %d", c.ServerPort)
	}
	switch c.DBTxEnd {
	case TxCommit, TxCommitAllowOverride, TxRollback, TxRollbackAllowOverride:
	default:
		return fmt.Errorf("db-tx-end: unknown value %q", c.DBTxEnd)
	}
	switch c.LogLevel {
	case LogCrit, LogError, LogWarn, LogInfo, LogDebug:
	default:
		return fmt.Errorf("log-level: unknown value %q", c.LogLevel)
	}
	switch c.ClientErrorVerbosity {
	case ErrorVerbose, ErrorMinimal:
	default:
		return fmt.Errorf("client-error-verbosity: unknown value %q", c.ClientErrorVerbosity)
	}
	switch c.OpenAPIMode {
	case OpenAPIFollowPrivileges, OpenAPIIgnorePrivileges, OpenAPIDisabled:
	default:
		return fmt.Errorf("openapi-mode: unknown value %q", c.OpenAPIMode)
	}
	if c.JWTRoleClaimKey == "" {
		return errors.New("jwt-role-claim-key must not be empty")
	}
	if c.DBPreRequest != "" && !isQualifiedName(c.DBPreRequest) {
		return fmt.Errorf("db-pre-request must be a schema-qualified function name, got %q", c.DBPreRequest)
	}
	return c.validateJWTSecret()
}

// isQualifiedName accepts `name` or `schema.name`.
func isQualifiedName(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c != '_' && c != '$' && !isASCIILetter(c) && !isASCIIDigit(c) {
				return false
			}
		}
	}
	return true
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isASCIIDigit(c byte) bool  { return c >= '0' && c <= '9' }

// validateJWTSecret accepts the same shapes as PostgREST's jwt-secret, plus a
// remote JWKS URL: a plain HMAC passphrase, "@file", a literal JWK/JWKS, or an
// http(s) URL.
func (c *Config) validateJWTSecret() error {
	s := strings.TrimSpace(c.JWTSecret)
	if s == "" {
		return nil
	}
	if isURL(s) || strings.HasPrefix(s, "@") || c.JWTSecretIsBase64 || isJSONObjectOrArray(s) {
		return nil
	}
	if len(s) < 32 {
		return fmt.Errorf("jwt-secret must be at least 32 characters, or a JWK/JWKS/@file/URL")
	}
	return nil
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func isJSONObjectOrArray(s string) bool {
	if s == "" {
		return false
	}
	return (s[0] == '{' && s[len(s)-1] == '}') || (s[0] == '[' && s[len(s)-1] == ']')
}

// ListenAddr returns the host:port passed to net.Listen, translating
// PostgREST's reserved host names.
func (c *Config) ListenAddr() string {
	host := c.ServerHost
	switch host {
	case "*":
		host = ""
	case "*4", "!4":
		host = "0.0.0.0"
	case "*6", "!6":
		host = "::"
	}
	return net.JoinHostPort(host, strconv.Itoa(c.ServerPort))
}

// SearchPath is the per-request search_path, exposed schemas first.
func (c *Config) SearchPath() []string {
	return slices.Concat(c.DBSchemas, c.DBExtraSearchPath)
}

// JWTSecretKind reports how jwt-secret should be interpreted.
type JWTSecretKind int

const (
	JWTSecretNone JWTSecretKind = iota
	JWTSecretPlain
	JWTSecretBase64
	JWTSecretFile
	JWTSecretJWKS
	JWTSecretURL
)

// JWTSecretKind classifies the configured jwt-secret.
func (c *Config) JWTSecretKind() JWTSecretKind {
	s := strings.TrimSpace(c.JWTSecret)
	switch {
	case s == "":
		return JWTSecretNone
	case isURL(s):
		return JWTSecretURL
	case strings.HasPrefix(s, "@"):
		return JWTSecretFile
	case isJSONObjectOrArray(s):
		return JWTSecretJWKS
	case c.JWTSecretIsBase64:
		return JWTSecretBase64
	default:
		return JWTSecretPlain
	}
}
