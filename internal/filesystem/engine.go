package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mediacruncher/internal/observability"
)

// Engine is the entry point for file discovery and ingestion.
type Engine struct {
	scopes      []ScanScope
	ingestFunc  func(FileRecord) error
	dedupPolicy DedupPolicy
	concurrency int
	logger      *observability.Logger
}

// Config holds the configuration for the filesystem engine.
type Config struct {
	Scopes      []ScanScope
	IngestFunc  func(FileRecord) error
	DedupPolicy DedupPolicy
	Concurrent  int
}

// New creates a new filesystem engine with the given configuration.
func New(cfg Config) *Engine {
	concurrency := cfg.Concurrent
	if concurrency <= 0 {
		concurrency = 4
	}
	dedupPolicy := cfg.DedupPolicy
	if dedupPolicy == "" {
		dedupPolicy = DedupSkip
	}
	return &Engine{
		scopes:      cfg.Scopes,
		ingestFunc:  cfg.IngestFunc,
		dedupPolicy: dedupPolicy,
		concurrency: concurrency,
		logger:      		observability.NewStdLogger(observability.DebugLevel, "filesystem"),
	}
}

// Scan performs a full discovery scan across all configured scopes.
func (e *Engine) Scan(ctx context.Context) (*DiscoveryResult, error) {
	start := time.Now()
	var warnings []string

	validScopes := make([]ScanScope, 0, len(e.scopes))
	for _, scope := range e.scopes {
		ws := scope.Validate()
		if len(ws) > 0 {
			warnings = append(warnings, ws...)
		} else {
			validScopes = append(validScopes, scope)
		}
	}

	if len(validScopes) == 0 {
		return &DiscoveryResult{Duration: time.Since(start)}, nil
	}

	resolvedScopes := resolveScopes(validScopes)

	var allFiles []FileRecord
	var discWarnings []string
	var discovered, accepted, skipped, duplicates, dirsSkipped int64

	for _, scope := range resolvedScopes {
		files, warns, d, a, sk, dup, ds := discoverScope(ctx, scope, e.dedupPolicy, e.ingestFunc, e.logger)
		allFiles = append(allFiles, files...)
		discWarnings = append(discWarnings, warns...)
		discovered += d
		accepted += a
		skipped += sk
		duplicates += dup
		dirsSkipped += ds
	}

	warnings = append(warnings, discWarnings...)
	dedupedFiles := finalDedup(allFiles)
	dedupCount := int64(len(allFiles) - len(dedupedFiles))

	result := &DiscoveryResult{
		Files:           dedupedFiles,
		Warnings:        int64(len(warnings)),
		TotalDiscovered: discovered,
		TotalAccepted:   accepted,
		TotalSkipped:    skipped,
		Duplicates:      duplicates + dedupCount,
		DirsSkipped:     dirsSkipped,
		Duration:        time.Since(start),
	}

	return result, nil
}

// Report generates a scan report from the discovery result.
func (e *Engine) Report(d *DiscoveryResult) *Report {
	r := FromDiscovery(d)
	r.ScanScopes = len(e.scopes)
	return r
}

func discoverScope(ctx context.Context, scope ScanScope, dedupPolicy DedupPolicy, ingestFunc func(FileRecord) error, log *observability.Logger) ([]FileRecord, []string, int64, int64, int64, int64, int64) {
	var files []FileRecord
	var warnings []string
	var discovered, accepted, skipped, duplicates, dirsSkipped int64
	seen := make(map[string]bool)

	err := filepath.WalkDir(scope.RootPath, func(path string, d os.DirEntry, walkErr error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if walkErr != nil {
			if os.IsPermission(walkErr) {
				dirsSkipped++
				warnings = append(warnings, fmt.Sprintf("permission denied: %s", path))
				return nil
			}
			warnings = append(warnings, fmt.Sprintf("error accessing %s: %v", path, walkErr))
			return nil
		}

		if scope.MaxDepth > 0 {
			rel, relErr := filepath.Rel(scope.RootPath, path)
			if relErr == nil {
				depth := countSeparator(rel)
				if depth > scope.MaxDepth {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
		}

		if d.IsDir() {
			return nil
		}

		discovered++

		info, infoErr := d.Info()
		if infoErr != nil {
			skipped++
			warnings = append(warnings, fmt.Sprintf("cannot stat %s: %v", path, infoErr))
			return nil
		}

		if info.Size() < 1024 {
			skipped++
			return nil
		}

		ext := filepath.Ext(path)
		if !matchExtension(ext, scope.IncludedExtensions) {
			skipped++
			return nil
		}

		absPath, _ := filepath.Abs(path)
		mediaType := classifyMediaType(absPath, ext)
		if mediaType == MediaTypeOther {
			skipped++
			return nil
		}

		partialHash, hashErr := computePartialHash(absPath)
		if hashErr != nil {
			warnings = append(warnings, fmt.Sprintf("hash error %s: %v", absPath, hashErr))
		}

		isDup := false
		if partialHash != "" {
			dupKey := partialHash + "|" + absPath
			if seen[dupKey] {
				isDup = true
				duplicates++
			} else {
				seen[dupKey] = true
			}
		}

		record := FileRecord{
			AbsolutePath: absPath,
			Size:         info.Size(),
			ModTime:      info.ModTime(),
			PartialHash:  partialHash,
			MediaType:    mediaType,
			Warnings:     []string{},
			Duplicate:    isDup,
			SourceScope:  scope.RootPath,
		}

		if isDup {
			record.Warnings = append(record.Warnings, "potential duplicate file")
			if dedupPolicy == DedupSkip || dedupPolicy == DedupReplace {
				skipped++
				return nil
			}
		}

		files = append(files, record)
		accepted++

		if ingestFunc != nil {
			if ingErr := ingestFunc(record); ingErr != nil {
				warnings = append(warnings, fmt.Sprintf("ingestion error %s: %v", absPath, ingErr))
			}
		}

		return nil
	})

	if err != nil && !isContextError(err) && err != filepath.SkipDir {
		warnings = append(warnings, fmt.Sprintf("walk error in %s: %v", scope.RootPath, err))
	}

	return files, warnings, discovered, accepted, skipped, duplicates, dirsSkipped
}

func resolveScopes(scopes []ScanScope) []ScanScope {
	if len(scopes) <= 1 {
		return scopes
	}
	sorted := make([]ScanScope, len(scopes))
	copy(sorted, scopes)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].RootPath) > len(sorted[j].RootPath)
	})
	var resolved []ScanScope
	for _, s := range sorted {
		isNested := false
		for _, existing := range resolved {
			if strings.HasPrefix(s.RootPath, existing.RootPath+string(filepath.Separator)) {
				isNested = true
				break
			}
		}
		if !isNested {
			var kept []ScanScope
			for _, existing := range resolved {
				if !strings.HasPrefix(existing.RootPath, s.RootPath+string(filepath.Separator)) {
					kept = append(kept, existing)
				}
			}
			resolved = kept
			resolved = append(resolved, s)
		}
	}
	return resolved
}

func finalDedup(files []FileRecord) []FileRecord {
	seen := make(map[string]bool)
	var deduped []FileRecord
	for _, f := range files {
		if f.Duplicate || seen[f.AbsolutePath] {
			continue
		}
		seen[f.AbsolutePath] = true
		deduped = append(deduped, f)
	}
	return deduped
}

func matchExtension(ext string, allowed []string) bool {
	ext = lowercase(ext)
	if ext == "" {
		return false
	}
	for _, a := range allowed {
		if lowercase(a) == ext {
			return true
		}
	}
	return false
}

func countSeparator(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == filepath.Separator {
			n++
		}
	}
	return n
}

func isContextError(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

// DiscoveryResult represents the aggregated output of a discovery phase.
type DiscoveryResult struct {
	TotalFiles      int64
	TotalSize       int64
	VideoFiles      int64
	AudioFiles      int64
	ImageFiles      int64
	OtherFiles      int64
	Warnings        int64
	Duplicates      int64
	Duration        time.Duration
	Scopes          map[string]int64
	Files           []FileRecord
	TotalDuplicates int64
	TotalDiscovered int64
	TotalAccepted   int64
	TotalSkipped    int64
	DirsSkipped     int64
}

// SortBySize sorts records by file size descending.
func (r *DiscoveryResult) SortBySize() {
	sort.Slice(r.Files, func(i, j int) bool {
		return r.Files[i].Size > r.Files[j].Size
	})
}

// SortByModTime sorts records by modification time descending.
func (r *DiscoveryResult) SortByModTime() {
	sort.Slice(r.Files, func(i, j int) bool {
		return r.Files[i].ModTime.After(r.Files[j].ModTime)
	})
}

// FilterByType returns records matching the given media type.
func (r *DiscoveryResult) FilterByType(mtype MediaType) []FileRecord {
	var result []FileRecord
	for _, rec := range r.Files {
		if rec.MediaType == mtype {
			result = append(result, rec)
		}
	}
	return result
}

// Summary returns a formatted summary string.
func (r *DiscoveryResult) Summary() string {
	sb := strings.Builder{}
	sb.WriteString("Discovery Summary:\n")
	sb.WriteString(fmt.Sprintf("  Total Files: %d\n", r.TotalFiles))
	sb.WriteString(fmt.Sprintf("  Total Size: %s\n", humanizeSize(r.TotalSize)))
	sb.WriteString(fmt.Sprintf("  Video: %d\n", r.VideoFiles))
	sb.WriteString(fmt.Sprintf("  Audio: %d\n", r.AudioFiles))
	sb.WriteString(fmt.Sprintf("  Image: %d\n", r.ImageFiles))
	sb.WriteString(fmt.Sprintf("  Other: %d\n", r.OtherFiles))
	sb.WriteString(fmt.Sprintf("  Warnings: %d\n", r.Warnings))
	sb.WriteString(fmt.Sprintf("  Duplicates: %d\n", r.Duplicates))
	sb.WriteString(fmt.Sprintf("  Duration: %v\n", r.Duration))
	sb.WriteString("  Scopes:\n")
	for scope, count := range r.Scopes {
		sb.WriteString(fmt.Sprintf("    %s: %d files\n", scope, count))
	}
	return sb.String()
}

// humanizeSize formats a byte count into a human-readable string.
func humanizeSize(bytes int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(bytes)
	unitIndex := 0
	for size >= 1024 && unitIndex < len(units)-1 {
		size /= 1024
		unitIndex++
	}
	return fmt.Sprintf("%.2f %s", size, units[unitIndex])
}
