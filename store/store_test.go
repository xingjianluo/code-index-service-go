package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/store"
)

func writeEnvelope(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.DefaultFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// minimalValidEnvelope is a hand-built v2 envelope used as a base.
const minimalValidEnvelope = `{
  "id": "",
  "store_format": 2,
  "index_rules": 2,
  "root": "/repo",
  "created_at": "2026-09-23T00:00:00Z",
  "files": {},
  "failures": []
}`

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := &model.Snapshot{
		StoreFormat: model.StoreFormatV2,
		IndexRules:  model.IndexRulesV2,
		Root:        "/repo",
		Files: map[string]*model.FileRecord{
			"a.go": {
				Path: "a.go", Language: "go", ContentHash: "h1", Lines: 1,
				Symbols:    []model.Symbol{{ID: "s1", Name: "Alpha", Kind: model.SymbolFunc, Path: "a.go", Line: 1}},
				References: []model.Reference{{Name: "Alpha", Path: "a.go", Line: 1, SymbolID: "s1"}},
				Chunks: []model.Chunk{{ID: "c1", Path: "a.go", StartLine: 1, EndLine: 1, Kind: "declaration",
					Content: "func Alpha() {}", Tokens: []string{"func", "alpha"}, ContentHash: "ch1"}},
			},
		},
		Failures: []model.Failure{},
	}
	snap.ID = model.SnapshotID(snap.Files, snap.Failures, snap.Skipped)
	if err := st.Save(context.Background(), snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := st.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Logf("roundtrip id=%s files=%d symbols=%d chunks=%d",
		loaded.ID, len(loaded.Files), len(loaded.Files["a.go"].Symbols), len(loaded.Files["a.go"].Chunks))
	if loaded.ID != snap.ID {
		t.Fatalf("id drift after roundtrip: %s vs %s", loaded.ID, snap.ID)
	}
}

func TestRejectUnknownVersionAndFields(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "future_store_format",
			body:    strings.Replace(minimalValidEnvelope, `"store_format": 2`, `"store_format": 99`, 1),
			wantSub: "unsupported version",
		},
		{
			name:    "future_index_rules",
			body:    strings.Replace(minimalValidEnvelope, `"index_rules": 2`, `"index_rules": 7`, 1),
			wantSub: "unsupported version",
		},
		{
			name: "unknown_field",
			body: `{
  "id": "", "store_format": 2, "index_rules": 2, "root": "/r",
  "created_at": "2026-09-23T00:00:00Z",
  "files": {}, "failures": [], "mystery": 42
}`,
			wantSub: "unknown",
		},
		{
			name:    "unknown_file_field",
			wantSub: "unknown",
			body: `{
  "id": "", "store_format": 2, "index_rules": 2, "root": "/r",
  "created_at": "2026-09-23T00:00:00Z",
  "files": {"a.go": {"path":"a.go","language":"go","size":0,"content_hash":"h","lines":0,
    "symbols":[],"references":[],"chunks":[],"bogus":1}},
  "failures": []
}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "st")
			writeEnvelope(t, dir, c.body)
			st, err := store.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, err = st.Load(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("want error containing %q, got %v", c.wantSub, err)
			}
			t.Logf("input=%-22s => rejected: %v", c.name, err)
		})
	}
}

func TestV1MigrationRetokenizes(t *testing.T) {
	// Hand-authored v1 envelope: old token list lacks camel-case split
	// ("GetUserName" stored as a single v1 token) and has no snapshot id.
	body := `{
  "store_format": 1,
  "index_rules": 1,
  "root": "/repo",
  "created_at": "2026-09-23T00:00:00Z",
  "files": {
    "a.go": {
      "path": "a.go", "language": "go", "size": 30, "content_hash": "deadbeef", "lines": 1,
      "symbols": [], "references": [],
      "chunks": [
        {"id":"c-old","path":"a.go","start_line":1,"end_line":1,"kind":"declaration",
         "content":"func GetUserName() string { return \"x\" }",
         "tokens":["GetUserName"], "content_hash":"c-hash"}
      ]
    }
  },
  "failures": []
}`
	dir := filepath.Join(t.TempDir(), "st")
	writeEnvelope(t, dir, body)
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Loading v1 without a migrator is explicitly refused (no silent downgrade).
	if _, err := st.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "migrator") {
		t.Fatalf("v1 without migrator must be rejected, got: %v", err)
	}

	loaded, err := st.LoadWithMigration(context.Background(), parser.MigrateV1ToV2)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	ch := loaded.Files["a.go"].Chunks[0]
	t.Logf("migrated tokens=%v versions=%d/%d id=%s", ch.Tokens, loaded.StoreFormat, loaded.IndexRules, loaded.ID)
	found := map[string]bool{}
	for _, tok := range ch.Tokens {
		found[tok] = true
	}
	for _, want := range []string{"get", "user", "name"} {
		if !found[want] {
			t.Fatalf("v2 migration missing token %q in %v", want, ch.Tokens)
		}
	}
	if ch.ID != "c-old" {
		t.Fatalf("chunk identity must be preserved across migration, got %s", ch.ID)
	}
	if loaded.StoreFormat != 2 || loaded.IndexRules != 2 {
		t.Fatalf("version tags not bumped: %d/%d", loaded.StoreFormat, loaded.IndexRules)
	}
	if loaded.ID == "" {
		t.Fatal("migrated snapshot has no computed id")
	}
}

func TestAtomicWriteLeavesNoTempFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	st, _ := store.New(dir)
	snap := &model.Snapshot{Files: map[string]*model.FileRecord{}, Failures: []model.Failure{}}
	snap.ID = model.SnapshotID(snap.Files, snap.Failures, snap.Skipped)
	if err := st.Save(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, store.DefaultFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind or stat error: %v", err)
	}
}
