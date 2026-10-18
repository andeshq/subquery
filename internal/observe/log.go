// Package observe provides logging and request instrumentation.
package observe

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/andeshq/subquery/internal/config"
)

// NewLogger returns a slog logger honoring PostgREST's log levels:
// crit, error, warn, info, debug.
func NewLogger(level config.LogLevel) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slogLevel(level),
	}))
}

func slogLevel(level config.LogLevel) slog.Level {
	switch level {
	case config.LogCrit:
		// slog has no "crit"; surface only the most severe records.
		return slog.LevelError + 4
	case config.LogError:
		return slog.LevelError
	case config.LogWarn:
		return slog.LevelWarn
	case config.LogInfo:
		return slog.LevelInfo
	case config.LogDebug:
		return slog.LevelDebug
	default:
		return slog.LevelError
	}
}

// Request is one completed HTTP request, from which the access log is built.
type Request struct {
	Method       string
	Path         string
	Status       int
	Duration     time.Duration
	BytesWritten int
	// RequestID is the trace id: the configured trace header when the client
	// sent one, otherwise a generated id.
	RequestID string
	// Role is the authenticated role, empty when the request never resolved one.
	Role string
	// ErrorCode is the PGRST or SQLSTATE code of the response, when it was an
	// error.
	ErrorCode string
	// Error is the underlying failure, when it did not become a client error.
	Error error
}

// LogRequest writes one access-log line, at a level derived from the response
// status, matching PostgREST's log-level = error/warn/info behavior.
func LogRequest(ctx context.Context, log *slog.Logger, req Request) {
	attrs := []any{
		"method", req.Method,
		"path", req.Path,
		"status", req.Status,
		"duration", req.Duration.String(),
		"bytes", req.BytesWritten,
	}
	if req.RequestID != "" {
		attrs = append(attrs, "request_id", req.RequestID)
	}
	if req.Role != "" {
		attrs = append(attrs, "role", req.Role)
	}
	if req.ErrorCode != "" {
		attrs = append(attrs, "error_code", req.ErrorCode)
	}
	if req.Error != nil {
		attrs = append(attrs, "error", req.Error)
	}
	switch {
	case req.Status >= 500:
		log.ErrorContext(ctx, "request failed", attrs...)
	case req.Status >= 400:
		log.WarnContext(ctx, "request failed", attrs...)
	default:
		log.InfoContext(ctx, "request", attrs...)
	}
}
