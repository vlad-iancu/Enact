// Package logging provides the leveled, structured logger used by all enact
// services.
//
// A log line has the format (the message is always double-quoted):
//
//	[{timestamp}] ({level}) field1=value1 field2=value2 msg="{message}"
//
// Levels are numeric and a record is printed when its level is less-or-equal
// than the configured LOGGING_LEVEL (default INFO = 1):
//
//	ERROR = -1, WARN = 0, INFO = 1, DEBUG = 2
//
// so INFO shows Info/Warn/Error, DEBUG shows everything, and ERROR shows only
// errors. LOGGING_LEVEL accepts either the number or the level name
// (case-insensitive), e.g. LOGGING_LEVEL=DEBUG or LOGGING_LEVEL=2.
//
// Loggers carry fields — ordered key-value pairs prepended to every record:
//
//	logger := logging.New().WithFields("kb_id", kbID, "file_name", filename)
//	logger.Info("document queued")
//	logger.Error("indexing failed", "err", err) // per-call fields append too
//
// The environment is consulted lazily on the first log call (not at package
// init), so LOGGING_LEVEL set via the .env files loaded by service.Run is
// honoured even for package-level loggers.
package logging

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Level is a log verbosity level. A record prints when its level is
// less-or-equal than the configured level.
type Level int

const (
	LevelError Level = -1
	LevelWarn  Level = 0
	LevelInfo  Level = 1
	LevelDebug Level = 2
)

// EnvVar is the environment variable that sets the logging level.
const EnvVar = "LOGGING_LEVEL"

// String returns the level's name as it appears in log lines.
func (l Level) String() string {
	switch l {
	case LevelError:
		return "ERROR"
	case LevelWarn:
		return "WARN"
	case LevelInfo:
		return "INFO"
	case LevelDebug:
		return "DEBUG"
	default:
		return fmt.Sprintf("LEVEL(%d)", int(l))
	}
}

// ParseLevel converts a level name ("debug", "INFO", ...) or number ("2")
// into a Level. Unrecognised values fall back to LevelInfo.
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ERROR":
		return LevelError
	case "WARN", "WARNING":
		return LevelWarn
	case "INFO", "":
		return LevelInfo
	case "DEBUG":
		return LevelDebug
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return Level(n)
	}
	return LevelInfo
}

// envLevel resolves LOGGING_LEVEL once, on first use. Resolution is deferred
// to the first log call so dotenv files loaded during service startup are
// visible even to loggers created at package init.
var envLevel = sync.OnceValue(func() Level {
	return ParseLevel(os.Getenv(EnvVar))
})

// field is one key-value pair attached to a logger or a single record.
type field struct {
	key   string
	value any
}

// Logger writes leveled, fielded log records. The zero value is not usable;
// construct one with New or NewWithLevel. Loggers are immutable: WithFields
// returns a derived copy, so a Logger is safe for concurrent use.
type Logger struct {
	level  *Level // nil = resolve from LOGGING_LEVEL lazily
	out    io.Writer
	fields []field
	ctx    context.Context // nil = no request context attached
}

// New returns a Logger writing to stderr whose level is read from
// LOGGING_LEVEL on first use (default INFO).
func New() *Logger {
	return &Logger{out: os.Stderr}
}

// NewWithLevel returns a Logger with an explicit level and output writer,
// independent of the environment. Intended for tests.
func NewWithLevel(level Level, out io.Writer) *Logger {
	return &Logger{level: &level, out: out}
}

// WithFields returns a copy of the logger with the given key-value pairs
// appended to its fields. Keys are strings; a trailing key without a value is
// recorded as key=!MISSING.
//
//	logger = logger.WithFields("kb_id", kbID, "file_name", filename)
func (l *Logger) WithFields(kv ...any) *Logger {
	if len(kv) == 0 {
		return l
	}
	derived := *l
	derived.fields = append(l.fields[:len(l.fields):len(l.fields)], pair(kv)...)
	return &derived
}

// WithContext returns a copy of the logger bound to ctx. Any span active on
// ctx is used two ways: its trace_id/span_id are added to each printed record
// so local logs correlate to traces in Grafana, and ctx is handed to the
// OpenTelemetry log bridge so records shipped to Loki carry the same trace
// context. Pass the request context (available to handlers after the
// requesthelper.TracingFilter runs) to tie a service's logs to its trace.
func (l *Logger) WithContext(ctx context.Context) *Logger {
	derived := *l
	derived.ctx = ctx
	return &derived
}

// Debug logs at DEBUG level. Extra parameters are key-value pairs, as in
// WithFields.
func (l *Logger) Debug(msg string, kv ...any) { l.log(LevelDebug, msg, kv) }

// Info logs at INFO level. Extra parameters are key-value pairs.
func (l *Logger) Info(msg string, kv ...any) { l.log(LevelInfo, msg, kv) }

// Warn logs at WARN level. Extra parameters are key-value pairs.
func (l *Logger) Warn(msg string, kv ...any) { l.log(LevelWarn, msg, kv) }

// Error logs at ERROR level. Extra parameters are key-value pairs.
func (l *Logger) Error(msg string, kv ...any) { l.log(LevelError, msg, kv) }

// Enabled reports whether a record at the given level would be printed.
func (l *Logger) Enabled(level Level) bool {
	configured := envLevel()
	if l.level != nil {
		configured = *l.level
	}
	return level <= configured
}

// timeFormat is RFC 3339 with millisecond precision.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

func (l *Logger) log(level Level, msg string, kv []any) {
	if !l.Enabled(level) {
		return
	}
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(time.Now().UTC().Format(timeFormat))
	b.WriteString("] (")
	b.WriteString(level.String())
	b.WriteByte(')')
	if tid, sid, ok := traceIDs(l.ctx); ok {
		writeField(&b, field{key: "trace_id", value: tid})
		writeField(&b, field{key: "span_id", value: sid})
	}
	for _, f := range l.fields {
		writeField(&b, f)
	}
	extra := pair(kv)
	for _, f := range extra {
		writeField(&b, f)
	}
	b.WriteString(" msg=")
	b.WriteString(strconv.Quote(msg))
	b.WriteByte('\n')
	// A single Write keeps concurrent records from interleaving.
	_, _ = io.WriteString(l.out, b.String())

	// Mirror the record to Loki via the OpenTelemetry log bridge when enabled.
	emitOTel(l.ctx, level, msg, l.fields, extra)
}

// traceIDs returns the trace and span IDs of the span active on ctx, if any.
func traceIDs(ctx context.Context) (traceID, spanID string, ok bool) {
	if ctx == nil {
		return "", "", false
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", "", false
	}
	return sc.TraceID().String(), sc.SpanID().String(), true
}

// pair groups a flat key-value list into fields. Non-string keys are
// stringified; a trailing key with no value gets the value !MISSING.
func pair(kv []any) []field {
	if len(kv) == 0 {
		return nil
	}
	fields := make([]field, 0, (len(kv)+1)/2)
	for i := 0; i < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			key = fmt.Sprint(kv[i])
		}
		var value any = "!MISSING"
		if i+1 < len(kv) {
			value = kv[i+1]
		}
		fields = append(fields, field{key: key, value: value})
	}
	return fields
}

func writeField(b *strings.Builder, f field) {
	b.WriteByte(' ')
	b.WriteString(f.key)
	b.WriteByte('=')
	b.WriteString(formatValue(f.value))
}

// formatValue renders a field value, quoting it when it contains characters
// that would break the key=value structure of the line.
func formatValue(v any) string {
	s := fmt.Sprint(v)
	if strings.ContainsAny(s, " =\"\n\t") {
		return strconv.Quote(s)
	}
	if s == "" {
		return `""`
	}
	return s
}
