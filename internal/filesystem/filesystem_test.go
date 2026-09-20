package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediacruncher/internal/config"
)

func TestDeduplication(t *testing.T) {
	tmpDir := t.TempDir()

	fileA := filepath.Join(tmpDir, "fileA.mp4")
	fileB := filepath.Join(tmpDir, "fileB.mp4")
	fileC := filepath.Join(tmpDir, "fileC.mp4")

	contentSame := []byte("identical content across duplicate files with size 50 bytes!!")
	contentDiff := []byte("different content in a file with the same 50 bytes total!!")

	if err := os.WriteFile(fileA, contentSame, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, contentSame, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileC, contentDiff, 0644); err != nil {
		t.Fatal(err)
	}

	idx := NewDedupeIndex()

	// First file: should be accepted
	isDupe, _, err := idx.CheckAndRecord(fileA, int64(len(contentSame)))
	if err != nil || isDupe {
		t.Fatalf("expected fileA to be unique, got dupe=%v, err=%v", isDupe, err)
	}

	// Second file (identical content & size): should be detected as duplicate
	isDupe, orig, err := idx.CheckAndRecord(fileB, int64(len(contentSame)))
	if err != nil || !isDupe {
		t.Fatalf("expected fileB to be duplicate, got dupe=%v, err=%v", isDupe, err)
	}
	if orig != fileA {
		t.Fatalf("expected original to be fileA, got %s", orig)
	}

	// Third file (same size, different content): should be unique!
	isDupe, _, err = idx.CheckAndRecord(fileC, int64(len(contentDiff)))
	if err != nil || isDupe {
		t.Fatalf("expected fileC with different bytes to be unique, got dupe=%v, err=%v", isDupe, err)
	}
}

func TestScanner(t *testing.T) {
	tmpDir := t.TempDir()

	// Create files
	media1 := filepath.Join(tmpDir, "movie1.mkv")
	media2 := filepath.Join(tmpDir, "movie2.mp4")
	crunchedFile := filepath.Join(tmpDir, "movie2_crunched.mp4")
	tempFile := filepath.Join(tmpDir, "movie.temp")
	subDir := filepath.Join(tmpDir, "nested")
	os.Mkdir(subDir, 0755)
	media3 := filepath.Join(subDir, "clip.mov")

	os.WriteFile(media1, []byte("movie 1 data"), 0644)
	os.WriteFile(media2, []byte("movie 2 data"), 0644)
	os.WriteFile(crunchedFile, []byte("movie 2 crunched data"), 0644)
	os.WriteFile(tempFile, []byte("temp data"), 0644)
	os.WriteFile(media3, []byte("movie 3 data"), 0644)

	buffer := NewIngestionBuffer(100)
	defer buffer.Close()

	scope := config.ScanScopeConfig{
		Path:           tmpDir,
		Extensions:     []string{".mkv", ".mp4", ".mov"},
		Exclusions:     []string{"*.temp"},
		MaxDepth:       5,
		FollowSymlinks: false,
	}

	scanner := NewScanner([]config.ScanScopeConfig{scope}, buffer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	report, err := scanner.ScanScopes(ctx)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if report.Accepted != 3 {
		t.Errorf("expected 3 accepted media files, got %d", report.Accepted)
	}
	if report.SkippedExclusions != 2 {
		t.Errorf("expected 2 skipped by exclusion (*.temp and *_crunched), got %d", report.SkippedExclusions)
	}
	if buffer.Len() != 3 {
		t.Errorf("expected 3 items in ingestion buffer, got %d", buffer.Len())
	}
}
