package parser_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xingjianluo/code-index-service-go/internal/model"
	"github.com/xingjianluo/code-index-service-go/internal/parser"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseGoFileExtractsSymbolsRefsAndChunks(t *testing.T) {
	dir := t.TempDir()
	src := `package demo

import "fmt"

// Greeter greets people.
type Greeter struct{ Name string }

// Greet prints a greeting.
func (g *Greeter) Greet() {
	fmt.Println("hello " + g.Name)
}

func NewGreeter(name string) *Greeter {
	return &Greeter{Name: name}
}
`
	abs := filepath.Join(dir, "greeter.go")
	mustWrite(t, abs, src)

	p := parser.New(parser.Options{})
	out := p.ParseFile(abs, "greeter.go")
	if out.Failure != nil {
		t.Fatalf("expected success, got failure [%s]: %s", out.Failure.Category, out.Failure.Reason)
	}
	f := out.File
	t.Logf("KEY INPUT rel=%q size=%d hash=%s lines=%d", f.Path, f.Size, f.Hash, f.Lines)
	if f.Language != model.LangGo {
		t.Fatalf("language = %s, want go", f.Language)
	}
	if f.ID != model.FileID(f.Hash) {
		t.Fatalf("file id not content-derived: %s", f.ID)
	}

	names := map[string]model.SymbolKind{}
	for _, s := range f.Symbols {
		names[s.Name] = s.Kind
	}
	t.Logf("JUDGMENT symbols=%v", names)
	if names["Greeter"] != model.SymbolType {
		t.Fatalf("Greeter type missing, got %v", names)
	}
	if names["Greet"] != model.SymbolMethod {
		t.Fatalf("Greet method missing, got %v", names)
	}
	if names["NewGreeter"] != model.SymbolFunc {
		t.Fatalf("NewGreeter func missing, got %v", names)
	}

	// "fmt" must appear as a reference usage and Println should be a ref.
	refNames := map[string]int{}
	for _, r := range f.Refs {
		refNames[r.Name]++
	}
	t.Logf("JUDGMENT ref counts (subset) Println=%d Name=%d", refNames["Println"], refNames["Name"])
	if refNames["Println"] != 1 {
		t.Fatalf("Println ref count = %d, want 1", refNames["Println"])
	}

	kinds := map[string]int{}
	for _, c := range f.Chunks {
		kinds[c.Kind]++
		if c.FileID != f.ID || c.Path != "greeter.go" {
			t.Fatalf("chunk denormalization wrong: fileID=%s path=%s", c.FileID, c.Path)
		}
	}
	t.Logf("JUDGMENT chunks by kind=%v", kinds)
	if kinds["symbol"] != 2 { // Greet + NewGreeter
		t.Fatalf("symbol chunks = %d, want 2", kinds["symbol"])
	}
	if kinds["text"] < 1 {
		t.Fatalf("expected at least one text chunk (package/imports/type), got %d", kinds["text"])
	}
}

func TestFailureCategoriesAreDistinguishable(t *testing.T) {
	dir := t.TempDir()
	p := parser.New(parser.Options{MaxFileBytes: 32})

	cases := []struct {
		name           string
		setup          func(t *testing.T) string
		want           model.FailureCategory
		reasonContains string
	}{
		{
			name: "unreadable vanished file",
			setup: func(t *testing.T) string {
				return filepath.Join(dir, "ghost.go")
			},
			want: model.FailureUnreadable,
		},
		{
			name: "oversized",
			setup: func(t *testing.T) string {
				path := filepath.Join(dir, "big.txt")
				mustWrite(t, path, strings.Repeat("a", 64))
				return path
			},
			want: model.FailureOversized,
		},
		{
			name: "invalid encoding",
			setup: func(t *testing.T) string {
				path := filepath.Join(dir, "bad.txt")
				if err := os.WriteFile(path, []byte{'h', 'i', 0xff, 0xfe}, 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: model.FailureInvalidEncoding,
		},
		{
			name: "unsupported extension",
			setup: func(t *testing.T) string {
				path := filepath.Join(dir, "weird.xyz")
				mustWrite(t, path, "whatever")
				return path
			},
			want: model.FailureUnsupported,
		},
		{
			name: "go parse error",
			setup: func(t *testing.T) string {
				path := filepath.Join(dir, "broken.go")
				mustWrite(t, path, "package broken\nfunc oops( {\n")
				return path
			},
			want: model.FailureParse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			abs := tc.setup(t)
			rel := filepath.Base(abs)
			out := p.ParseFile(abs, rel)
			if out.File != nil {
				t.Fatalf("expected failure %s, got success", tc.want)
			}
			t.Logf("KEY INPUT rel=%q -> JUDGMENT category=%s reason=%q",
				rel, out.Failure.Category, out.Failure.Reason)
			if out.Failure.Category != tc.want {
				t.Fatalf("category = %s, want %s", out.Failure.Category, tc.want)
			}
			if out.Failure.ID != model.FailureID(rel, tc.want) {
				t.Fatalf("failure id not stable for path+category")
			}
		})
	}
}

func TestStableIDsAcrossRepeatedParses(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "a.go")
	src := "package a\nfunc F() int { return 1 }\n"
	mustWrite(t, abs, src)
	p := parser.New(parser.Options{})

	o1 := p.ParseFile(abs, "a.go")
	o2 := p.ParseFile(abs, "a.go")
	if o1.File.ID != o2.File.ID {
		t.Fatalf("file id changed across parses: %s vs %s", o1.File.ID, o2.File.ID)
	}
	if len(o1.File.Symbols) != len(o2.File.Symbols) || o1.File.Symbols[0].ID != o2.File.Symbols[0].ID {
		t.Fatalf("symbol ids unstable: %v vs %v", o1.File.Symbols, o2.File.Symbols)
	}
	// Moving the same content to another path must preserve the file id.
	o3 := p.ParseFile(abs, "renamed/deep/a.go")
	if o3.File.ID != o1.File.ID {
		t.Fatalf("file id changed with path (rename): %s vs %s", o3.File.ID, o1.File.ID)
	}
	t.Logf("JUDGMENT stable file id across rename: %s", o3.File.ID)
}

func TestNonRegularFilesAreUnreadable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	mustWrite(t, target, "hello")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	p := parser.New(parser.Options{})
	out := p.ParseFile(link, "link.txt")
	if out.Failure == nil {
		t.Fatalf("symlink must not be silently indexed")
	}
	t.Logf("KEY INPUT symlink -> JUDGMENT category=%s reason=%q",
		out.Failure.Category, out.Failure.Reason)
	if out.Failure.Category != model.FailureUnreadable {
		t.Fatalf("symlink category = %s, want unreadable", out.Failure.Category)
	}
}
