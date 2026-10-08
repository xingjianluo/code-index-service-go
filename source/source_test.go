package source_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/code-index-service-go/source"
)

func TestScanDeterministicAndFilters(t *testing.T) {
	root := t.TempDir()
	mk := func(p, content string) {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("z.go", "package z")
	mk("a/b.go", "package b")
	mk("a/skip.me", "ignore")
	mk(".git/config", "[core]")
	mk("node_modules/x.js", "// ignored")
	mk("readme.md", "# hi")

	sc := source.NewScanner(source.Options{Root: root})
	files1, excluded1, err := sc.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	files2, excluded2, err := sc.Scan()
	if err != nil {
		t.Fatalf("scan2: %v", err)
	}
	var rels []string
	for _, f := range files1 {
		rels = append(rels, f.RelPath)
		t.Logf("discovered %s size=%d", f.RelPath, f.Size)
	}
	want := []string{"a/b.go", "readme.md", "z.go"}
	if len(files1) != len(want) {
		t.Fatalf("got %v want %v", rels, want)
	}
	for i := range want {
		if files1[i].RelPath != want[i] || files2[i].RelPath != want[i] {
			t.Fatalf("scan order nondeterministic or wrong: %v / %v", rels, want)
		}
	}
	// Files outside the include set are reported, not dropped silently.
	t.Logf("input: a/skip.me has unlisted extension .me; judgment: excluded_extension")
	if len(excluded1) != 1 || excluded1[0].RelPath != "a/skip.me" ||
		excluded1[0].Reason != source.ReasonExcludedExtension {
		t.Fatalf("excluded: want [a/skip.me excluded_extension], got %+v", excluded1)
	}
	if len(excluded2) != 1 || excluded2[0].RelPath != "a/skip.me" {
		t.Fatalf("excluded list must be deterministic across scans: %+v / %+v", excluded1, excluded2)
	}
	t.Logf("excluded %s reason=%s size=%d", excluded1[0].RelPath, excluded1[0].Reason, excluded1[0].Size)
}

func TestScanMissingRootIsError(t *testing.T) {
	sc := source.NewScanner(source.Options{Root: filepath.Join(t.TempDir(), "nope")})
	if _, _, err := sc.Scan(); err == nil {
		t.Fatal("missing root must return an error, not an empty file list")
	} else {
		t.Logf("missing root rejected: %v", err)
	}
}
