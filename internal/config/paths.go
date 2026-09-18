package config

import (
	"path/filepath"
	"runtime"
	"strings"
)

// NormalizePath standardizes paths across operating systems:
// - Resolves relative paths to clean absolute representations
// - Converts Windows backslashes to clean slash representations for internal keys
// - Prepends Windows extended-length prefix (\\?\) for local paths longer than 240 chars
func NormalizePath(p string) string {
	if p == "" {
		return ""
	}

	p = strings.TrimSpace(p)
	clean := filepath.Clean(p)

	if runtime.GOOS == "windows" {
		// Handle UNC network paths (e.g. \\server\share)
		if strings.HasPrefix(clean, `\\`) && !strings.HasPrefix(clean, `\\?\`) {
			if strings.HasPrefix(strings.ToLower(clean), `\\?\unc\`) {
				return clean
			}
			return clean
		}

		// Handle drive letter paths (e.g. C:\...)
		if len(clean) >= 2 && clean[1] == ':' {
			if len(clean) > 240 && !strings.HasPrefix(clean, `\\?\`) {
				return `\\?\` + clean
			}
		}
	}

	return clean
}

// ToSlashPath converts OS-specific path separators to forward slashes for internal consistency.
func ToSlashPath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

// ComparePaths checks if two paths point to the same filesystem target (case-insensitive on Windows/macOS).
func ComparePaths(a, b string) bool {
	normA := filepath.Clean(a)
	normB := filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(normA, normB)
	}
	return normA == normB
}
