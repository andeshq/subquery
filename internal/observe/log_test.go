package observe

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andeshq/subquery/internal/config"
)

func TestNewLoggerHonorsLevel(t *testing.T) {
	tests := []struct {
		level   config.LogLevel
		enabled bool // whether Error records pass at this level
	}{
		{config.LogCrit, false},
		{config.LogError, true},
		{config.LogWarn, true},
		{config.LogInfo, true},
		{config.LogDebug, true},
	}
	for _, tc := range tests {
		logger := NewLogger(tc.level)
		if got := logger.Enabled(t.Context(), slog.LevelError); got != tc.enabled {
			t.Errorf("NewLogger(%q).Enabled(Error) = %v, want %v", tc.level, got, tc.enabled)
		}
	}

	if !NewLogger(config.LogDebug).Enabled(t.Context(), slog.LevelDebug) {
		t.Error("debug should be enabled at log-level=debug")
	}
	if NewLogger(config.LogInfo).Enabled(t.Context(), slog.LevelDebug) {
		t.Error("debug should be disabled at log-level=info")
	}
}

func TestSlogLevelMapping(t *testing.T) {
	if slogLevel(config.LogCrit) <= slogLevel(config.LogError) {
		t.Error("crit must be more severe than error")
	}
	if slogLevel(config.LogDebug) >= slogLevel(config.LogInfo) {
		t.Error("debug must be less severe than info")
	}
	// An unrecognized level falls back to error rather than silencing logs.
	if got := slogLevel(config.LogLevel("nonsense")); got != slog.LevelError {
		t.Errorf("unknown level mapped to %v, want error", got)
	}
}

// logLine renders one access-log record and returns it.
func logLine(t *testing.T, req Request) string {
	t.Helper()
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	LogRequest(t.Context(), logger, req)
	return buffer.String()
}

func TestLogRequestLevels(t *testing.T) {
	tests := []struct {
		status int
		level  string
	}{
		{200, "level=INFO"},
		{404, "level=WARN"},
		{500, "level=ERROR"},
	}
	for _, tc := range tests {
		line := logLine(t, Request{Method: "GET", Path: "/things", Status: tc.status})
		if !strings.Contains(line, tc.level) {
			t.Errorf("status %d logged %q, want %s", tc.status, line, tc.level)
		}
		for _, want := range []string{"method=GET", "path=/things", "status="} {
			if !strings.Contains(line, want) {
				t.Errorf("status %d log line missing %q: %q", tc.status, want, line)
			}
		}
	}
}

func TestLogRequestCarriesContext(t *testing.T) {
	line := logLine(t, Request{
		Method:       "POST",
		Path:         "/things",
		Status:       409,
		Duration:     1500 * time.Microsecond,
		BytesWritten: 42,
		RequestID:    "trace-123",
		Role:         "webuser",
		ErrorCode:    "23505",
		Error:        errors.New("boom"),
	})
	for _, want := range []string{
		"duration=1.5ms", "bytes=42", "request_id=trace-123",
		"role=webuser", "error_code=23505", "boom",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %q", want, line)
		}
	}
}

// TestLogRequestOmitsEmptyFields keeps the common anonymous success line clean.
func TestLogRequestOmitsEmptyFields(t *testing.T) {
	line := logLine(t, Request{Method: "GET", Path: "/", Status: 200})
	for _, unwanted := range []string{"request_id=", "role=", "error_code=", "error="} {
		if strings.Contains(line, unwanted) {
			t.Errorf("log line carried %q with no value: %q", unwanted, line)
		}
	}
}
