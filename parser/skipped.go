package parser

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"unicode"
	"unicode/utf8"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/source"
)

const (
	// sniffLimit bounds how many bytes of an excluded file are inspected
	// when the file exceeds the parse size limit.
	sniffLimit = 32 << 10
	// maxSkippedTokens caps the distinct identifiers kept per skipped text
	// file; the cap is deterministic (first-seen order) and recorded via
	// TokensTruncated so queries know name presence is only partially known.
	maxSkippedTokens = 4096
	// maxSkippedTokenLen drops garbage runs instead of storing them.
	maxSkippedTokenLen = 128
)

// InspectSkipped classifies one scan-excluded file (extension outside the
// include set) into a model.SkippedFile. The file is never silently dropped:
//
//   - content provably not analyzable text (NUL byte or invalid UTF-8) ->
//     SkipNonText; no tokens are extracted;
//   - otherwise -> SkipExtensionNotIndexed; the distinct raw identifiers
//     (case preserved) are recorded so relation queries can decide name
//     presence from the snapshot alone, without re-reading the disk.
//
// Files larger than maxFileSize are inspected on a bounded prefix only;
// TokensTruncated then also marks the token list as prefix-derived.
// An unreadable file is still recorded (extension category, no tokens) with
// the read error in Detail.
func InspectSkipped(f source.DiscoveredFile, maxFileSize int64) model.SkippedFile {
	sk := model.SkippedFile{
		Path:     f.RelPath,
		Category: model.SkipExtensionNotIndexed,
		Size:     f.Size,
	}
	if maxFileSize <= 0 {
		maxFileSize = 2 << 20
	}

	fi, statErr := os.Stat(f.AbsPath)
	if statErr != nil {
		sk.Detail = fmt.Sprintf("extension not in include set; content could not be inspected (stat: %v)", statErr)
		return sk
	}
	size := fi.Size()
	sk.Size = size

	data, readErr := readPrefix(f.AbsPath, size, maxFileSize)
	if readErr != nil {
		sk.Detail = fmt.Sprintf("extension not in include set; content could not be inspected (read: %v)", readErr)
		return sk
	}
	complete := int64(len(data)) == size
	if complete {
		sk.ContentHash = model.HashBytes(data)
	}

	if !looksText(data, complete) {
		sk.Category = model.SkipNonText
		sk.Tokens = nil
		sk.Detail = "content is not analyzable text (NUL byte or invalid UTF-8); no symbol-level analysis applies"
		return sk
	}

	tokens, truncated := rawIdentifiers(string(data), maxSkippedTokens)
	if !complete {
		truncated = true
	}
	sk.Tokens = tokens
	sk.TokensTruncated = truncated
	sk.Detail = "extension not in include set; content is analyzable text but was never parsed"
	if !complete {
		sk.Detail = fmt.Sprintf("extension not in include set; text inspected on first %d of %d bytes only (size limit)", len(data), size)
	}
	return sk
}

// readPrefix reads up to limit bytes of path (the whole file when it fits).
func readPrefix(path string, size, limit int64) ([]byte, error) {
	if size <= limit {
		return os.ReadFile(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, sniffLimit)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

// looksText reports whether b is analyzable text: no NUL bytes and valid
// UTF-8. When complete is false, b is a prefix and an invalid-looking tail
// may just be a multi-byte rune cut in half, so only invalid sequences that
// cannot be a truncation artifact count.
func looksText(b []byte, complete bool) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if utf8.Valid(b) {
		return true
	}
	if complete {
		return false
	}
	// Prefix read: tolerate one partial rune at the very end.
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		b = b[:len(b)-1]
		if utf8.Valid(b) {
			return true
		}
	}
	return false
}

// rawIdentifiers extracts the distinct raw identifier-like runs (letters,
// digits, underscore; case preserved) in first-seen order, then sorts them
// for deterministic storage. Runs longer than maxSkippedTokenLen are dropped.
// truncated reports that the cap was hit (or would have been).
func rawIdentifiers(content string, max int) (tokens []string, truncated bool) {
	seen := make(map[string]bool, 256)
	var out []string
	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		if len(run) <= maxSkippedTokenLen {
			tok := string(run)
			if !seen[tok] {
				seen[tok] = true
				if len(out) < max {
					out = append(out, tok)
				} else {
					truncated = true
				}
			}
		}
		run = run[:0]
	}
	for _, r := range content {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			run = append(run, r)
			continue
		}
		flush()
	}
	flush()
	sort.Strings(out)
	return out, truncated
}
