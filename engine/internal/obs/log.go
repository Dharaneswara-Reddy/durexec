package obs

import (
	"context"
	"log/slog"
	"os"

	"github.com/dharaneswar339/durexec/internal/core"
)

// requestIDKey is the context key for the request id. It has an unexported
// type so no other package can collide with it, accidentally or otherwise.
type requestIDKey struct{}

// RequestIDHeader is the HTTP header the API reads an inbound request id from
// and echoes back on the response.
const RequestIDHeader = "X-Request-Id"

// WithRequestID returns a context carrying id.
//
// The id is put in the context rather than passed as an argument because it
// has to survive a journey nobody wants to thread a parameter through: an HTTP
// handler calls the dispatcher, which loads history, which commits a decision,
// which enqueues a task. Any of those can fail, and the only way to line the
// resulting log lines up afterwards is a value that every layer already has.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID reads the id back, or "" when there is none. A missing id is
// normal — background workers have no request behind them — so this never
// returns an error.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Logger is a thin wrapper over log/slog that folds the request id, and the
// engine's own identifiers, into every line automatically.
//
// It wraps rather than aliases slog so that callers cannot log a shard id
// under three different key names. Correlating a shard handover across two
// nodes' logs is only possible if both wrote "shard" and not "shard_id" or
// "shardID", and the only reliable way to guarantee that is to not let the
// caller choose.
type Logger struct {
	l *slog.Logger
}

// NewLogger builds a JSON logger at the given level, writing to stderr.
// Structured JSON is the default because these logs are read by a collector
// during a fault-injection run, not by a person watching a terminal.
func NewLogger(level slog.Level) *Logger {
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return &Logger{l: slog.New(h)}
}

// NewLoggerFrom wraps an existing slog.Logger, for a host process that has
// already configured its own handler. A nil logger falls back to slog's
// default rather than panicking at the first log line.
func NewLoggerFrom(l *slog.Logger) *Logger {
	if l == nil {
		l = slog.Default()
	}
	return &Logger{l: l}
}

// With returns a logger that adds attrs to every line, for a component that
// wants to stamp its own identity once.
func (lg *Logger) With(args ...any) *Logger {
	return &Logger{l: lg.l.With(args...)}
}

// WithShard stamps a shard id under the one agreed key.
func (lg *Logger) WithShard(s core.ShardID) *Logger {
	return &Logger{l: lg.l.With("shard", int64(s))}
}

// WithWorkflow stamps a workflow and run under the agreed keys.
func (lg *Logger) WithWorkflow(workflowID string, run core.RunID) *Logger {
	return &Logger{l: lg.l.With("workflow_id", workflowID, "run_id", string(run))}
}

// Debug, Info, Warn and Error all take a context so the request id is attached
// without the caller remembering to. That is the entire reason the context is
// the first parameter: making it easy to include is the only way it ends up on
// every line.
func (lg *Logger) Debug(ctx context.Context, msg string, args ...any) {
	lg.log(ctx, slog.LevelDebug, msg, args...)
}

func (lg *Logger) Info(ctx context.Context, msg string, args ...any) {
	lg.log(ctx, slog.LevelInfo, msg, args...)
}

func (lg *Logger) Warn(ctx context.Context, msg string, args ...any) {
	lg.log(ctx, slog.LevelWarn, msg, args...)
}

func (lg *Logger) Error(ctx context.Context, msg string, args ...any) {
	lg.log(ctx, slog.LevelError, msg, args...)
}

// Slog exposes the underlying logger for a library that insists on one.
func (lg *Logger) Slog() *slog.Logger { return lg.l }

func (lg *Logger) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if id := RequestID(ctx); id != "" {
		args = append(args, "request_id", id)
	}
	lg.l.Log(ctx, level, msg, args...)
}
