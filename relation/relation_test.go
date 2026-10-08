package relation_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/code-index-service-go/indexer"
	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/relation"
	"github.com/xingjianluo/code-index-service-go/source"
	"github.com/xingjianluo/code-index-service-go/store"
)

// --- helpers (mirrors indexer test support; test packages cannot share) ---

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

func newStack(t *testing.T, root, storeDir string) *indexer.Indexer {
	t.Helper()
	sc := source.NewScanner(source.Options{Root: root, MaxFileSize: 2 << 20})
	pa := parser.New(parser.Options{MaxFileSize: 2 << 20})
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	return indexer.New(sc, pa, st)
}

func indexOnce(t *testing.T, idx *indexer.Indexer, root string) (*model.Snapshot, *model.IndexReport) {
	t.Helper()
	snap, report, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	return snap, report
}

func mustRefs(t *testing.T, snap *model.Snapshot, name string, l relation.Limits) *relation.ReferencesOutcome {
	t.Helper()
	out, err := relation.NewEngine().References(t.Context(), snap, name, l)
	if err != nil {
		t.Fatalf("references %q: %v", name, err)
	}
	return out
}

func mustImpact(t *testing.T, snap *model.Snapshot, files []string, l relation.Limits) *relation.ImpactOutcome {
	t.Helper()
	out, err := relation.NewEngine().Impact(t.Context(), snap, files, l)
	if err != nil {
		t.Fatalf("impact %v: %v", files, err)
	}
	return out
}

func findHit(list []relation.ReferenceHit, path string, line int) *relation.ReferenceHit {
	for i := range list {
		if list[i].Path == path && list[i].Line == line {
			return &list[i]
		}
	}
	return nil
}

// --- tests ----------------------------------------------------------------

// Resolved and unresolved occurrences of a name must be reported in separate
// lists, never mixed.
func TestReferencesSplitResolvedAndUnresolved(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"defs/helper.go": "package defs\n\nfunc Helper() {}\n",
		"use/caller.go":  "package use\n\nfunc Call() {\n\tHelper()\n\tGhost()\n}\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	out := mustRefs(t, snap, "Helper", relation.Limits{})
	t.Logf("input: name=Helper; judgment: occurrences bound to a definition at index time -> resolved")
	t.Logf("definitions=%v", out.Definitions)
	for _, h := range out.Resolved {
		t.Logf("resolved %s:%d:%d -> %s:%d key=%s", h.Path, h.Line, h.Column, h.TargetPath, h.TargetLine, h.TargetKey)
	}
	if len(out.Definitions) != 1 || out.Definitions[0].Path != "defs/helper.go" {
		t.Fatalf("definitions: want 1 at defs/helper.go, got %+v", out.Definitions)
	}
	// Occurrences: the declaration ident itself (defs/helper.go:3) and the
	// call (use/caller.go:4). Both resolved.
	if len(out.Resolved) != 2 {
		t.Fatalf("resolved: want 2, got %+v", out.Resolved)
	}
	call := findHit(out.Resolved, "use/caller.go", 4)
	if call == nil || call.TargetPath != "defs/helper.go" || call.TargetLine != 3 {
		t.Fatalf("call occurrence should resolve to defs/helper.go:3, got %+v", call)
	}
	if len(out.Unresolved) != 0 {
		t.Fatalf("Helper has a definition; want no unresolved, got %+v", out.Unresolved)
	}

	ghost := mustRefs(t, snap, "Ghost", relation.Limits{})
	t.Logf("input: name=Ghost; judgment: no definition anywhere -> every occurrence unresolved")
	for _, h := range ghost.Unresolved {
		t.Logf("unresolved %s:%d:%d", h.Path, h.Line, h.Column)
	}
	if len(ghost.Definitions) != 0 || len(ghost.Resolved) != 0 {
		t.Fatalf("Ghost: want no definitions/resolved, got defs=%+v resolved=%+v", ghost.Definitions, ghost.Resolved)
	}
	if len(ghost.Unresolved) != 1 || ghost.Unresolved[0].Path != "use/caller.go" || ghost.Unresolved[0].Line != 5 {
		t.Fatalf("Ghost: want one unresolved at use/caller.go:5, got %+v", ghost.Unresolved)
	}
}

// With several same-name definitions the attribution is the fixed index-time
// rule (same-file wins; otherwise kind priority, then path, then line), and
// repeated/ differently ordered queries return identical results.
func TestReferencesMultiDefinitionAttribution(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"aa/dup.go":     "package aa\n\nfunc Dup() {}\n",
		"zz/dup.go":     "package zz\n\nfunc Dup() {}\n\nfunc Self() {\n\tDup()\n}\n",
		"mid/caller.go": "package mid\n\nfunc Use() {\n\tDup()\n}\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	out := mustRefs(t, snap, "Dup", relation.Limits{})
	t.Logf("input: name=Dup with definitions in aa/dup.go and zz/dup.go")
	t.Logf("judgment: same-file definition wins; otherwise global order (kind, path, line) -> aa/dup.go beats zz/dup.go")
	for _, h := range out.Resolved {
		t.Logf("resolved %s:%d -> %s:%d", h.Path, h.Line, h.TargetPath, h.TargetLine)
	}
	if len(out.Definitions) != 2 {
		t.Fatalf("want 2 definitions, got %+v", out.Definitions)
	}
	if out.Definitions[0].Path != "aa/dup.go" || out.Definitions[1].Path != "zz/dup.go" {
		t.Fatalf("definitions must be listed in (path,line) order, got %+v", out.Definitions)
	}
	// Cross-file caller: global fallback picks aa/dup.go (path order).
	if h := findHit(out.Resolved, "mid/caller.go", 4); h == nil || h.TargetPath != "aa/dup.go" {
		t.Fatalf("mid/caller.go Dup should attribute to aa/dup.go, got %+v", h)
	}
	// Same-file caller: zz/dup.go's own Dup wins for the occurrence in zz.
	if h := findHit(out.Resolved, "zz/dup.go", 6); h == nil || h.TargetPath != "zz/dup.go" {
		t.Fatalf("zz/dup.go Dup occurrence should attribute to its own file, got %+v", h)
	}

	// Determinism: repeated queries on the same snapshot are byte-identical.
	again := mustRefs(t, snap, "Dup", relation.Limits{})
	b1, _ := json.Marshal(out)
	b2, _ := json.Marshal(again)
	if string(b1) != string(b2) {
		t.Fatalf("repeated query differs:\n%s\n%s", b1, b2)
	}
	t.Logf("determinism: two identical queries produced identical %d-byte JSON", len(b1))
}

// A pure rename (content untouched) must not be seen as "definition lost +
// new definition appeared": the rename-stable key survives, and previously
// resolved references keep resolving to the same logical symbol.
func TestRenameKeepsSymbolTracking(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"old/feat.go":   "package old\n\nfunc Tracked() {}\n",
		"use/caller.go": "package use\n\nfunc Call() {\n\tTracked()\n}\n",
	})
	idx := newStack(t, root, storeDir)
	snap1, _ := indexOnce(t, idx, root)

	before := mustRefs(t, snap1, "Tracked", relation.Limits{})
	if len(before.Definitions) != 1 {
		t.Fatalf("want 1 definition before rename, got %+v", before.Definitions)
	}
	keyBefore := before.Definitions[0].Key
	idBefore := before.Definitions[0].ID
	callBefore := findHit(before.Resolved, "use/caller.go", 4)
	if callBefore == nil {
		t.Fatalf("call occurrence should be resolved before rename: %+v", before)
	}
	t.Logf("before rename: def=%s:%d id=%s key=%s; call resolved to %s:%d",
		before.Definitions[0].Path, before.Definitions[0].Line, idBefore, keyBefore,
		callBefore.TargetPath, callBefore.TargetLine)

	// Pure rename: same bytes, new path.
	from := filepath.Join(root, "old", "feat.go")
	to := filepath.Join(root, "moved", "feat.go")
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	snap2, report := indexOnce(t, idx, root)
	t.Logf("rename report: %+v", report.Renamed)
	if len(report.Renamed) != 1 || report.Renamed[0].OldPath != "old/feat.go" || report.Renamed[0].NewPath != "moved/feat.go" {
		t.Fatalf("want pure rename old/feat.go -> moved/feat.go, got %+v", report.Renamed)
	}

	after := mustRefs(t, snap2, "Tracked", relation.Limits{})
	if len(after.Definitions) != 1 {
		t.Fatalf("want still exactly 1 definition after rename (not lost+new), got %+v", after.Definitions)
	}
	got := after.Definitions[0]
	t.Logf("after rename: def=%s:%d id=%s key=%s", got.Path, got.Line, got.ID, got.Key)
	if got.Key != keyBefore {
		t.Fatalf("rename-stable key changed across pure rename: %s -> %s", keyBefore, got.Key)
	}
	if got.ID == idBefore {
		t.Fatalf("path-based symbol ID should move with the file, got unchanged %s", got.ID)
	}
	callAfter := findHit(after.Resolved, "use/caller.go", 4)
	if callAfter == nil {
		t.Fatalf("reference relation broke across rename: resolved=%+v unresolved=%+v", after.Resolved, after.Unresolved)
	}
	if callAfter.TargetKey != keyBefore || callAfter.TargetPath != "moved/feat.go" {
		t.Fatalf("call should still point at the same logical symbol (key=%s) at moved/feat.go, got %+v",
			keyBefore, callAfter)
	}
	if len(after.Unresolved) != 0 {
		t.Fatalf("rename must not turn resolved references unresolved: %+v", after.Unresolved)
	}
	t.Logf("judgment: key stable (%s), path-based id moved (%s -> %s), reference still resolved",
		keyBefore, idBefore, got.ID)
}

// Impact lists files depending on changed-file symbols, with the reference
// chain, and classifies every changed input explicitly (including failed and
// non-indexed files).
func TestImpactDirectAndChangedFileClassification(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"core/base.go":  "package core\n\nfunc Base() {}\n",
		"app/user.go":   "package app\n\nfunc Run() {\n\tBase()\n}\n",
		"app/alone.go":  "package app\n\nfunc Lonely() {}\n",
		"bad/broken.go": "\xff\xfe not utf8",
		"bin/blob.xyz":  "binary-ish content, extension not indexed",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	changed := []string{"core/base.go", "bad/broken.go", "bin/blob.xyz", "no/such/file.go"}
	out := mustImpact(t, snap, changed, relation.Limits{})
	t.Logf("input: changed=%v", changed)
	for _, c := range out.Changed {
		t.Logf("changed file %s -> status=%s category=%s (%s)", c.Path, c.Status, c.Category, c.Detail)
	}

	status := map[string]relation.ChangedFileStatus{}
	for _, c := range out.Changed {
		status[c.Path] = c
	}
	if status["core/base.go"].Status != "indexed" {
		t.Fatalf("core/base.go should be indexed, got %+v", status["core/base.go"])
	}
	if status["bad/broken.go"].Status != "failed" || status["bad/broken.go"].Category != string(model.FailureInvalidUTF8) {
		t.Fatalf("bad/broken.go should be failed/invalid_utf8, got %+v", status["bad/broken.go"])
	}
	// In-repo but never analyzed (extension not collected): distinct class.
	if status["bin/blob.xyz"].Status != "skipped" || status["bin/blob.xyz"].Category != "excluded_extension" {
		t.Fatalf("bin/blob.xyz should be skipped/excluded_extension, got %+v", status["bin/blob.xyz"])
	}
	// Never seen by the indexer at all: a different, clearly separable class.
	if status["no/such/file.go"].Status != "unknown" {
		t.Fatalf("no/such/file.go should be unknown (path never seen), got %+v", status["no/such/file.go"])
	}

	if len(out.Impacted) != 1 || out.Impacted[0].Path != "app/user.go" {
		t.Fatalf("want exactly app/user.go impacted, got %+v", out.Impacted)
	}
	imp := out.Impacted[0]
	if len(imp.Symbols) != 1 || imp.Symbols[0] != "Base" {
		t.Fatalf("impacted symbols should be [Base], got %v", imp.Symbols)
	}
	if len(imp.Chains) != 1 || len(imp.Chains[0]) != 2 {
		t.Fatalf("want one 2-edge chain, got %+v", imp.Chains)
	}
	defEdge, refEdge := imp.Chains[0][0], imp.Chains[0][1]
	t.Logf("chain: [%s] %s -> %s | [%s] %s -> %s",
		defEdge.Kind, defEdge.From, defEdge.To, refEdge.Kind, refEdge.From, refEdge.To)
	if defEdge.Kind != "defines" || defEdge.From != "core/base.go" || defEdge.To != "Base" {
		t.Fatalf("bad defines edge: %+v", defEdge)
	}
	if refEdge.Kind != "references" || refEdge.From != "app/user.go:4" || refEdge.To != "Base" {
		t.Fatalf("bad references edge: %+v", refEdge)
	}
	if len(out.Suspected) != 0 {
		t.Fatalf("no same-name-elsewhere bindings expected, got %+v", out.Suspected)
	}
}

// An occurrence bound to a same-name definition in an unchanged file while a
// changed file also defines that name is suspected (attribution may shift),
// never reported as direct impact.
func TestImpactSuspectedBoundElsewhere(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"aa/dup.go":     "package aa\n\nfunc Dup() {}\n",
		"zz/dup.go":     "package zz\n\nfunc Dup() {}\n",
		"mid/caller.go": "package mid\n\nfunc Use() {\n\tDup()\n}\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	out := mustImpact(t, snap, []string{"zz/dup.go"}, relation.Limits{})
	t.Logf("input: changed=[zz/dup.go]; mid/caller.go's Dup is bound to aa/dup.go (global fallback: path order)")
	t.Logf("judgment: name Dup also defined in changed file -> attribution-sensitive -> suspected, not direct")
	if len(out.Impacted) != 0 {
		t.Fatalf("nothing binds to zz's Dup; want no direct impact, got %+v", out.Impacted)
	}
	if len(out.Suspected) != 1 || out.Suspected[0].Path != "mid/caller.go" {
		t.Fatalf("want mid/caller.go suspected, got %+v", out.Suspected)
	}
	s := out.Suspected[0]
	t.Logf("suspected chain: %+v", s.Chains[0])
	if s.Chains[0][1].Kind != "references_bound_elsewhere" {
		t.Fatalf("suspected chain edge kind: %+v", s.Chains[0][1])
	}

	direct := mustImpact(t, snap, []string{"aa/dup.go"}, relation.Limits{})
	if len(direct.Impacted) != 1 || direct.Impacted[0].Path != "mid/caller.go" {
		t.Fatalf("aa/dup.go is the actual binding target; want direct impact, got %+v", direct.Impacted)
	}
	t.Logf("control: changing aa/dup.go (the bound definition) -> direct impact on mid/caller.go")
}

// Impact caps are deterministic: the first N files in path order are kept
// and the omission is explained.
func TestImpactLimitsDeterministic(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"core/base.go": "package core\n\nfunc Base() {}\n",
		"b/one.go":     "package b\n\nfunc A() {\n\tBase()\n}\n",
		"c/two.go":     "package c\n\nfunc B() {\n\tBase()\n}\n",
		"d/three.go":   "package d\n\nfunc C() {\n\tBase()\n}\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	out := mustImpact(t, snap, []string{"core/base.go"}, relation.Limits{MaxImpacted: 2})
	t.Logf("input: changed=[core/base.go] MaxImpacted=2; judgment: keep first 2 impacted files in path order")
	for _, tr := range out.Truncated {
		t.Logf("truncation[%s] %s", tr.Reason, tr.Detail)
	}
	if len(out.Impacted) != 2 || out.Impacted[0].Path != "b/one.go" || out.Impacted[1].Path != "c/two.go" {
		t.Fatalf("want b/one.go,c/two.go kept in path order, got %+v", out.Impacted)
	}
	if len(out.Truncated) != 1 || out.Truncated[0].Reason != "max_impacted" {
		t.Fatalf("want one max_impacted truncation, got %+v", out.Truncated)
	}
}

// References caps: occurrences beyond MaxReferences are cut in deterministic
// (path,line,column) order and explained.
func TestReferencesLimitsDeterministic(t *testing.T) {
	root, storeDir := writeRepo(t, map[string]string{
		"defs/helper.go": "package defs\n\nfunc Helper() {}\n",
		"a/x.go":         "package a\n\nfunc A() {\n\tHelper()\n}\n",
		"b/y.go":         "package b\n\nfunc B() {\n\tHelper()\n}\n",
	})
	snap, _ := indexOnce(t, newStack(t, root, storeDir), root)

	out := mustRefs(t, snap, "Helper", relation.Limits{MaxReferences: 2})
	t.Logf("input: name=Helper MaxReferences=2; resolved occurrences cut in (path,line,column) order")
	for _, tr := range out.Truncated {
		t.Logf("truncation[%s] %s", tr.Reason, tr.Detail)
	}
	if len(out.Resolved) != 2 {
		t.Fatalf("want 2 resolved kept, got %+v", out.Resolved)
	}
	if out.Resolved[0].Path != "a/x.go" || out.Resolved[1].Path != "b/y.go" {
		t.Fatalf("kept occurrences must be the first in path order, got %+v", out.Resolved)
	}
	if len(out.Truncated) != 1 || out.Truncated[0].Reason != "max_references" {
		t.Fatalf("want one max_references truncation, got %+v", out.Truncated)
	}
}
