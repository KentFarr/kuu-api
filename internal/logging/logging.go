package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
)

// Init configures slog as the process-wide default and rewires the stdlib
// log package through it. Anything still calling log.Printf will land in
// the same JSON stream as native slog calls, so we don't have to touch
// every call site at once.
func Init() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))
}

// NewRequestID returns a 16-byte hex token. We use crypto/rand rather than
// monotonic counters or hostnames so the ID is opaque and unguessable —
// useful when echoing it back to the client.
func NewRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failures are catastrophic; degrade gracefully rather
		// than panic in a request-handling path.
		return "norand"
	}
	return hex.EncodeToString(buf[:])
}

// WithRequestID returns a context that carries the given request ID and a
// logger pre-bound to it. FromContext on the returned ctx returns that
// bound logger so call sites don't have to re-attach the ID on every line.
func WithRequestID(ctx context.Context, id string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyRequestID, id)
	logger := slog.Default().With("request_id", id)
	return context.WithValue(ctx, ctxKeyLogger, logger)
}

// RequestIDFrom returns the request ID stored in ctx, or "" if absent.
// Background workers that didn't inherit a request context will see "".
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// FromContext returns the logger bound to ctx, or slog.Default() when no
// request scope is present (background workers, init code).
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
