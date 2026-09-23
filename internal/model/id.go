package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ContentHash returns the hex SHA-256 of the given bytes.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// FileID is content-addressed: identical normalized content yields the same
// file ID regardless of its path, so renamed files keep a stable identity
// and re-indexing an unchanged file never changes its ID.
func FileID(hash string) string {
	return "f_" + hash[:16]
}

// SymbolID is derived from the file content hash and the symbol's structural
// signature (kind, name, receiver, line/column). It stays stable across
// re-indexing of unchanged content and is independent of the file path.
func SymbolID(fileHash string, kind SymbolKind, receiver, name string, line, col int) string {
	raw := "s:" + fileHash + ":" + string(kind) + ":" + receiver + ":" + name +
		":" + itoa(line) + ":" + itoa(col)
	return "s_" + ContentHash([]byte(raw))[:16]
}

// RefID identifies a reference occurrence structurally.
func RefID(fileHash, name string, line, col int) string {
	raw := "r:" + fileHash + ":" + name + ":" + itoa(line) + ":" + itoa(col)
	return "r_" + ContentHash([]byte(raw))[:16]
}

// ChunkID identifies a chunk structurally by its source span and content.
func ChunkID(fileHash, kind, symbol string, start, end int, content string) string {
	raw := "c:" + fileHash + ":" + kind + ":" + symbol + ":" +
		itoa(start) + ":" + itoa(end) + ":" + content
	return "c_" + ContentHash([]byte(raw))[:16]
}

// FailureID keys a failure by path and category. A path that fails the same
// way across repeated indexing keeps a stable identity; changing category
// (e.g. file fixed then later broken differently) yields a new ID.
func FailureID(path string, cat FailureCategory) string {
	return "x_" + ContentHash([]byte("x:" + path + ":" + string(cat)))[:16]
}

// EstimateTokens returns a deterministic rough token count for budget
// accounting: whitespace-joined words plus a per-line overhead. The exact
// tokenizer is irrelevant to correctness; only determinism matters.
func EstimateTokens(text string) int {
	words := 0
	inWord := false
	lines := 1
	for _, r := range text {
		if r == '\n' {
			lines++
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			inWord = false
			continue
		}
		if !inWord {
			words++
			inWord = true
		}
	}
	n := words + lines/2
	if n < 1 && len(text) > 0 {
		n = 1
	}
	return n
}

// NormalizeContent converts CRLF to LF so line endings do not perturb hashes.
func NormalizeContent(data []byte) []byte {
	return []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
