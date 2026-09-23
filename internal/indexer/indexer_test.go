package indexer_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/xingjianluo/code-index-service-go/internal/indexer"
	"github.com/xingjianluo/code-index-service-go/internal/model"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeAll(t *testing.T, p string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, root string, prev *model.Snapshot) (*model.Snapshot, indexer.ScanResult) {
	t.Helper()
	ix := indexer.New(indexer.Options{Root: root})
	snap, res, err := ix.BuildSnapshot(context.Background(), prev)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	return snap, res
}

func TestIncrementalAddUpdateDeleteRename(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package demo\nfunc Alpha() {}\n")
	writeFile(t, root, "notes.txt", "first line\nsecond line\n")

	s1, r1 := build(t, root, nil)
	t.Logf("KEY INPUT initial files a.go+notes.txt -> JUDGMENT gen=%d added=%v failures=%v",
		s1.Generation, r1.Added, r1.Failures)
	if s1.Generation != 1 || len(s1.Files) != 2 {
		t.Fatalf("initial snapshot wrong: gen=%d files=%d", s1.Generation, len(s1.Files))
	}
	alphaID := s1.Files["a.go"].ID

	// 1) Add a file.
	writeFile(t, root, "b.go", "package demo\nfunc Beta() {}\n")
	s2, r2 := build(t, root, s1)
	t.Logf("add b.go -> JUDGMENT added=%v unchanged=%v gen=%d", r2.Added, r2.Unchanged, s2.Generation)
	if !reflect.DeepEqual(r2.Added, []string{"b.go"}) {
		t.Fatalf("added = %v, want [b.go]", r2.Added)
	}
	if s2.Files["a.go"].ID != alphaID {
		t.Fatalf("unchanged a.go id mutated: %s -> %s", alphaID, s2.Files["a.go"].ID)
	}
	if s2.Generation != 2 {
		t.Fatalf("generation = %d, want 2", s2.Generation)
	}

	// 2) Modify a file: old chunks must disappear from retrieval space.
	writeFile(t, root, "a.go", "package demo\nfunc AlphaRenamed() {}\n")
	s3, r3 := build(t, root, s2)
	t.Logf("modify a.go -> JUDGMENT updated=%v gen=%d", r3.Updated, s3.Generation)
	if !reflect.DeepEqual(r3.Updated, []string{"a.go"}) {
		t.Fatalf("updated = %v, want [a.go]", r3.Updated)
	}
	if s3.Files["a.go"].ID == alphaID {
		t.Fatalf("modified file kept stale content-addressed id")
	}
	for _, c := range s3.Files["a.go"].Chunks {
		if c.SymbolName == "Alpha" {
			t.Fatalf("old symbol chunk Alpha survived update")
		}
	}

	// 3) Rename with identical content -> rename, not add+delete.
	if err := os.Rename(filepath.Join(root, "b.go"), filepath.Join(root, "b_renamed.go")); err != nil {
		t.Fatal(err)
	}
	s4, r4 := build(t, root, s3)
	t.Logf("rename b.go -> JUDGMENT renames=%v deleted=%v added=%v",
		r4.Renames, r4.Deleted, r4.Added)
	if src, ok := r4.Renames["b_renamed.go"]; !ok || src != "b.go" {
		t.Fatalf("rename detection wrong: %v", r4.Renames)
	}
	if len(r4.Added) != 0 || len(r4.Deleted) != 0 {
		t.Fatalf("rename should not surface as add/delete: added=%v deleted=%v",
			r4.Added, r4.Deleted)
	}
	if _, present := s4.Files["b.go"]; present {
		t.Fatalf("old path b.go still present after rename")
	}
	if s4.Files["b_renamed.go"].ID != s2.Files["b.go"].ID {
		t.Fatalf("renamed file id changed; identity must follow content")
	}

	// 4) Delete a file: it must be gone and reported exactly once.
	removeAll(t, filepath.Join(root, "notes.txt"))
	s5, r5 := build(t, root, s4)
	t.Logf("delete notes.txt -> JUDGMENT deleted=%v gen=%d", r5.Deleted, s5.Generation)
	if !reflect.DeepEqual(r5.Deleted, []string{"notes.txt"}) {
		t.Fatalf("deleted = %v, want [notes.txt]", r5.Deleted)
	}
	if _, present := s5.Files["notes.txt"]; present {
		t.Fatalf("deleted file still present in snapshot")
	}
}

func TestReindexSameTreeIsIdentical(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "x.go", "package demo\nfunc X() {}\n")
	s1, _ := build(t, root, nil)
	s2, r2 := build(t, root, s1)
	t.Logf("KEY INPUT unchanged tree -> JUDGMENT gen s1=%d s2=%d unchanged=%v",
		s1.Generation, s2.Generation, r2.Unchanged)
	if s2.Generation != s1.Generation {
		t.Fatalf("generation bumped without changes: %d -> %d", s1.Generation, s2.Generation)
	}
	if len(r2.Added)+len(r2.Updated)+len(r2.Deleted)+len(r2.Renames) != 0 {
		t.Fatalf("spurious delta on unchanged tree: %+v", r2)
	}
	// File object identity is reused: guarantees stable embedded IDs.
	if s1.Files["x.go"] != s2.Files["x.go"] {
		t.Fatalf("unchanged file entry was rebuilt instead of carried over")
	}
	// Two independent builds from nil (no shared pointers) must be equal too.
	sA, _ := build(t, root, nil)
	sB, _ := build(t, root, nil)
	if !snapshotsEqual(sA, sB) {
		t.Fatalf("fresh rebuilds of same tree differ")
	}
}

func TestFailuresAreKeptNotSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ok.go", "package demo\nfunc OK() {}\n")
	writeFile(t, root, "broken.go", "package demo\nfunc broken( {\n")
	s1, r1 := build(t, root, nil)
	t.Logf("KEY INPUT one good + one broken go file -> JUDGMENT files=%d failures=%v",
		len(s1.Files), r1.Failures)
	if len(s1.Files) != 1 || s1.Files["ok.go"] == nil {
		t.Fatalf("good file missing: files=%v", keys(s1.Files))
	}
	fx, ok := s1.Failures["broken.go"]
	if !ok {
		t.Fatalf("parse failure not recorded")
	}
	if fx.Category != model.FailureParse {
		t.Fatalf("failure category = %s, want parse_error", fx.Category)
	}

	// Fix the file: failure must be replaced by a file entry.
	writeFile(t, root, "broken.go", "package demo\nfunc Fixed() {}\n")
	s2, r2 := build(t, root, s1)
	t.Logf("fix broken.go -> JUDGMENT files=%d failures=%d updated=%v",
		len(s2.Files), len(s2.Failures), r2.Updated)
	if _, stillFails := s2.Failures["broken.go"]; stillFails {
		t.Fatalf("stale failure survived after fix")
	}
	if s2.Files["broken.go"] == nil {
		t.Fatalf("fixed file did not become a healthy entry")
	}
}

func snapshotsEqual(a, b *model.Snapshot) bool {
	if a.Generation != b.Generation ||
		len(a.Files) != len(b.Files) || len(a.Failures) != len(b.Failures) ||
		len(a.Renames) != len(b.Renames) {
		return false
	}
	for p, fa := range a.Files {
		fb, ok := b.Files[p]
		if !ok || fa.ID != fb.ID || fa.Hash != fb.Hash || len(fa.Chunks) != len(fb.Chunks) {
			return false
		}
	}
	for p, xa := range a.Failures {
		xb, ok := b.Failures[p]
		if !ok || xa.ID != xb.ID || xa.Category != xb.Category {
			return false
		}
	}
	return true
}

func keys(m map[string]*model.File) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPersistingFailuresDoNotBumpGeneration(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ok.go", "package demo\nfunc OK() {}\n")
	writeFile(t, root, "bad.go", "package demo\nfunc oops( {\n")
	s1, r1 := build(t, root, nil)
	t.Logf("KEY INPUT good+bad -> JUDGMENT gen=%d failures=%d failureChanged=%v",
		s1.Generation, len(r1.Failures), r1.FailureChanged)
	if s1.Generation != 1 || len(r1.FailureChanged) != 1 {
		t.Fatalf("first scan: gen=%d changedFailures=%v", s1.Generation, r1.FailureChanged)
	}
	// Same tree state: failures persist but nothing changes.
	s2, r2 := build(t, root, s1)
	t.Logf("KEY INPUT unchanged (failure persists) -> JUDGMENT gen=%d failureChanged=%v unchanged=%v",
		s2.Generation, r2.FailureChanged, r2.Unchanged)
	if s2.Generation != 1 {
		t.Fatalf("generation bumped on unchanged failures: %d", s2.Generation)
	}
	if len(r2.FailureChanged) != 0 {
		t.Fatalf("unchanged failures reported as changed: %v", r2.FailureChanged)
	}
}
