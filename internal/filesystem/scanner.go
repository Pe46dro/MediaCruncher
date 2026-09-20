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
	dedupe   *DedupeIndex
	metrics  *observability.Metrics
}

func NewScanner(scopes []config.ScanScopeConfig, buffer *IngestionBuffer) *Scanner {
	return &Scanner{
		scopes:  scopes,
		buffer:  buffer,
		dedupe:  NewDedupeIndex(),
		metrics: observability.GetMetrics(),
	}
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
