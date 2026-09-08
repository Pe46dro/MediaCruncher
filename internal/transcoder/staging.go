package transcoder

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StagingManager handles temporary file operations for transcoding.
type StagingManager struct {
	BaseDir    string
	Retention  time.Duration
}

// NewStagingManager creates a new staging manager.
func NewStagingManager(baseDir string, retention time.Duration) *StagingManager {
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "mediacruncher-staging")
	}
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	return &StagingManager{
		BaseDir:   baseDir,
		Retention: retention,
	}
}

// Allocate creates a staging directory for a job.
func (s *StagingManager) Allocate(jobID string) (string, error) {
	stagingDir := filepath.Join(s.BaseDir, jobID)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	return stagingDir, nil
}

// Commit moves the staged output to the final location.
func (s *StagingManager) Commit(stagingDir, outputPath string) error {
	if stagingDir == "" || outputPath == "" {
		return fmt.Errorf("invalid staging or output path")
	}

	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	stagedFile := filepath.Join(stagingDir, filepath.Base(outputPath))
	if _, err := os.Stat(stagedFile); err != nil {
		stagedFile = filepath.Join(stagingDir, "out"+filepath.Ext(outputPath))
	}

	if err := os.Rename(stagedFile, outputPath); err != nil {
		return fmt.Errorf("move staged output to final location: %w", err)
	}

	return nil
}

// Cleanup removes the staging directory and all its contents.
func (s *StagingManager) Cleanup(jobID string) error {
	stagingDir := filepath.Join(s.BaseDir, jobID)
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("cleanup staging directory: %w", err)
	}
	return nil
}

// CleanupAll removes all staging directories.
func (s *StagingManager) CleanupAll() error {
	return os.RemoveAll(s.BaseDir)
}

// Preserve temporarily preserves a staging directory for forensic analysis.
func (s *StagingManager) Preserve(jobID string) (string, error) {
	stagingDir := filepath.Join(s.BaseDir, jobID)
	preservedDir := stagingDir + ".preserved"

	if _, err := os.Stat(stagingDir); os.IsNotExist(err) {
		return "", fmt.Errorf("staging directory does not exist: %s", stagingDir)
	}

	if err := os.Rename(stagingDir, preservedDir); err != nil {
		return "", fmt.Errorf("preserve staging directory: %w", err)
	}

	return preservedDir, nil
}

// GetStagedPath returns the expected path for a staged output file.
func (s *StagingManager) GetStagedPath(jobID string, ext string) string {
	return filepath.Join(s.BaseDir, jobID, "out"+ext)
}

// Exists checks if a staging directory exists.
func (s *StagingManager) Exists(jobID string) bool {
	stagingDir := filepath.Join(s.BaseDir, jobID)
	info, err := os.Stat(stagingDir)
	return err == nil && info.IsDir()
}
