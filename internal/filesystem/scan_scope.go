package filesystem

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MediaType represents the classification of a media file.
type MediaType string

const (
	MediaTypeVideo MediaType = "video"
	MediaTypeAudio MediaType = "audio"
	MediaTypeImage MediaType = "image"
	MediaTypeOther MediaType = "other"
)

// SymlinkPolicy defines how symbolic links are handled during traversal.
type SymlinkPolicy string

const (
	SymlinkFollow    SymlinkPolicy = "follow"
	SymlinkSkip      SymlinkPolicy = "skip"
	SymlinkDereference SymlinkPolicy = "dereference"
)

// DedupPolicy defines how duplicate files are handled.
type DedupPolicy string

const (
	DedupSkip    DedupPolicy = "skip"
	DedupFlag    DedupPolicy = "flag"
	DedupReplace DedupPolicy = "replace"
)

// FileRecord is the output unit of the filesystem engine.
type FileRecord struct {
	AbsolutePath string
	Size         int64
	ModTime      time.Time
	PartialHash  string
	MediaType    MediaType
	Warnings     []string
	Duplicate    bool
	SourceScope  string
}

// ScanScope represents a user-configured root path with inclusion and exclusion rules.
type ScanScope struct {
	RootPath           string
	IncludedExtensions []string
	ExclusionPatterns  []string
	MaxDepth           int
	SymlinkPolicy      SymlinkPolicy
	IOTimeout          time.Duration
}

// Validate checks the scan scope configuration and returns warnings.
func (s *ScanScope) Validate() []string {
	var warnings []string
	if s.RootPath == "" {
		return []string{"root_path is empty"}
	}

	cleanPath := filepath.Clean(s.RootPath)
	if !filepath.IsAbs(cleanPath) {
		abs, err := filepath.Abs(cleanPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("cannot resolve path: %s", err))
			return warnings
		}
		cleanPath = abs
	}

	canonical, err := filepath.EvalSymlinks(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("path does not exist: %s", s.RootPath))
		} else if os.IsPermission(err) {
			warnings = append(warnings, fmt.Sprintf("permission denied: %s", s.RootPath))
		} else {
			warnings = append(warnings, fmt.Sprintf("cannot access path: %s", err))
		}
		return warnings
	}

	info, err := os.Stat(canonical)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("cannot stat path: %s", err))
		return warnings
	}
	if !info.IsDir() {
		warnings = append(warnings, fmt.Sprintf("path is not a directory: %s", s.RootPath))
	}

	s.RootPath = canonical

	if s.SymlinkPolicy == "" {
		s.SymlinkPolicy = SymlinkSkip
	}
	if s.IncludedExtensions == nil {
		s.IncludedExtensions = []string{"mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "mpg", "mpeg", "ts", "m2ts"}
	}
	return warnings
}

// classifyMediaType determines the media type based on file extension and MIME sniffing.
func classifyMediaType(path string, ext string) MediaType {
	ext = strings.TrimPrefix(lowercase(ext), ".")
	mediaType := sniffMediaType(path)
	if mediaType != "" {
		return mediaType
	}
	switch ext {
	case "mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "mpg", "mpeg", "ts", "m2ts", "vob":
		return MediaTypeVideo
	case "mp3", "wav", "flac", "aac", "ogg", "wma", "m4a", "opus":
		return MediaTypeAudio
	case "jpg", "jpeg", "png", "gif", "bmp", "tiff", "webp", "svg":
		return MediaTypeImage
	default:
		return MediaTypeOther
	}
}

// sniffMediaType reads the first 512 bytes of a file and attempts to determine its type.
func sniffMediaType(path string) MediaType {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil || n < 4 {
		return ""
	}
	buf = buf[:n]
	if len(buf) > 8 && string(buf[4:8]) == "ftyp" {
		return MediaTypeVideo
	}
	if len(buf) > 4 && buf[0] == 0x1A && buf[1] == 0x45 && buf[2] == 0xDF && buf[3] == 0xA3 {
		return MediaTypeVideo
	}
	if len(buf) > 8 && string(buf[0:4]) == "RIFF" && string(buf[8:12]) == "AVI" {
		return MediaTypeVideo
	}
	return ""
}

// ComputePartialHash computes a hash from the first and last megabyte of the file.
func ComputePartialHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1024*1024)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read first MB: %w", err)
	}
	h.Write(buf[:n])
	stat, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat file: %w", err)
	}
	fileSize := stat.Size()
	var seekOffset int64
	if fileSize > int64(len(buf)) {
		seekOffset = fileSize - int64(len(buf))
	}
	if seekOffset > 0 {
		if _, err := f.Seek(seekOffset, io.SeekStart); err != nil {
			return "", fmt.Errorf("seek to last MB: %w", err)
		}
		n, err := f.Read(buf)
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read last MB: %w", err)
		}
		h.Write(buf[:n])
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func computePartialHash(path string) (string, error) {
	return ComputePartialHash(path)
}

func lowercase(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c = c + ('a' - 'A')
		}
		result[i] = c
	}
	return string(result)
}

// MatchExtension checks if the file extension is in the allowed list.
func MatchExtension(ext string, allowed []string) bool {
	ext = lowercase(filepath.Ext(ext))
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
