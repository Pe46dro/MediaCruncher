package transcoder

import (
	"fmt"
	"path/filepath"
	"strings"
)

// sanitizePath validates and cleans a file path for safe use with external processes.
// It ensures the path is absolute, does not start with a dash (preventing flag injection),
// and resolves to a location within the expected scope directories.
func sanitizePath(path string, scopeDirs []string) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return "", fmt.Errorf("resolve path %q: %w", path, err)
		}
		clean = abs
	}

	if strings.HasPrefix(clean, "-") {
		return "", fmt.Errorf("path appears to be a flag, not a file: %s", path)
	}

	if len(scopeDirs) > 0 {
		valid := false
		for _, dir := range scopeDirs {
			absDir, err := filepath.Abs(filepath.Clean(dir))
			if err != nil {
				continue
			}
			if strings.HasPrefix(clean, absDir+string(filepath.Separator)) || clean == absDir {
				valid = true
				break
			}
		}
		if !valid {
			return "", fmt.Errorf("path %q is outside allowed scope directories", path)
		}
	}

	return clean, nil
}
