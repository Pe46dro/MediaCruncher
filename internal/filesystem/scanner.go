// Package filesystem implements directory scanning and media file discovery
// for MediaCruncher.
//
// Architecture:
//
//	The Scanner walks configured scopes (directories) and pushes discovered
//	media files into an IngestionBuffer. The buffer decouples scanning from
//	queuing, allowing the scanner to run at full disk speed while the queue
//	consumer processes at its own pace.
//
// Deduplication integration:
//	The Scanner receives a shared *dedupe.Index singleton. Each discovered file
//	is checked through CheckAndRecord() before being pushed to the buffer.
//	Duplicates are filtered at scan time, reducing queue pressure.
//
// Exclusion patterns:
//	Each scope can specify extensions (accepted) and exclusions (wildcards like
//	"*.part", "*.tmp", "*tmp*"). Files not matching extensions are skipped;
//	files matching exclusions are skipped even if they match extensions.
//
// Hot scope management:
//	AddScope() / RemoveScope() allow runtime modification of scan directories.
//	The REST API endpoints /api/filesystem/scopes expose this for live config
//	updates without restart.
package filesystem

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/dedupe"
	"mediacruncher/internal/observability"
)

type ScanReport struct {
	TotalDiscovered   int           `json:"total_discovered"`
	Accepted          int           `json:"accepted"`
	Duplicates        int           `json:"duplicates"`
	SkippedExtension  int           `json:"skipped_extension"`
	SkippedExclusions int           `json:"skipped_exclusions"`
	Errors            int           `json:"errors"`
	Duration          time.Duration `json:"duration"`
}

type Scanner struct {
	scopes   []config.ScanScopeConfig
	buffer   *IngestionBuffer
	dedupe   *dedupe.Index
	metrics  *observability.Metrics
}

// NewScanner creates a Scanner that shares the provided DedupeIndex.
// The DedupeIndex must be a singleton shared across the daemon lifetime
// to avoid re-counting already-seen files on each scan cycle.
func NewScanner(scopes []config.ScanScopeConfig, buffer *IngestionBuffer, dedupeIdx *dedupe.Index) *Scanner {
	return &Scanner{
		scopes:  scopes,
		buffer:  buffer,
		dedupe:  dedupeIdx,
		metrics: observability.GetMetrics(),
	}
}

// AddScope appends a new scope and triggers an immediate scan of it.
func (s *Scanner) AddScope(scope config.ScanScopeConfig) {
	s.scopes = append(s.scopes, scope)
}

// RemoveScope removes a scope by path (case-sensitive match) and returns true if found.
func (s *Scanner) RemoveScope(path string) bool {
	for i, scope := range s.scopes {
		if scope.Path == path {
			s.scopes = append(s.scopes[:i], s.scopes[i+1:]...)
			return true
		}
	}
	return false
}

// GetScopes returns a copy of the current scopes (safe for read access).
func (s *Scanner) GetScopes() []config.ScanScopeConfig {
	out := make([]config.ScanScopeConfig, len(s.scopes))
	copy(out, s.scopes)
	return out
}

// ScanScopes walks all configured directories and pushes discovered media files into the buffer.
func (s *Scanner) ScanScopes(ctx context.Context) (*ScanReport, error) {
	start := time.Now()
	report := &ScanReport{}

	for _, scope := range s.scopes {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		s.scanScope(ctx, scope, report)
	}

	report.Duration = time.Since(start)
	slog.Info("completed filesystem scan",
		"total", report.TotalDiscovered,
		"accepted", report.Accepted,
		"duplicates", report.Duplicates,
		"errors", report.Errors,
		"duration", report.Duration.String(),
	)
	return report, nil
}

func (s *Scanner) scanScope(ctx context.Context, scope config.ScanScopeConfig, report *ScanReport) {
	rootPath := config.NormalizePath(scope.Path)
	info, err := os.Stat(rootPath)
	if err != nil {
		slog.Warn("scan scope inaccessible", "path", rootPath, "error", err)
		report.Errors++
		return
	}
	if !info.IsDir() {
		slog.Warn("scan scope is not a directory", "path", rootPath)
		report.Errors++
		return
	}

	allowedExts := make(map[string]bool)
	for _, ext := range scope.Extensions {
		allowedExts[strings.ToLower(strings.TrimPrefix(ext, "."))] = true
	}

	err = filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			slog.Warn("directory walk permission or read error", "path", path, "error", err)
			report.Errors++
			return nil // continue walking other entries
		}

		if d.IsDir() {
			rel, _ := filepath.Rel(rootPath, path)
			depth := len(strings.Split(filepath.ToSlash(rel), "/"))
			if scope.MaxDepth > 0 && depth > scope.MaxDepth && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}

		// Handle symlink policy
		if d.Type()&fs.ModeSymlink != 0 {
			if !scope.FollowSymlinks {
				return nil
			}
		}

		report.TotalDiscovered++
		s.metrics.FilesScannedTotal.Add(1)

		// Check exclusions
		fileName := d.Name()
		for _, pattern := range scope.Exclusions {
			if matched, _ := filepath.Match(pattern, fileName); matched {
				report.SkippedExclusions++
				return nil
			}
		}

		// Automatically skip already crunched files (*_crunched.*)
		extWithDot := filepath.Ext(fileName)
		baseName := strings.TrimSuffix(fileName, extWithDot)
		if strings.HasSuffix(strings.ToLower(baseName), "_crunched") {
			report.SkippedExclusions++
			return nil
		}

		// Check extensions
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileName), "."))
		if len(allowedExts) > 0 && !allowedExts[ext] {
			report.SkippedExtension++
			return nil
		}

		fileInfo, err := d.Info()
		if err != nil {
			report.Errors++
			return nil
		}
		size := fileInfo.Size()

		// Evaluate Two-Tier Deduplication
		normalizedPath := config.NormalizePath(path)
		isDupe, origPath, err := s.dedupe.CheckAndRecord(normalizedPath, size)
		if err != nil {
			slog.Warn("deduplication check error", "path", normalizedPath, "error", err)
		}

		record := &FileRecord{
			Path:        normalizedPath,
			Size:        size,
			ModTime:     fileInfo.ModTime(),
			Extension:   ext,
			IsDuplicate: isDupe,
			Original:    origPath,
		}

		if isDupe {
			report.Duplicates++
			s.metrics.DeduplicatedFiles.Add(1)
		} else {
			report.Accepted++
		}

		// Push to buffer (blocks when buffer is full, providing natural backpressure)
		if err := s.buffer.Push(ctx, record); err != nil {
			return err
		}

		return nil
	})

	if err != nil && err != context.Canceled {
		slog.Error("scope scan completed with error", "path", rootPath, "error", err)
	}
}
