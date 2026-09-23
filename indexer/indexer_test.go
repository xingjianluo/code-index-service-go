package indexer_test

import (
	"testing"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/store"
)

const goodGoA = `package main

// Alpha greets a user.
func Alpha(name string) string {
	return "hi " + name
}
`

const goodGoB = `package main

// Beta is a second symbol.
func Beta(x int) int {
	return x + 1
}
`

const goodGoA2 = `package main

// Alpha greets a user (updated body).
func Alpha(name string) string {
	return "hello there, " + name + "!"
}
`

// TestIncrementalAddModifyDelete drives add -> modify -> delete and asserts the
// reports and that deleted content never survives in the snapshot.
func TestIncrementalAddModifyDelete(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{"a.go": goodGoA})
	idx := newStack(t, root, storeDir, 1<<20)

	snap1, rep1, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#1: %v", err)
	}
	t.Logf("pass1 snapshot=%s added=%v unchanged=%v", rep1.SnapshotID, rep1.Added, rep1.Unchanged)
	if len(rep1.Added) != 1 || rep1.Added[0] != "a.go" {
		t.Fatalf("pass1 want added=[a.go], got %+v", rep1.Added)
	}
	alphaID1 := snap1.Files["a.go"].Symbols[0].ID
	t.Logf("symbol Alpha id=%s (must be stable while content/path unchanged)", alphaID1)

	// Same state re-indexed: everything unchanged, same snapshot identity.
	snap2, rep2, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#2: %v", err)
	}
	t.Logf("pass2 snapshot=%s unchanged=%v", rep2.SnapshotID, rep2.Unchanged)
	if snap1.ID != snap2.ID {
		t.Fatalf("reindex identity drifted: %s vs %s", snap1.ID, snap2.ID)
	}
	if len(rep2.Unchanged) != 1 || rep2.Unchanged[0] != "a.go" {
		t.Fatalf("pass2 want unchanged=[a.go], got %+v", rep2.Unchanged)
	}
	if got := snap2.Files["a.go"].Symbols[0].ID; got != alphaID1 {
		t.Fatalf("symbol id changed on unchanged content: %s vs %s", got, alphaID1)
	}

	// Modify in place: modified list, symbol id stable (same name/line/path),
	// old chunk ids must be gone.
	oldChunkIDs := map[string]bool{}
	for _, c := range snap1.Files["a.go"].Chunks {
		oldChunkIDs[c.ID] = true
	}
	rewriteFile(t, root, "a.go", goodGoA2)
	snap3, rep3, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#3: %v", err)
	}
	t.Logf("pass3 snapshot=%s modified=%v", rep3.SnapshotID, rep3.Modified)
	if len(rep3.Modified) != 1 || rep3.Modified[0] != "a.go" {
		t.Fatalf("pass3 want modified=[a.go], got %+v", rep3.Modified)
	}
	if got := snap3.Files["a.go"].Symbols[0].ID; got != alphaID1 {
		t.Fatalf("Alpha identity changed after body edit at same line: %s vs %s", got, alphaID1)
	}
	for _, c := range snap3.Files["a.go"].Chunks {
		if oldChunkIDs[c.ID] {
			t.Fatalf("stale chunk %s still present after content overwrite", c.ID)
		}
		if c.Content == goodGoA {
			t.Fatalf("old content recalled after overwrite")
		}
	}

	// Add a second file, then delete the first.
	rewriteFile(t, root, "b.go", goodGoB)
	_, rep4, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#4: %v", err)
	}
	t.Logf("pass4 added=%v", rep4.Added)
	if len(rep4.Added) != 1 || rep4.Added[0] != "b.go" {
		t.Fatalf("pass4 want added=[b.go], got %+v", rep4.Added)
	}

	removeFile(t, root, "a.go")
	snap5, rep5, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#5: %v", err)
	}
	t.Logf("pass5 deleted=%v files=%v", rep5.Deleted, mapKeys2(snap5.Files))
	if len(rep5.Deleted) != 1 || rep5.Deleted[0] != "a.go" {
		t.Fatalf("pass5 want deleted=[a.go], got %+v", rep5.Deleted)
	}
	if _, ok := snap5.Files["a.go"]; ok {
		t.Fatalf("deleted file a.go still present in snapshot")
	}
	if _, ok := snap5.Files["b.go"]; !ok {
		t.Fatalf("b.go should still be indexed")
	}

	// Persisted snapshot on disk must equal the in-memory one (no old content).
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	loaded, err := st.Load(t.Context())
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	t.Logf("loaded snapshot=%s id-match=%v", loaded.ID, loaded.ID == snap5.ID)
	if loaded.ID != snap5.ID {
		t.Fatalf("persisted snapshot id %s != computed pass id %s", loaded.ID, snap5.ID)
	}
	if _, ok := loaded.Files["a.go"]; ok {
		t.Fatalf("deleted a.go recalled from disk")
	}
}

// TestRenameDetection verifies that identical content at a new path with the
// old path gone is classified exactly once as a rename, not add+delete.
func TestRenameDetection(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"old/a.go": goodGoA,
		"keep.go":  goodGoB,
	})
	idx := newStack(t, root, storeDir, 1<<20)
	snap1, rep1, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#1: %v", err)
	}
	t.Logf("pass1 snapshot=%s added=%v", rep1.SnapshotID, rep1.Added)
	oldRec := snap1.Files["old/a.go"]

	renamePath(t, root, "old/a.go", "new/a.go")
	snap2, rep2, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#2: %v", err)
	}
	t.Logf("pass2 renamed=%+v added=%v deleted=%v", rep2.Renamed, rep2.Added, rep2.Deleted)
	if len(rep2.Renamed) != 1 || rep2.Renamed[0].OldPath != "old/a.go" || rep2.Renamed[0].NewPath != "new/a.go" {
		t.Fatalf("want one rename old/a.go->new/a.go, got %+v", rep2.Renamed)
	}
	if len(rep2.Added) != 0 || len(rep2.Deleted) != 0 {
		t.Fatalf("rename must not also count as add/delete: add=%v del=%v", rep2.Added, rep2.Deleted)
	}
	newRec := snap2.Files["new/a.go"]
	if newRec.ContentHash != oldRec.ContentHash {
		t.Fatalf("content hash changed across rename: %s vs %s", newRec.ContentHash, oldRec.ContentHash)
	}
	if _, ok := snap2.Files["old/a.go"]; ok {
		t.Fatalf("old rename path still indexed")
	}
}

// TestFailureCategories feeds each bad input class and asserts distinguishable,
// non-silent failure records.
func TestFailureCategories(t *testing.T) {
	// oversize file
	huge := make([]byte, 5000)
	for i := range huge {
		huge[i] = 'a'
	}
	root, storeDir := writeRepo(t, map[string]string{
		"ok.go":        goodGoA,
		"badutf8.go":   "package main\n\nvar s = \"\xff\xfe\"\n",
		"badsyntax.go": "package main\nfunc Broken( {{{\n",
		"huge.txt":     string(huge),
	})
	idx := newStack(t, root, storeDir, 1024) // tiny limit => huge.txt oversize
	_, rep, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	cats := map[model.FailureCategory]string{}
	for _, f := range rep.Failed {
		cats[f.Category] = f.Path
		t.Logf("failure category=%-13q path=%q message=%q size=%d", f.Category, f.Path, f.Message, f.Size)
	}
	for _, want := range []model.FailureCategory{
		model.FailureInvalidUTF8, model.FailureUnparseable, model.FailureOversize,
	} {
		if _, ok := cats[want]; !ok {
			t.Fatalf("expected failure category %s, got categories=%v", want, cats)
		}
	}
	if _, ok := cats[model.FailureInvalidUTF8]; ok && cats[model.FailureInvalidUTF8] != "badutf8.go" {
		t.Fatalf("invalid_utf8 attributed to wrong file: %s", cats[model.FailureInvalidUTF8])
	}
	// Deterministic failures participate in identity: same bad inputs twice.
	snapA := rep.SnapshotID
	_, repB, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#2: %v", err)
	}
	if repB.SnapshotID != snapA {
		t.Fatalf("identical state incl. failures produced different ids: %s vs %s", repB.SnapshotID, snapA)
	}
	// Fixing the bad utf8 file removes its failure and flips identity.
	rewriteFile(t, root, "badutf8.go", "package main\n\nvar s = \"ok\"\n")
	_, repC, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index#3: %v", err)
	}
	t.Logf("after fix snapshot=%s failures=%d", repC.SnapshotID, len(repC.Failed))
	if repC.SnapshotID == snapA {
		t.Fatalf("snapshot id did not change after fixing an invalid input")
	}
	for _, f := range repC.Failed {
		if f.Category == model.FailureInvalidUTF8 {
			t.Fatalf("invalid_utf8 failure survived after file was fixed")
		}
	}
}

func mapKeys2(m map[string]*model.FileRecord) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestOldSnapshotImmutableAfterReindex guards the snapshot isolation contract:
// reference resolution in a later pass rewrites the new snapshot's slices only;
// a reader still holding the old snapshot must observe its original bytes.
func TestOldSnapshotImmutableAfterReindex(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"a.go": "package main\n\nfunc Solo() int { return 1 }\n",
		"b.go": "package main\n\nfunc Caller() int { return Solo() }\n",
	})
	idx := newStack(t, root, storeDir, 1<<20)
	snap1, _, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	beforeRefs := append([]model.Reference(nil), snap1.Files["b.go"].References...)
	t.Logf("snapshot1=%s b.go references=%d", snap1.ID, len(beforeRefs))

	// Add a new file that introduces additional name resolutions, keep a.go.
	rewriteFile(t, root, "c.go", "package main\n\nfunc Extra() int { return Solo() }\n")
	if _, _, err := idx.Index(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	afterRefs := snap1.Files["b.go"].References
	if len(afterRefs) != len(beforeRefs) {
		t.Fatalf("old snapshot mutated: references %d -> %d", len(beforeRefs), len(afterRefs))
	}
	for i := range beforeRefs {
		if beforeRefs[i] != afterRefs[i] {
			t.Fatalf("old snapshot reference %d mutated: %+v -> %+v", i, beforeRefs[i], afterRefs[i])
		}
	}
	t.Logf("old snapshot b.go references unchanged after later reindex (n=%d)", len(afterRefs))
}
