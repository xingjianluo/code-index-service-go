package relation_test

import (
	"encoding/json"
	"testing"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/relation"
)

// Files the scanner saw but never analyzed (uncommon extension) or could not
// analyze (non-UTF-8 content) must be explicitly accounted for in reference
// results: listed, each under its own category, never silently absent.
func TestReferencesUnanalyzedFilesAreAccountedFor(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"defs/helper.go": "package defs\n\nfunc Helper() {}\n",
		"use/caller.go":  "package use\n\nfunc Call() {\n\tHelper()\n\tGhost()\n}\n",
		// Mention the queried names, but none of these are analyzable code:
		"assets/data.dat":   "Helper Ghost\n",
		"assets/page.tmpl":  "{{ Helper }}\n",
		"assets/compat.inc": "Helper = 1\n",
		"bin/blob.go":       "\xff\xfe binary, not utf8\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	// The snapshot itself must carry the scanned-but-unanalyzed files.
	var skippedPaths []string
	for _, sk := range snap.Skipped {
		skippedPaths = append(skippedPaths, sk.Path)
	}
	t.Logf("snapshot skipped files: %v", skippedPaths)
	if len(skippedPaths) != 3 {
		t.Fatalf("want 3 skipped files (.dat/.tmpl/.inc), got %v", skippedPaths)
	}

	out := mustRefs(t, snap, "Helper", relation.Limits{})
	t.Logf("input: name=Helper; repo also contains assets/data.dat, assets/page.tmpl, assets/compat.inc (excluded extensions) and bin/blob.go (invalid UTF-8)")
	for _, u := range out.Unanalyzed {
		t.Logf("unanalyzed %s category=%s (%s)", u.Path, u.Category, u.Detail)
	}
	for _, h := range out.Resolved {
		t.Logf("resolved %s:%d:%d -> %s:%d", h.Path, h.Line, h.Column, h.TargetPath, h.TargetLine)
	}

	// The unanalyzed files are listed, each under its own category.
	byCat := map[string]string{}
	for _, u := range out.Unanalyzed {
		byCat[u.Path] = u.Category
	}
	want := map[string]string{
		"assets/data.dat":   "excluded_extension",
		"assets/page.tmpl":  "excluded_extension",
		"assets/compat.inc": "excluded_extension",
		"bin/blob.go":       string(model.FailureInvalidUTF8),
	}
	if len(out.Unanalyzed) != len(want) {
		t.Fatalf("want %d unanalyzed files, got %+v", len(want), out.Unanalyzed)
	}
	for p, cat := range want {
		if byCat[p] != cat {
			t.Fatalf("unanalyzed[%s]: want category %q, got %q (all: %+v)", p, cat, byCat[p], out.Unanalyzed)
		}
	}
	// Deterministic order: path-sorted.
	for i := 1; i < len(out.Unanalyzed); i++ {
		if out.Unanalyzed[i-1].Path > out.Unanalyzed[i].Path {
			t.Fatalf("unanalyzed not in path order: %+v", out.Unanalyzed)
		}
	}

	// The unanalyzed files never leak into resolved/unresolved, and the
	// analyzed occurrences are exactly as before (no shrink, no misplacement).
	if len(out.Resolved) != 2 {
		t.Fatalf("resolved: want 2 (declaration + call), got %+v", out.Resolved)
	}
	if h := findHit(out.Resolved, "use/caller.go", 4); h == nil || h.TargetPath != "defs/helper.go" {
		t.Fatalf("call occurrence must still resolve to defs/helper.go, got %+v", h)
	}
	if len(out.Unresolved) != 0 {
		t.Fatalf("Helper has a definition; want no unresolved, got %+v", out.Unresolved)
	}

	// Unresolved classification is untouched by the new category: Ghost is
	// still "name matched but no definition", never confused with unanalyzed.
	ghost := mustRefs(t, snap, "Ghost", relation.Limits{})
	t.Logf("input: name=Ghost; judgment: occurrence in analyzed code, no definition -> unresolved (not unanalyzed)")
	if len(ghost.Unresolved) != 1 || ghost.Unresolved[0].Path != "use/caller.go" {
		t.Fatalf("Ghost: want one unresolved at use/caller.go, got %+v", ghost.Unresolved)
	}
	if len(ghost.Unanalyzed) != len(want) {
		t.Fatalf("Ghost: unanalyzed files must be reported for every name, got %+v", ghost.Unanalyzed)
	}
}

// Impact must distinguish three situations that used to collapse into one
// "unknown" bucket: indexed code, in-repo-but-unanalyzed (split into failed
// vs skipped), and paths the indexer never saw.
func TestImpactDistinguishesSkippedFailedUnknown(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"core/base.go":     "package core\n\nfunc Base() {}\n",
		"assets/data.dat":  "Base\n",
		"assets/page.tmpl": "Base\n",
		"bin/blob.go":      "\xff\xfe binary\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	changed := []string{"assets/data.dat", "assets/page.tmpl", "bin/blob.go", "no/such/file.go", "core/base.go"}
	out := mustImpact(t, snap, changed, relation.Limits{})
	t.Logf("input: changed=%v", changed)
	for _, c := range out.Changed {
		t.Logf("changed file %s -> status=%s category=%s (%s)", c.Path, c.Status, c.Category, c.Detail)
	}

	status := map[string]relation.ChangedFileStatus{}
	for _, c := range out.Changed {
		status[c.Path] = c
	}
	cases := []struct {
		path, status, category string
	}{
		{"core/base.go", "indexed", ""},
		{"assets/data.dat", "skipped", "excluded_extension"},
		{"assets/page.tmpl", "skipped", "excluded_extension"},
		{"bin/blob.go", "failed", string(model.FailureInvalidUTF8)},
		{"no/such/file.go", "unknown", ""},
	}
	for _, tc := range cases {
		got := status[tc.path]
		if got.Status != tc.status || got.Category != tc.category {
			t.Fatalf("%s: want status=%s category=%q, got %+v", tc.path, tc.status, tc.category, got)
		}
		if tc.status != "indexed" && got.Detail == "" {
			t.Fatalf("%s: non-indexed status must carry a human-readable detail", tc.path)
		}
		t.Logf("judgment: %s -> %s/%s detail=%q", tc.path, got.Status, got.Category, got.Detail)
	}
}

// Same snapshot, repeated queries and differently ordered inputs must give
// byte-identical answers, including the new unanalyzed/skipped accounting.
func TestUnanalyzedAccountingIsDeterministic(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"defs/helper.go":   "package defs\n\nfunc Helper() {}\n",
		"use/caller.go":    "package use\n\nfunc Call() {\n\tHelper()\n}\n",
		"assets/data.dat":  "Helper\n",
		"assets/page.tmpl": "Helper\n",
		"bin/blob.go":      "\xff\xfe\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	r1 := mustRefs(t, snap, "Helper", relation.Limits{})
	r2 := mustRefs(t, snap, "Helper", relation.Limits{})
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	if string(b1) != string(b2) {
		t.Fatalf("repeated references query differs:\n%s\n%s", b1, b2)
	}
	t.Logf("references determinism: two queries, identical %d-byte JSON (unanalyzed=%d)", len(b1), len(r1.Unanalyzed))

	i1 := mustImpact(t, snap, []string{"assets/data.dat", "bin/blob.go", "ghost.go"}, relation.Limits{})
	i2 := mustImpact(t, snap, []string{"ghost.go", "bin/blob.go", "assets/data.dat"}, relation.Limits{})
	j1, _ := json.Marshal(i1)
	j2, _ := json.Marshal(i2)
	if string(j1) != string(j2) {
		t.Fatalf("impact must not depend on input order:\n%s\n%s", j1, j2)
	}
	t.Logf("impact determinism: reordered inputs, identical %d-byte JSON", len(j1))
}
