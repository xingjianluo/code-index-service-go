package indexer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/code-index-service-go/indexer"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/source"
	"github.com/xingjianluo/code-index-service-go/store"
)

// writeRepo creates files (path -> content) under a fresh temp directory.
func writeRepo(t *testing.T, files map[string]string) (root, storeDir string) {
	t.Helper()
	dir := t.TempDir()
	root = filepath.Join(dir, "repo")
	storeDir = filepath.Join(dir, "store")
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root, storeDir
}

// rewriteFile replaces one file's content.
func rewriteFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

// removeFile deletes one file.
func removeFile(t *testing.T, root, path string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// renamePath moves one file.
func renamePath(t *testing.T, root, oldP, newP string) {
	t.Helper()
	from := filepath.Join(root, filepath.FromSlash(oldP))
	to := filepath.Join(root, filepath.FromSlash(newP))
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatalf("mkdir rename target: %v", err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatalf("rename %s->%s: %v", oldP, newP, err)
	}
}

func newStack(t *testing.T, root, storeDir string, maxSize int64) *indexer.Indexer {
	t.Helper()
	sc := source.NewScanner(source.Options{Root: root, MaxFileSize: maxSize})
	pa := parser.New(parser.Options{MaxFileSize: maxSize})
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	return indexer.New(sc, pa, st)
}

func mustIndex(t *testing.T, idx *indexer.Indexer, root string) {
	t.Helper()
	if _, _, err := idx.Index(t.Context(), root); err != nil {
		t.Fatalf("index root=%s: %v", root, err)
	}
}
