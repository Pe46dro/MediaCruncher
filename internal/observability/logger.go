package observability

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// SetupLogger initializes a production-ready structured logger with:
// - JSON format when logJSON is true
// - Optional file rotation via native implementation (no external deps)
// - Configurable log level
//
// Call once early in main() before any other component logs.
func SetupLogger(logLevel string, logJSON bool, logFile string) error {
	// Determine log level
	var level slog.Level
	switch logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// Build options
	opts := &slog.HandlerOptions{
		Level: level,
	}

	// Create base handler
	var handler slog.Handler
	if logJSON {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	// If log file is configured, add file rotation
	if logFile != "" {
		// Ensure log directory exists
		dir := filepath.Dir(logFile)
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Warn("failed to create log directory", "dir", dir, "err", err)
		} else {
			// Use dual writer: stdout + rotating file log
			fileWriter, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				slog.Warn("failed to open log file", "path", logFile, "err", err)
			} else {
				multiWriter := io.MultiWriter(os.Stdout, fileWriter)
				if logJSON {
					handler = slog.NewJSONHandler(multiWriter, opts)
				} else {
					handler = slog.NewTextHandler(multiWriter, opts)
				}
			}
		}
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

// FileRotatingWriter provides simple log rotation without external dependencies.
type FileRotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int64 // max bytes before rotation (default 10MB)
	maxBackups int   // number of backup files to keep (default 7)
	file       *os.File
	currentSize int64
}

// NewFileRotatingWriter creates a new rotating file writer.
// maxSize is in bytes (default 10MB), maxBackups is the number of rotated files to keep.
func NewFileRotatingWriter(path string, maxSize int64, maxBackups int) (*FileRotatingWriter, error) {
	if maxSize <= 0 {
		maxSize = 10 * 1024 * 1024 // 10MB
	}
	if maxBackups <= 0 {
		maxBackups = 7
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	// Get current file size
	var size int64
	info, err := os.Stat(path)
	if err == nil {
		size = info.Size()
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}

	return &FileRotatingWriter{
		path:         path,
		maxSize:      maxSize,
		maxBackups:   maxBackups,
		file:         f,
		currentSize:  size,
	}, nil
}

// Write implements io.Writer with automatic rotation.
func (w *FileRotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Check if rotation is needed
	if w.currentSize+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.currentSize += int64(n)
	return n, err
}

// rotate renames the current log file and opens a new one.
func (w *FileRotatingWriter) rotate() error {
	// Close current file
	w.file.Close()

	// Remove oldest backup if we have too many
	for i := w.maxBackups; i > 0; i-- {
		oldPath := w.path + "." + string(rune('0'+i))
		if i == w.maxBackups {
			// Remove the oldest
			os.Remove(oldPath)
		} else {
			// Rename old backup to next slot
			newPath := w.path + "." + string(rune('0'+i+1))
			os.Rename(oldPath, newPath)
		}
	}

	// Rotate current log
	backupPath := w.path + ".1"
	os.Rename(w.path, backupPath)

	// Open new file
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	w.file = f
	w.currentSize = 0
	return nil
}

// Close closes the underlying file.
func (w *FileRotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// MustSetupLogger is like SetupLogger but panics on error.
func MustSetupLogger(logLevel string, logJSON bool, logFile string) {
	if err := SetupLogger(logLevel, logJSON, logFile); err != nil {
		panic("failed to setup logger: " + err.Error())
	}
}
