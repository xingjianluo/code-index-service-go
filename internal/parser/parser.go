// Package parser turns repository source files into normalized model.File
// representations, or into categorized model.Failure values.
package parser

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/xingjianluo/code-index-service-go/internal/model"
)

// DefaultMaxFileBytes is the default per-file size cap (256 KiB).
const DefaultMaxFileBytes int64 = 256 * 1024

// textChunkLines bounds the number of source lines in one text chunk.
const textChunkLines = 40

// Options configures parsing limits.
type Options struct {
	// MaxFileBytes rejects files larger than this (0 -> default).
	MaxFileBytes int64
	// SupportedExtensions maps lower-cased extension (".go") to language.
	// A nil map selects the built-in default set.
	SupportedExtensions map[string]model.Language
}

func (o Options) withDefaults() Options {
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.SupportedExtensions == nil {
		o.SupportedExtensions = map[string]model.Language{
			".go":  model.LangGo,
			".txt": model.LangText,
			".md":  model.LangMarkdown,
			".mod": model.LangText,
		}
	}
	return o
}

// Parser parses individual files under the configured limits.
type Parser struct {
	opts Options
}

// New constructs a Parser with defaults filled in.
func New(opts Options) *Parser {
	return &Parser{opts: opts.withDefaults()}
}

// Outcome is the result of attempting one file. Exactly one field is set.
type Outcome struct {
	File    *model.File
	Failure *model.Failure
}

func fail(relPath string, cat model.FailureCategory, reason string, size int64) Outcome {
	return Outcome{Failure: &model.Failure{
		ID:       model.FailureID(relPath, cat),
		Path:     relPath,
		Category: cat,
		Reason:   reason,
		SizeHint: size,
	}}
}

// ParseFile reads, validates and parses the file at abs with repo-relative
// relPath. Failures are categorized and returned, never silently skipped:
//
//	unreadable        -> stat/read failed or path is not a regular file
//	oversized         -> size exceeds Options.MaxFileBytes
//	invalid_encoding  -> content is not valid UTF-8
//	unsupported_type  -> extension has no registered parser
//	parse_error       -> language parser rejected the content
func (p *Parser) ParseFile(abs, relPath string) Outcome {
	st, err := os.Lstat(abs)
	if err != nil {
		return fail(relPath, model.FailureUnreadable, "stat: "+err.Error(), 0)
	}
	if !st.Mode().IsRegular() {
		return fail(relPath, model.FailureUnreadable,
			fmt.Sprintf("not a regular file (mode %s)", st.Mode().Type()), st.Size())
	}
	if st.Size() > p.opts.MaxFileBytes {
		return fail(relPath, model.FailureOversized,
			fmt.Sprintf("size %d exceeds limit %d bytes", st.Size(), p.opts.MaxFileBytes),
			st.Size())
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return fail(relPath, model.FailureUnreadable, "read: "+err.Error(), st.Size())
	}
	if !utf8.Valid(raw) {
		return fail(relPath, model.FailureInvalidEncoding,
			fmt.Sprintf("invalid UTF-8 byte sequence at offset %d", firstInvalidUTF8(raw)),
			int64(len(raw)))
	}

	data := model.NormalizeContent(raw)
	ext := strings.ToLower(filepath.Ext(relPath))
	lang, ok := p.opts.SupportedExtensions[ext]
	if !ok {
		return fail(relPath, model.FailureUnsupported,
			"no parser registered for extension "+ext, int64(len(data)))
	}

	hash := model.ContentHash(data)
	switch lang {
	case model.LangGo:
		return parseGoSource(relPath, data, hash)
	default:
		return parsePlainText(relPath, lang, data, hash)
	}
}

func firstInvalidUTF8(data []byte) int {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}

// lineOffsets returns the byte offset of the start of each 1-based line.
// offset[0] is unused (or zero); offset[1] starts line 1.
func lineOffsets(data []byte) []int {
	off := []int{0, 0}
	for i, b := range data {
		if b == '\n' {
			off = append(off, i+1)
		}
	}
	return off
}

// sliceLines returns the text of inclusive 1-based lines start..end.
func sliceLines(data []byte, offsets []int, start, end int) string {
	if start < 1 || end < start || start >= len(offsets) {
		return ""
	}
	lo := offsets[start]
	if end+1 < len(offsets) {
		// exclude the trailing newline of the last line
		hi := offsets[end+1] - 1
		if hi > lo {
			return string(data[lo:hi])
		}
	}
	return string(data[lo:])
}

// chunkBlocks groups 1-based line numbers into deterministic chunks of at
// most textChunkLines lines, preferring blank-line boundaries.
func chunkBlocks(totalLines int, isBlank func(line int) bool) [][2]int {
	var blocks [][2]int
	start := 1
	for start <= totalLines {
		hardEnd := start + textChunkLines - 1
		if hardEnd > totalLines {
			hardEnd = totalLines
		}
		end := hardEnd
		if hardEnd-start+1 == textChunkLines && hardEnd < totalLines {
			// window full and more follows: split at the last blank line
			for ln := hardEnd; ln > start; ln-- {
				if isBlank(ln) {
					end = ln - 1
					break
				}
			}
			if end < start {
				end = hardEnd
			}
		}
		blocks = append(blocks, [2]int{start, end})
		start = end + 1
	}
	return blocks
}

func parsePlainText(relPath string, lang model.Language, data []byte, hash string) Outcome {
	offsets := lineOffsets(data)
	totalLines := len(offsets) - 1
	lines := bytes.Split(data, []byte("\n"))
	isBlank := func(ln int) bool {
		if ln < 1 || ln > len(lines) {
			return false
		}
		return len(bytes.TrimSpace(lines[ln-1])) == 0
	}

	f := &model.File{
		ID:       model.FileID(hash),
		Path:     relPath,
		Language: lang,
		Size:     int64(len(data)),
		Hash:     hash,
		Lines:    totalLines,
		Symbols:  []model.Symbol{},
		Refs:     []model.Reference{},
	}
	for _, span := range chunkBlocks(totalLines, isBlank) {
		content := sliceLines(data, offsets, span[0], span[1])
		f.Chunks = append(f.Chunks, model.Chunk{
			ID:        model.ChunkID(hash, "text", "", span[0], span[1], content),
			FileID:    f.ID,
			Path:      relPath,
			Language:  lang,
			StartLine: span[0],
			EndLine:   span[1],
			Kind:      "text",
			Content:   content,
			Tokens:    model.EstimateTokens(content),
		})
	}
	return Outcome{File: f}
}
