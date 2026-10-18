package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/auth"
	"github.com/andeshq/subquery/internal/config"
)

// bufferLogger returns a logger that appends to buffer, plus the buffer.
func bufferLogger() (*slog.Logger, *bytes.Buffer) {
	var buffer bytes.Buffer
	return slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})), &buffer
}

// TestAccessLogFields asserts the access log records the status, error code,
// duration, and a generated request id.
func TestAccessLogFields(t *testing.T) {
	log, buffer := bufferLogger()

	cfg := config.Default()
	cfg.DBAnonRole = "" // anonymous access disabled: a 401 that never needs the pool
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	s := New(Options{Config: cfg, Logger: log, Verifier: verifier, Store: fakeStore{}})

	if rec := do(t, s, http.MethodGet, "/users", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	line := buffer.String()
	for _, want := range []string{"status=401", "error_code=PGRST302", "duration=", "request_id="} {
		if !strings.Contains(line, want) {
			t.Errorf("access log missing %q: %q", want, line)
		}
	}
}

// TestAccessLogTraceHeader asserts a configured trace header becomes the
// request id.
func TestAccessLogTraceHeader(t *testing.T) {
	log, buffer := bufferLogger()

	cfg := config.Default()
	cfg.DBAnonRole = ""
	cfg.ServerTraceHeader = "X-Trace-Id"
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	s := New(Options{Config: cfg, Logger: log, Verifier: verifier, Store: fakeStore{}})

	do(t, s, http.MethodGet, "/users", map[string]string{"X-Trace-Id": "abc-123"})
	if !strings.Contains(buffer.String(), "request_id=abc-123") {
		t.Errorf("trace header was not used as the request id: %q", buffer.String())
	}
}

// TestLogQuery asserts log-query logs the generated SQL with placeholders only,
// never a bound value.
func TestLogQuery(t *testing.T) {
	log, buffer := bufferLogger()
	s := newLiveServerLogging(t, func(c *config.Config) { c.LogQuery = true }, log)

	if rec := get(t, s, "/todos?task=eq.SECRETVALUE&limit=1", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	line := buffer.String()
	if !strings.Contains(line, "msg=query") || !strings.Contains(line, "select") {
		t.Errorf("query was not logged: %q", line)
	}
	if !strings.Contains(line, "$1") {
		t.Errorf("logged query lost its placeholders: %q", line)
	}
	if strings.Contains(line, "SECRETVALUE") {
		t.Errorf("logged query leaked a bound value: %q", line)
	}
}

// TestReadyRequiresSchemaCache asserts /_ready reports unready until the schema
// snapshot is loaded, not merely until the pool answers.
func TestReadyRequiresSchemaCache(t *testing.T) {
	if rec := get(t, newLiveServer(t), "/_ready", nil); rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", rec.Code)
	}
	if rec := get(t, newLiveServerNoSchema(t), "/_ready", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unready status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
}
