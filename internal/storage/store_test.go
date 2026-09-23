package storage_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/code-index-service-go/internal/model"
	"github.com/xingjianluo/code-index-service-go/internal/storage"
)

func sampleSnapshot(gen int64) *model.Snapshot {
	return &model.Snapshot{
		FormatVersion: model.FormatV1,
		RuleVersion:   model.RulesV1,
		Generation:    gen,
		Files: map[string]*model.File{
			"a.go": {
				ID:       "f_abc",
				Path:     "a.go",
				Language: model.LangGo,
				Hash:     "deadbeefdeadbeefdeadbeefdeadbeef",
				Chunks: []model.Chunk{{
					ID: "c_1", FileID: "f_abc", Path: "a.go",
					StartLine: 1, EndLine: 2, Kind: "symbol",
					SymbolName: "F", Content: "func F() {}", Tokens: 3,
				}},
				Symbols: []model.Symbol{{ID: "s_1", Name: "F", Kind: model.SymbolFunc, Line: 1, Column: 6}},
				Refs:    []model.Reference{},
			},
		},
		Failures: map[string]*model.Failure{
			"bad.go": {
				ID:       model.FailureID("bad.go", model.FailureParse),
				Path:     "bad.go",
				Category: model.FailureParse,
				Reason:   "boom",
			},
		},
		Renames: map[string]string{},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	st := storage.New(dir)
	if st.Exists() {
		t.Fatal("empty store should not exist")
	}
	snap := sampleSnapshot(7)
	if err := st.Save(snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Logf("KEY INPUT snapshot gen=7 files=1 failures=1 -> JUDGMENT loaded gen=%d file=%s failureCat=%s",
		got.Generation, got.Files["a.go"].ID, got.Failures["bad.go"].Category)
	if got.Generation != 7 || len(got.Files) != 1 || len(got.Failures) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Files["a.go"].Chunks[0].Content != "func F() {}" {
		t.Fatalf("chunk content lost")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{
	  "format_version": 1,
	  "rule_version": 1,
	  "generation": 1,
	  "files": {},
	  "failures": {},
	  "renames": {},
	  "mysterious_new_field": 42
	}`)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := storage.New(dir).Load()
	t.Logf("KEY INPUT unknown field 'mysterious_new_field' -> JUDGMENT err=%v", err)
	if err == nil {
		t.Fatal("unknown field must be rejected, got nil error")
	}
	if !errors.Is(err, storage.ErrUnsupportedVersion) {
		t.Fatalf("error should relate to unsupported handling: %v", err)
	}
}

func TestLoadRejectsFutureFormatAndForeignRuleVersion(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"future format", `{"format_version":99,"rule_version":1,"files":{},"failures":{}}`},
		{"missing rule version", `{"format_version":1,"files":{},"failures":{}}`},
		{"future rules", `{"format_version":1,"rule_version":99,"files":{},"failures":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := storage.DecodeSnapshot([]byte(tc.raw))
			t.Logf("KEY INPUT %s -> JUDGMENT err=%v", tc.name, err)
			if !errors.Is(err, storage.ErrUnsupportedVersion) {
				t.Fatalf("want unsupported-version error, got %v", err)
			}
			var mr *storage.MigrationRequired
			if tc.name == "future rules" && !errors.As(err, &mr) {
				t.Fatalf("rule mismatch should carry MigrationRequired guidance, got %T %v", err, err)
			}
		})
	}
}

func TestV0Migration(t *testing.T) {
	v0 := []byte(`{
	  "version": 0,
	  "generation": 3,
	  "files": [
	    {
	      "path": "old.go",
	      "language": "go",
	      "size": 12,
	      "hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	      "lines": 1,
	      "symbols": [],
	      "refs": [],
	      "chunks": [
	        {"id":"c_old","start_line":1,"end_line":1,"kind":"text","content":"package old","tokens":2}
	      ]
	    }
	  ],
	  "failures": [
	    {"path":"bad.txt","category":"invalid_encoding","reason":"bad byte"}
	  ]
	}`)
	snap, err := storage.MigrateV0Bytes(v0)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	t.Logf("KEY INPUT v0 payload -> JUDGMENT format=%d rule=%d gen=%d files=%d failures=%d",
		snap.FormatVersion, snap.RuleVersion, snap.Generation, len(snap.Files), len(snap.Failures))
	if snap.FormatVersion != model.FormatV1 || snap.RuleVersion != model.RulesV1 {
		t.Fatalf("versions not stamped to v1: %d/%d", snap.FormatVersion, snap.RuleVersion)
	}
	f := snap.Files["old.go"]
	if f == nil {
		t.Fatal("files not re-keyed by path")
	}
	if f.ID != model.FileID(f.Hash) {
		t.Fatalf("file id not backfilled: %s", f.ID)
	}
	if f.Chunks[0].FileID != f.ID || f.Chunks[0].Path != "old.go" {
		t.Fatalf("chunk denormalization not backfilled: %+v", f.Chunks[0])
	}
	if snap.Failures["bad.txt"].Category != model.FailureInvalidEncoding {
		t.Fatalf("failure not migrated: %+v", snap.Failures)
	}

	// Migration must be deterministic and reject unknown fields.
	snap2, err := storage.MigrateV0Bytes(v0)
	if err != nil {
		t.Fatal(err)
	}
	if snap2.Files["old.go"].ID != f.ID || snap2.Generation != snap.Generation {
		t.Fatal("same v0 bytes migrated differently")
	}
	bad := append([]byte(nil), v0...)
	bad = append(bad[:len(bad)-2], []byte(`,"strange":1}}`)...)
	if _, err := storage.MigrateV0Bytes(bad); err == nil {
		t.Fatal("v0 payload with unknown field must be rejected")
	}
}

func TestLoadV0FileMigratesThroughStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v0 := []byte(`{"version":0,"generation":1,"files":[],"failures":[]}`)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), v0, 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := storage.New(dir).Load()
	if err != nil {
		t.Fatalf("store load should migrate v0: %v", err)
	}
	if snap.FormatVersion != 1 {
		t.Fatalf("migrated snapshot format = %d", snap.FormatVersion)
	}
}
