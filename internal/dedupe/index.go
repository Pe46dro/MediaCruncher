// Package dedupe provides a memory-efficient, two-tier file deduplication engine.
//
// Two-tier strategy:
//  Tier 1 — Size check: files with unique byte sizes are immediately considered
//            distinct. Zero I/O — only a map lookup.
//  Tier 2 — Boundary hashing: when two files share the same size, SHA-256 is
//            computed over the first 1MB and last 1MB only. If both boundaries
//            match, the files are treated as duplicates without reading the full content.
//
// Why boundary hashing?
//	Two files of equal size with identical first-and-last MB are 99.99%+ likely
//	to be identical. Reading only 2MB per file vs the full file (often 1-50GB)
//	makes dedup practical at scale without exhausting I/O or memory.
//
// Persistence:
//	FileRecord structs carry the hash bytes and timestamps. They can be
//	stored to SQLite (persistence.FileRecord) and loaded back on cold-start,
//	rebuilding the in-memory signature index without re-scanning files.
//
// Thread safety:
//	All public methods lock idx.mu. The Index is designed as a singleton
//	shared across all scanners and schedulers in the daemon.
package dedupe

import (
	"crypto/sha256"
	"io"
	"os"
	"sync"
)

type Hash32 [32]byte

type FileSignature struct {
	Size       int64
	FirstMBHash Hash32
	LastMBHash  Hash32
}

// DedupeIndex maintains a memory-efficient index of file signatures.
// Uses compact raw [32]byte arrays to minimize Go heap allocations and GC pauses.
type Index struct {
	mu          sync.RWMutex
	sizeIndex   map[int64][]string              // size -> list of paths
	sigIndex    map[int64]map[Hash32]string     // size -> (firstMBHash -> path)
	diskRecords map[string]*FileRecord // path -> stored record for disk persistency
}

// FileRecord is a persisted file signature that can be stored in a database.
type FileRecord struct {
	Path         string
	Size         int64
	FirstMBHash  []byte // 32 bytes
	LastMBHash   []byte // 32 bytes
	FirstSeen    string // RFC3339
	LastScanned  string // RFC3339
}

// NewIndex creates a new DedupeIndex.
func NewIndex() *Index {
	return &Index{
		sizeIndex:   make(map[int64][]string),
		sigIndex:    make(map[int64]map[Hash32]string),
		diskRecords: make(map[string]*FileRecord),
	}
}

// CheckAndRecord evaluates a file against the two-tier deduplication strategy:
// Tier 1: Check exact byte size. If unique, file cannot be a duplicate (zero hash I/O).
// Tier 2: If size matches an existing file, read & compute SHA-256 over first and last 1MB.
func (idx *Index) CheckAndRecord(path string, size int64) (isDuplicate bool, existingPath string, err error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	pathsWithSameSize, found := idx.sizeIndex[size]
	if !found {
		// Tier 1: Unique size - definitely unique
		idx.sizeIndex[size] = []string{path}
		// Load from disk if available
		if rec, diskFound := idx.diskRecords[path]; diskFound {
			// It was previously seen but not in memory (cold start)
			// Rebuild the in-memory hash if the size matches
			if rec.Size == size {
				var combinedHash Hash32
				copy(combinedHash[:], append(rec.FirstMBHash, rec.LastMBHash...))
				combinedHash = sha256.Sum256(append(rec.FirstMBHash, rec.LastMBHash...))
				if idx.sigIndex[size] == nil {
					idx.sigIndex[size] = make(map[Hash32]string)
				}
				idx.sigIndex[size][combinedHash] = rec.Path
			}
		}
		return false, "", nil
	}

	// Check if already recorded (same path)
	for _, existing := range pathsWithSameSize {
		if existing == path {
			return false, "", nil
		}
	}

	// Tier 2: Size collision detected! Compute boundary hashes
	firstHash, lastHash, err := ComputeBoundaryHashes(path, size)
	if err != nil {
		return false, "", err
	}
	combinedHash := sha256.Sum256(append(firstHash[:], lastHash[:]...))

	sizeSigs, exists := idx.sigIndex[size]
	if !exists {
		sizeSigs = make(map[Hash32]string)
		idx.sigIndex[size] = sizeSigs
	}

	if originalPath, hit := sizeSigs[combinedHash]; hit {
		return true, originalPath, nil
	}

	sizeSigs[combinedHash] = path
	idx.sizeIndex[size] = append(idx.sizeIndex[size], path)
	return false, "", nil
}

// AddDiskRecord adds a record from the disk to cold-start an index from previous scans.
func (idx *Index) AddDiskRecord(rec *FileRecord) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.diskRecords[rec.Path] = rec
}

// ForEachDiskRecord calls fn for each stored disk record.
func (idx *Index) ForEachDiskRecord(fn func(*FileRecord)) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for _, rec := range idx.diskRecords {
		fn(rec)
	}
}

// ForEachMemoryRecord calls fn for each in-memory record.
func (idx *Index) ForEachMemoryRecord(fn func(path string, size int64)) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for size, paths := range idx.sizeIndex {
		for _, path := range paths {
			fn(path, size)
		}
	}
}

// TotalUniqueFiles returns the count of unique files in the index.
func (idx *Index) TotalUniqueFiles() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.sizeIndex)
}

// ComputeBoundaryHashes reads up to 1MB from the start and end of the file.
func ComputeBoundaryHashes(path string, size int64) (firstHash Hash32, lastHash Hash32, err error) {
	f, err := os.Open(path)
	if err != nil {
		return firstHash, lastHash, err
	}
	defer f.Close()

	const mb = 1024 * 1024
	buf := make([]byte, mb)

	// Read first MB
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return firstHash, lastHash, err
	}
	firstHash = sha256.Sum256(buf[:n])

	// If file is smaller than 2MB, last hash equals first hash
	if size <= mb {
		return firstHash, firstHash, nil
	}

	// Read last MB
	seekPos := size - mb
	if seekPos < 0 {
		seekPos = 0
	}
	if _, err := f.Seek(seekPos, io.SeekStart); err != nil {
		return firstHash, lastHash, err
	}

	n, err = io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return firstHash, lastHash, err
	}
	lastHash = sha256.Sum256(buf[:n])

	return firstHash, lastHash, nil
}

// Clear removes all in-memory records (keeps disk records).
func (idx *Index) Clear() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.sizeIndex = make(map[int64][]string)
	idx.sigIndex = make(map[int64]map[Hash32]string)
}

// UpsertRecord represents a file signature to be persisted to DB.
type UpsertRecord struct {
	Path         string
	Size         int64
	FirstMBHash  []byte
	LastMBHash   []byte
	FirstSeen    string // empty = use current time
}
