package observability

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Level represents a structured log severity level.
type Level int

const (
	DebugLevel Level = iota
	InfoLevel
	WarnLevel
	ErrorLevel
	CriticalLevel
)

func (l Level) String() string {
	switch l {
	case DebugLevel:
		return "debug"
	case InfoLevel:
		return "info"
	case WarnLevel:
		return "warn"
	case ErrorLevel:
		return "error"
	case CriticalLevel:
		return "critical"
	default:
		return "unknown"
	}
}

// Field is a key-value pair in the log entry context block.
type Field struct {
	Key   string
	Value interface{}
}

// Logger provides structured JSON logging with severity levels and module context.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	level  Level
	module string
	fields []Field
}

// NewLogger creates a logger that writes JSON entries to the given writer
// at the specified minimum severity level and module context.
func NewLogger(w io.Writer, level Level, module string) *Logger {
	return &Logger{
		w:      w,
		level:  level,
		module: module,
	}
}

// NewStdLogger creates a logger configured for the application's stdout
// with the given log level and module context.
func NewStdLogger(level Level, module string) *Logger {
	return &Logger{
		w:      os.Stdout,
		level:  level,
		module: module,
	}
}

// WithFields returns a new logger with additional context fields appended
// to each log entry.
func (l *Logger) WithFields(fields ...Field) *Logger {
	nl := &Logger{
		w:      l.w,
		level:  l.level,
		module: l.module,
		fields: make([]Field, len(l.fields)+len(fields)),
	}
	copy(nl.fields, l.fields)
	copy(nl.fields[len(l.fields):], fields)
	return nl
}

// Log writes a structured JSON log entry at the specified level.
func (l *Logger) Log(level Level, msg string) {
	if level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := make(map[string]interface{}, 4+len(l.fields))
	entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	entry["level"] = level.String()
	entry["module"] = l.module
	entry["msg"] = msg

	if len(l.fields) > 0 {
		ctx := make(map[string]interface{}, len(l.fields))
		for _, f := range l.fields {
			ctx[f.Key] = f.Value
		}
		entry["context"] = ctx
	}

	enc := json.NewEncoder(l.w)
	enc.SetEscapeHTML(false)
	enc.Encode(entry)
}

// Debug logs at debug severity.
func (l *Logger) Debug(msg string) {
	l.Log(DebugLevel, msg)
}

// Info logs at info severity.
func (l *Logger) Info(msg string) {
	l.Log(InfoLevel, msg)
}

// Warn logs at warn severity.
func (l *Logger) Warn(msg string) {
	l.Log(WarnLevel, msg)
}

// Error logs at error severity.
func (l *Logger) Error(msg string) {
	l.Log(ErrorLevel, msg)
}

// Critical logs at critical severity for unrecoverable errors.
func (l *Logger) Critical(msg string) {
	l.Log(CriticalLevel, msg)
}
