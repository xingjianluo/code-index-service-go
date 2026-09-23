package parser_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/source"
)

func TestTokenizeV2(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"GetUserName2FA", []string{"get", "user", "name", "2", "fa"}},
		{"snake_case-kebab", []string{"snake", "case", "kebab"}},
		{"HTTPServer", []string{"http", "server"}},
		{"MixedCASEWord", []string{"mixed", "case", "word"}},
		{"foo 123 BAR", []string{"foo", "123", "bar"}},
		{"!!! ???", nil},
	}
	for _, c := range cases {
		got := parser.Tokenize(c.in)
		if !eqSlice(got, c.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", c.in, got, c.want)
		}
		t.Logf("input=%q tokens=%v", c.in, got)
	}
	// Determinism: repeated calls identical.
	a := parser.Tokenize("TheQuickBrownFox123")
	b := parser.Tokenize("TheQuickBrownFox123")
	if !eqSlice(a, b) {
		t.Fatalf("tokenize nondeterministic: %v vs %v", a, b)
	}
}

func eqSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const sampleGo = `package main

import "fmt"

type Account struct {
	Owner string
	Balance int
}

func (a *Account) Deposit(amount int) {
	a.Balance = a.Balance + amount
	fmt.Println(a.Owner, amount)
}

func main() {
	acc := &Account{Owner: "x", Balance: 0}
	acc.Deposit(10)
}
`

func parseTemp(t *testing.T, name, content string, maxSize int64) (*model.FileRecord, *model.Failure) {
	t.Helper()
	dir := t.TempDir()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := parser.New(parser.Options{MaxFileSize: maxSize})
	rel, err := filepath.Rel(dir, full)
	if err != nil {
		t.Fatal(err)
	}
	return p.ParseFile(context.Background(), source.DiscoveredFile{
		RelPath: filepath.ToSlash(rel), AbsPath: full, Size: int64(len(content)),
	})
}

func TestParseGoSymbolsAndChunks(t *testing.T) {
	rec, fail := parseTemp(t, "main.go", sampleGo, 1<<20)
	if fail != nil {
		t.Fatalf("unexpected failure: %+v", fail)
	}
	kinds := map[model.SymbolKind]int{}
	for _, s := range rec.Symbols {
		kinds[s.Kind]++
		t.Logf("symbol kind=%s name=%s line=%d id=%s", s.Kind, s.Name, s.Line, s.ID)
	}
	if kinds[model.SymbolType] != 1 || kinds[model.SymbolField] != 2 || kinds[model.SymbolMethod] != 1 || kinds[model.SymbolFunc] != 1 {
		t.Fatalf("unexpected symbol kind counts: %+v", kinds)
	}
	if len(rec.Chunks) == 0 {
		t.Fatal("expected chunks")
	}
	for _, c := range rec.Chunks {
		if c.ID == "" || c.ContentHash == "" || len(c.Tokens) == 0 {
			t.Fatalf("chunk missing identity/tokens: %+v", c)
		}
	}
	// Stability: parse identical content twice -> identical ids/hashes.
	rec2, _ := parseTemp(t, "main.go", sampleGo, 1<<20)
	if rec2.ContentHash != rec.ContentHash {
		t.Fatal("content hash unstable for identical bytes")
	}
	if len(rec2.Symbols) != len(rec.Symbols) {
		t.Fatal("symbol count unstable")
	}
	for i := range rec.Symbols {
		if rec.Symbols[i].ID != rec2.Symbols[i].ID {
			t.Fatalf("symbol id drift: %s vs %s", rec.Symbols[i].ID, rec2.Symbols[i].ID)
		}
	}
}

func TestParseFailuresDistinguishable(t *testing.T) {
	_, f1 := parseTemp(t, "x.go", "package main\nfunc Bad( {\n", 1<<20)
	if f1 == nil || f1.Category != model.FailureUnparseable {
		t.Fatalf("want unparseable, got %+v", f1)
	}
	t.Logf("bad syntax => category=%s message=%q", f1.Category, f1.Message)

	_, f2 := parseTemp(t, "x.go", "package main\nvar s = \"\xff\"\n", 1<<20)
	if f2 == nil || f2.Category != model.FailureInvalidUTF8 {
		t.Fatalf("want invalid_utf8, got %+v", f2)
	}
	t.Logf("bad encoding => category=%s size=%d", f2.Category, f2.Size)

	_, f3 := parseTemp(t, "x.txt", "0123456789", 5)
	if f3 == nil || f3.Category != model.FailureOversize {
		t.Fatalf("want oversize, got %+v", f3)
	}
	t.Logf("oversize => category=%s message=%q", f3.Category, f3.Message)

	// Missing file: scanned then deleted before parse.
	dir := t.TempDir()
	full := filepath.Join(dir, "gone.go")
	p := parser.New(parser.Options{})
	_, f4 := p.ParseFile(context.Background(), source.DiscoveredFile{RelPath: "gone.go", AbsPath: full})
	if f4 == nil || (f4.Category != model.FailureMissing && f4.Category != model.FailureUnreadable) {
		t.Fatalf("want missing/unreadable for vanished file, got %+v", f4)
	}
	t.Logf("vanished file => category=%s message=%q", f4.Category, f4.Message)
}

func TestResolveReferences(t *testing.T) {
	rec1, fail1 := parseTemp(t, "a.go", "package main\n\nfunc Target() int { return 1 }\n", 1<<20)
	if fail1 != nil {
		t.Fatal(fail1)
	}
	rec2, fail2 := parseTemp(t, "b.go", "package main\n\nfunc Caller() int { return Target() }\n", 1<<20)
	if fail2 != nil {
		t.Fatal(fail2)
	}
	recs := []*model.FileRecord{rec2, rec1} // intentionally shuffled
	p := parser.New(parser.Options{})
	p.ResolveReferences(recs)

	var targetID string
	for _, s := range rec1.Symbols {
		if s.Name == "Target" {
			targetID = s.ID
		}
	}
	resolved := 0
	for _, r := range rec2.References {
		if r.Name == "Target" {
			t.Logf("reference Target at %s:%d -> symbol %s", r.Path, r.Line, r.SymbolID)
			if r.SymbolID != targetID {
				t.Fatalf("reference Target resolved to %s, want %s", r.SymbolID, targetID)
			}
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("expected exactly 1 resolved Target reference, got %d", resolved)
	}
}
