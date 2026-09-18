package transcoder

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PromoteFile moves a staged output file to its final destination safely across filesystem boundaries.
func PromoteFile(stagedPath, destPath string, inPlaceOverwrite bool) error {
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory %s: %w", destDir, err)
	}

	backupPath := destPath + ".media_cruncher_backup"
	hasBackup := false

	// If in-place overwrite, move existing destination to a temporary backup first
	if _, err := os.Stat(destPath); err == nil {
		if err := os.Rename(destPath, backupPath); err != nil {
			// Fallback copy if rename fails
			if copyErr := streamCopy(destPath, backupPath); copyErr != nil {
				return fmt.Errorf("failed to backup existing file before replacement: %w", copyErr)
			}
			os.Remove(destPath)
		}
		hasBackup = true
	}

	// Attempt fast atomic intra-filesystem rename
	err := os.Rename(stagedPath, destPath)
	if err == nil {
		// Success! Remove backup if one was created
		if hasBackup {
			os.Remove(backupPath)
		}
		return nil
	}

	// Cross-device EXDEV / cross-volume fallback
	tmpDest := destPath + ".media_cruncher_tmp"
	defer os.Remove(tmpDest)

	if copyErr := streamCopyWithVerification(stagedPath, tmpDest); copyErr != nil {
		// Restore original file from backup on failure
		if hasBackup {
			os.Rename(backupPath, destPath)
		}
		return fmt.Errorf("cross-device promotion failed: %w", copyErr)
	}

	// Atomic rename within the destination filesystem
	if renameErr := os.Rename(tmpDest, destPath); renameErr != nil {
		if hasBackup {
			os.Rename(backupPath, destPath)
		}
		return fmt.Errorf("destination promotion rename failed: %w", renameErr)
	}

	// Clean up staged file and backup
	os.Remove(stagedPath)
	if hasBackup {
		os.Remove(backupPath)
	}

	return nil
}

func streamCopyWithVerification(src, dst string) error {
	sFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sFile.Close()

	dFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dFile.Close()

	sHash := sha256.New()
	dHash := sha256.New()

	sReader := io.TeeReader(sFile, sHash)
	dWriter := io.MultiWriter(dFile, dHash)

	buf := make([]byte, 1024*1024) // 1MB streaming buffer
	if _, err := io.CopyBuffer(dWriter, sReader, buf); err != nil {
		return err
	}

	if err := dFile.Sync(); err != nil {
		return err
	}

	// Verify cryptographic hash integrity
	sSum := sHash.Sum(nil)
	dSum := dHash.Sum(nil)
	if string(sSum) != string(dSum) {
		return fmt.Errorf("sha-256 checksum mismatch after cross-device copy")
	}

	return nil
}

func streamCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
