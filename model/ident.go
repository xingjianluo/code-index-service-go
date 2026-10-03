package model

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Identity rules
//
// ContentHash: SHA-256 of raw file bytes, hex encoded. Identical content at
// any path always has the same hash; it drives incremental skip/rename logic.
//
// SymbolID:   sha256("sym\x00" + path + "\x00" + name + "\x00" + kind +
//
//	"\x00" + line). Path-relative so renaming a file changes the IDs it
//	contains (the symbols move), while unchanged content at the same path
//	keeps identical IDs across repeated indexings.
//
// SymbolKey:  sha256("skey\x00" + name + "\x00" + kind + "\x00" + line +
//
//	"\x00" + fileContentHash). Path-INDEPENDENT: a pure rename (identical
//	bytes at a new path) keeps the same key, so "the same symbol" can be
//	recognized across moves; any content edit yields a new key.
//
// ChunkID:    sha256("chk\x00" + path + "\x00" + startLine + ":" + endLine +
//
//	"\x00" + fileContentHash). The file-level content hash is part of the
//	identity so a fragment distinguishes the file revision it belongs to:
//	same file revision + same path/lines => stable ID across reindexings;
//	any edit to the file changes every chunk ID, making stale fragments
//	unreachable.
//
// SnapshotID: sha256 of the canonical (sorted) file content-hash list plus
// failure list. Same source state => same snapshot ID, independent of which
// incremental operations produced it.

// HashBytes returns the hex SHA-256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// StableSymbolID derives the stable ID of a symbol.
func StableSymbolID(path, name string, kind SymbolKind, line int) string {
	return shortHash("sym", path, name, string(kind), itoa(line))
}

// StableSymbolKey derives the path-independent identity of a symbol:
//
//	sha256("skey\x00" + name + "\x00" + kind + "\x00" + line + "\x00" +
//	fileContentHash)
//
// The containing file's raw-content hash anchors the key to the exact file
// revision, so a pure rename or directory move (identical bytes) keeps the
// key while any content edit changes it. The line is included so two
// same-name, same-kind definitions inside one file revision (e.g. two
// struct fields called ID) remain distinguishable; identical content
// guarantees identical lines, so this does not break rename continuity.
func StableSymbolKey(name string, kind SymbolKind, line int, fileContentHash string) string {
	return shortHash("skey", name, string(kind), itoa(line), fileContentHash)
}

// StableChunkID derives the stable ID of a chunk. fileContentHash is the
// containing file's raw-content hash; chunkContentHash is the fragment's own
// bytes hash (retained on the chunk for content-level checks).
func StableChunkID(path string, startLine, endLine int, fileContentHash, chunkContentHash string) string {
	return shortHash("chk", path, itoa(startLine)+":"+itoa(endLine), fileContentHash, chunkContentHash)
}

// SnapshotID derives a deterministic identity from the indexed state.
// File paths are sorted; failures are included so that identical inputs
// (including identical bad inputs) yield identical IDs.
func SnapshotID(files map[string]*FileRecord, failures []Failure) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		f := files[p]
		b.WriteString(p)
		b.WriteByte(0)
		b.WriteString(f.ContentHash)
		b.WriteByte(0)
	}
	// Deterministic failure contribution.
	sorted := make([]Failure, len(failures))
	copy(sorted, failures)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		return sorted[i].Category < sorted[j].Category
	})
	for _, fl := range sorted {
		b.WriteString(string(fl.Category))
		b.WriteByte(0)
		b.WriteString(fl.Path)
		b.WriteByte(0)
		b.WriteString(fl.Message)
		b.WriteByte(0)
	}
	return shortHash("snap", b.String())
}

func itoa(n int) string { return strconv.Itoa(n) }

func shortHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
