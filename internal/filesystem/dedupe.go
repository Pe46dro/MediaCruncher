package filesystem

import (
	"crypto/sha256"
	"io"
	"os"
	"sync"
)

type Hash32 [32]byte

type FileSignature struct {
	Size         int64
	FirstMBHash  Hash32
	LastMBHash   Hash32
}

// DedupeIndex maintains a memory-efficient index of file signatures.
// Uses compact raw [32]byte arrays to minimize Go heap allocations and GC pauses.
type DedupeIndex struct {
	mu           sync.RWMutex
	sizeIndex    map[int64][]string             // size -> list of paths
	sigIndex     map[int64]map[Hash32]string     // size -> (firstMBHash -> path)
}

func NewDedupeIndex() *DedupeIndex {
	return &DedupeIndex{
		sizeIndex: make(map[int64][]string),
		sigIndex:  make(map[int64]map[Hash32]string),
	}
}

// CheckAndRecord evaluates a file against the two-tier deduplication strategy:
// Tier 1: Check exact byte size. If unique, file cannot be a duplicate (zero hash I/O).
// Tier 2: If size matches an existing file, read & compute SHA-256 over first and last 1MB.
func (idx *DedupeIndex) CheckAndRecord(path string, size int64) (isDuplicate bool, existingPath string, err error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	pathsWithSameSize, found := idx.sizeIndex[size]
	if !found {
		// Tier 1: Unique size - definitely unique
		idx.sizeIndex[size] = []string{path}
		return false, "", nil
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

		// Compute combined boundary hashes for all prior files sharing this size
		for _, prevPath := range pathsWithSameSize {
			if pFirst, pLast, err := ComputeBoundaryHashes(prevPath, size); err == nil {
				pCombined := sha256.Sum256(append(pFirst[:], pLast[:]...))
				sizeSigs[pCombined] = prevPath
			}
		}
	}

	if originalPath, hit := sizeSigs[combinedHash]; hit {
		return true, originalPath, nil
	}

	sizeSigs[combinedHash] = path
	idx.sizeIndex[size] = append(idx.sizeIndex[size], path)
	return false, "", nil
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
