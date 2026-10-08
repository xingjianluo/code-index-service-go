package parser_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/source"
)

func inspect(t *testing.T, root, rel string, maxSize int64) model.SkippedFile {
	t.Helper()
	return parser.InspectSkipped(source.DiscoveredFile{
		RelPath: rel,
		AbsPath: filepath.Join(root, filepath.FromSlash(rel)),
	}, maxSize)
}

// InspectSkipped assigns the distinguishable categories: analyzable text with
// an unlisted extension keeps its identifiers; binary content is non_text.
func TestInspectSkippedCategories(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tpl/page.tmpl", "hello Helper world\n")
	write("bin/blob.dat", "\x00\x01not text\xff")
	write("big/huge.dat", strings.Repeat("Alpha Beta ", 4000)) // ~44 KiB

	text := inspect(t, root, "tpl/page.tmpl", 2<<20)
	t.Logf("input: tpl/page.tmpl (text, unlisted ext); judgment: extension_not_indexed with raw tokens")
	if text.Category != model.SkipExtensionNotIndexed {
		t.Fatalf("want extension_not_indexed, got %+v", text)
	}
	found := false
	for _, tok := range text.Tokens {
		if tok == "Helper" {
			found = true
		}
	}
	if !found {
		t.Fatalf("raw identifier Helper must be kept (case preserved), got %v", text.Tokens)
	}
	if text.ContentHash == "" {
		t.Fatalf("fully inspected text file must carry a content hash")
	}

	bin := inspect(t, root, "bin/blob.dat", 2<<20)
	t.Logf("input: bin/blob.dat (NUL + invalid UTF-8); judgment: non_text, no tokens")
	if bin.Category != model.SkipNonText || bin.Tokens != nil {
		t.Fatalf("want non_text without tokens, got %+v", bin)
	}

	huge := inspect(t, root, "big/huge.dat", 1024)
	t.Logf("input: big/huge.dat over the 1024-byte limit; judgment: prefix-inspected, tokens marked truncated")
	if huge.Category != model.SkipExtensionNotIndexed || !huge.TokensTruncated {
		t.Fatalf("want extension_not_indexed with truncated tokens, got %+v", huge)
	}
	if huge.ContentHash != "" {
		t.Fatalf("partially inspected file must not carry a full-content hash")
	}

	missing := inspect(t, root, "ghost/nope.dat", 2<<20)
	t.Logf("input: ghost/nope.dat vanished after scan; judgment: still recorded, inspection error in detail")
	if missing.Category != model.SkipExtensionNotIndexed || !strings.Contains(missing.Detail, "could not be inspected") {
		t.Fatalf("unreadable file must still be recorded explicitly, got %+v", missing)
	}
}
