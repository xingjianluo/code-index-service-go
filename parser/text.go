package parser

import (
	"strings"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/source"
)

// parseText handles non-Go textual source. No language symbols are claimed;
// the file is cut into line-bounded chunks so retrieval stays budgeted.
func parseText(f source.DiscoveredFile, content string, size int64, fileHash string, opts Options) (*model.FileRecord, *parseFailure) {
	lines := splitLines(content)
	rec := &model.FileRecord{
		Path:       f.RelPath,
		Language:   "text",
		Size:       size,
		Lines:      len(lines),
		Symbols:    []model.Symbol{},
		References: []model.Reference{},
		Metadata: map[string]string{
			"parser": "text-v2",
		},
	}
	chunks := lineChunks(f.RelPath, lines, fileHash, opts.MaxChunkLines, "text", f.RelPath)
	rec.Chunks = chunks
	return rec, nil
}

// splitLines splits content into lines without trailing newline characters.
func splitLines(content string) []string {
	if content == "" {
		return []string{""}
	}
	return strings.Split(content, "\n")
}

// lineChunks groups 1-based line ranges into bounded chunks and computes
// stable per-chunk hashes/IDs. ContentHash of the record is filled by the
// caller; chunk hashes hash the chunk's own bytes, so stable regardless.
func lineChunks(path string, lines []string, fileHash string, maxLines int, kind, title string) []model.Chunk {
	if maxLines <= 0 {
		maxLines = 60
	}
	var chunks []model.Chunk
	for start := 0; start < len(lines); start += maxLines {
		end := start + maxLines
		if end > len(lines) {
			end = len(lines)
		}
		body := strings.Join(lines[start:end], "\n")
		if strings.TrimSpace(body) == "" && len(lines) > 1 {
			continue
		}
		ch := model.Chunk{
			Path:        path,
			StartLine:   start + 1,
			EndLine:     end,
			Kind:        kind,
			Content:     body,
			Tokens:      Tokenize(body),
			ContentHash: model.HashBytes([]byte(body)),
		}
		if start == 0 {
			ch.Title = title
		}
		ch.ID = model.StableChunkID(path, ch.StartLine, ch.EndLine, fileHash, ch.ContentHash)
		chunks = append(chunks, ch)
	}
	return chunks
}
