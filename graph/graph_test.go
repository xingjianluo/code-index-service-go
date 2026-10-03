package graph_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xingjianluo/code-index-service-go/graph"
	"github.com/xingjianluo/code-index-service-go/indexer"
	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/source"
	"github.com/xingjianluo/code-index-service-go/store"
)

// --- helpers ---------------------------------------------------------------

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

func newIndexer(t *testing.T, root, storeDir string) *indexer.Indexer {
	t.Helper()
	sc := source.NewScanner(source.Options{Root: root})
	pa := parser.New(parser.Options{})
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return indexer.New(sc, pa, st)
}

func mustIndex(t *testing.T, idx *indexer.Indexer, root string) (*model.Snapshot, *model.IndexReport) {
	t.Helper()
	snap, report, err := idx.Index(t.Context(), root)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	return snap, report
}

func defByPath(out *graph.RefsOutcome, path string) *graph.Definition {
	for i := range out.Definitions {
		if out.Definitions[i].Path == path {
			return &out.Definitions[i]
		}
	}
	return nil
}

func siteAt(out *graph.RefsOutcome, path string, line int) *graph.RefSite {
	for i := range out.Resolved {
		if out.Resolved[i].Path == path && out.Resolved[i].Line == line {
			return &out.Resolved[i]
		}
	}
	return nil
}

// --- rename continuity -----------------------------------------------------

// A pure rename (identical bytes at a new path) must keep the symbol's
// path-independent Key, must not turn previously resolved references into
// unresolved ones, and must keep impact chains working through the new path.
func TestRenameKeepsSymbolIdentityAndReferences(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\nfunc Helper() int { return 1 }\n\nfunc Handler() int { return Helper() }\n",
		"b.go": "package main\n\nfunc UseAll() int { return Helper() + Handler() }\n",
	}
	root, storeDir := writeRepo(t, files)
	idx := newIndexer(t, root, storeDir)
	eng := graph.NewEngine()

	snap1, _ := mustIndex(t, idx, root)
	before, err := eng.References(snap1, "Helper", graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Definitions) != 1 {
		t.Fatalf("want 1 definition of Helper, got %d", len(before.Definitions))
	}
	keyBefore := before.Definitions[0].Key
	idBefore := before.Definitions[0].SymbolID
	t.Logf("before rename: def at %s:%d id=%s key=%s resolved=%d unresolved=%d",
		before.Definitions[0].Path, before.Definitions[0].Line, idBefore, keyBefore,
		len(before.Resolved), len(before.Unresolved))

	// Pure rename: same bytes, new path in a new directory.
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a.go"), filepath.Join(root, "sub", "a.go")); err != nil {
		t.Fatal(err)
	}
	snap2, report := mustIndex(t, idx, root)
	t.Logf("reindex report: renamed=%v added=%v deleted=%v", report.Renamed, report.Added, report.Deleted)
	if len(report.Renamed) != 1 || report.Renamed[0].OldPath != "a.go" || report.Renamed[0].NewPath != "sub/a.go" {
		t.Fatalf("rename not detected as pure rename: %+v", report.Renamed)
	}

	after, err := eng.References(snap2, "Helper", graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Definitions) != 1 {
		t.Fatalf("want 1 definition after rename, got %d", len(after.Definitions))
	}
	d := after.Definitions[0]
	t.Logf("after rename: def at %s:%d id=%s key=%s resolved=%d unresolved=%d",
		d.Path, d.Line, d.SymbolID, d.Key, len(after.Resolved), len(after.Unresolved))

	// Judgment basis: Key derives from (name, kind, line, contentHash) — all
	// unchanged by a pure rename — so it must survive; ID embeds the path,
	// so it must move with the file.
	if d.Key != keyBefore {
		t.Fatalf("symbol key broke across rename: %s -> %s", keyBefore, d.Key)
	}
	if d.SymbolID == idBefore {
		t.Fatalf("symbol id should move with the path, still %s", d.SymbolID)
	}
	if d.Path != "sub/a.go" {
		t.Fatalf("definition path not updated: %s", d.Path)
	}
	if len(after.Resolved) != len(before.Resolved) {
		t.Fatalf("resolved reference count changed across rename: %d -> %d",
			len(before.Resolved), len(after.Resolved))
	}
	if len(after.Unresolved) != 0 {
		t.Fatalf("rename turned resolved references unresolved: %+v", after.Unresolved)
	}

	// Impact through the renamed file still chains to its consumers.
	imp, err := eng.Impact(snap2, []string{"sub/a.go"}, graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	var bgo *graph.AffectedFile
	for i := range imp.AffectedFiles {
		if imp.AffectedFiles[i].Path == "b.go" {
			bgo = &imp.AffectedFiles[i]
		}
	}
	if bgo == nil {
		t.Fatalf("b.go not affected by changed sub/a.go: %+v", imp.AffectedFiles)
	}
	if len(bgo.Via) != 1 || bgo.Via[0].ToPath != "sub/a.go" || bgo.Via[0].ToSymbolKey != keyBefore {
		t.Fatalf("impact chain broken across rename: %+v", bgo.Via)
	}
	t.Logf("impact after rename: b.go depth=%d via=%s:%d -%s-> %s key=%s (chain intact)",
		bgo.Depth, bgo.Via[0].FromPath, bgo.Via[0].FromLine, bgo.Via[0].Name, bgo.Via[0].ToPath, bgo.Via[0].ToSymbolKey)
}

// --- multi-definition attribution ------------------------------------------

// Same name, two definitions: the fixed rule must attribute references
// deterministically — same-file definition wins; otherwise the global first
// by (kind priority, path, line, ID) — and repeated queries must agree.
func TestMultiDefinitionAttributionIsDeterministic(t *testing.T) {
	files := map[string]string{
		// var Shared at line 3; body reference at line 5.
		"dd.go": "package main\n\nvar Shared = 1\n\nfunc UseD() int { return Shared }\n",
		"ee.go": "package main\n\nvar Shared = 2\n\nfunc UseE() int { return Shared }\n",
		// No local definition: falls back to the global rule.
		"bb.go": "package main\n\nfunc UseB() int { return Shared }\n",
	}
	root, storeDir := writeRepo(t, files)
	idx := newIndexer(t, root, storeDir)
	snap, _ := mustIndex(t, idx, root)
	eng := graph.NewEngine()

	out, err := eng.References(snap, "Shared", graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Definitions) != 2 {
		t.Fatalf("want 2 definitions, got %d", len(out.Definitions))
	}
	ddDef := defByPath(out, "dd.go")
	eeDef := defByPath(out, "ee.go")
	if ddDef == nil || eeDef == nil {
		t.Fatalf("definitions missing: %+v", out.Definitions)
	}
	// Precedence order: equal kind, path dd.go < ee.go decides.
	if out.Definitions[0].Path != "dd.go" {
		t.Fatalf("precedence order wrong: first=%s", out.Definitions[0].Path)
	}
	for _, d := range out.Definitions {
		t.Logf("def %s:%d id=%s refcount=%d", d.Path, d.Line, d.SymbolID, d.RefCount)
	}
	for _, r := range out.Resolved {
		t.Logf("resolved site %s:%d -> %s", r.Path, r.Line, r.SymbolID)
	}

	// Judgment basis per site:
	// - dd.go:5 has a same-file definition => dd def wins.
	// - ee.go:5 has a same-file definition => ee def wins.
	// - bb.go:3 has no same-file definition => global first (dd.go) wins.
	if s := siteAt(out, "dd.go", 5); s == nil || s.SymbolID != ddDef.SymbolID {
		t.Fatalf("dd.go:5 must attribute to same-file def %s, got %+v", ddDef.SymbolID, s)
	}
	if s := siteAt(out, "ee.go", 5); s == nil || s.SymbolID != eeDef.SymbolID {
		t.Fatalf("ee.go:5 must attribute to same-file def %s, got %+v", eeDef.SymbolID, s)
	}
	if s := siteAt(out, "bb.go", 3); s == nil || s.SymbolID != ddDef.SymbolID {
		t.Fatalf("bb.go:3 must attribute to global-first def %s, got %+v", ddDef.SymbolID, s)
	}
	if len(out.Unresolved) != 0 {
		t.Fatalf("all Shared references must resolve, got unresolved %+v", out.Unresolved)
	}

	// Repeated query on the same snapshot must produce the identical answer.
	again, err := eng.References(snap, "Shared", graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, again) {
		t.Fatalf("repeated query diverged:\nfirst=%+v\nsecond=%+v", out, again)
	}
	t.Logf("repeat query identical: %v", reflect.DeepEqual(out, again))
}

// --- impact from changed files ---------------------------------------------

// Changed files must map to affected files/symbols along explicit reference
// chains, transitively up to the depth cap, independent of input order.
func TestImpactFromChangedFiles(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\nfunc Helper() int { return 1 }\n",
		"b.go": "package main\n\nfunc UseAll() int { return Helper() }\n",
		"f.go": "package main\n\nfunc Deep() int { return UseAll() }\n",
		"g.go": "package main\n\nfunc TooDeep() int { return Deep() }\n",
	}
	root, storeDir := writeRepo(t, files)
	idx := newIndexer(t, root, storeDir)
	snap, _ := mustIndex(t, idx, root)
	eng := graph.NewEngine()

	out, err := eng.Impact(snap, []string{"a.go"}, graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]graph.AffectedFile{}
	for _, f := range out.AffectedFiles {
		byPath[f.Path] = f
		var via []string
		for _, e := range f.Via {
			via = append(via, e.FromPath+" -"+e.Name+"-> "+e.ToPath)
		}
		t.Logf("affected file %s depth=%d chain=%v", f.Path, f.Depth, via)
	}
	for _, s := range out.AffectedSymbols {
		t.Logf("affected symbol %s %s:%d depth=%d key=%s", s.Name, s.Path, s.Line, s.Depth, s.Key)
	}

	// Judgment basis: b.go references a.go's Helper (depth 1); f.go
	// references b.go's UseAll (depth 2); g.go is one hop beyond the
	// default MaxDepth=2 and must NOT appear.
	b := byPath["b.go"]
	if b.Path == "" || b.Depth != 1 || len(b.Via) != 1 {
		t.Fatalf("b.go must be depth-1 with a 1-edge chain, got %+v", b)
	}
	if b.Via[0].FromPath != "b.go" || b.Via[0].ToPath != "a.go" || b.Via[0].Name != "Helper" {
		t.Fatalf("b.go chain must be b.go -Helper-> a.go, got %+v", b.Via[0])
	}
	f := byPath["f.go"]
	if f.Path == "" || f.Depth != 2 || len(f.Via) != 2 {
		t.Fatalf("f.go must be depth-2 with a 2-edge chain, got %+v", f)
	}
	if f.Via[1].FromPath != "f.go" || f.Via[1].ToPath != "b.go" || f.Via[1].Name != "UseAll" {
		t.Fatalf("f.go second hop must be f.go -UseAll-> b.go, got %+v", f.Via[1])
	}
	if _, ok := byPath["g.go"]; ok {
		t.Fatalf("g.go is depth 3, beyond MaxDepth=2, must be excluded: %+v", byPath["g.go"])
	}

	// Symbols: Helper (in changed file, referenced) at depth 0; UseAll
	// (propagates impact to f.go) at depth 1.
	symDepth := map[string]int{}
	for _, s := range out.AffectedSymbols {
		symDepth[s.Name] = s.Depth
	}
	if d, ok := symDepth["Helper"]; !ok || d != 0 {
		t.Fatalf("Helper must be an affected symbol at depth 0, got %v", symDepth)
	}
	if d, ok := symDepth["UseAll"]; !ok || d != 1 {
		t.Fatalf("UseAll must be an affected symbol at depth 1, got %v", symDepth)
	}

	// Input order must not matter.
	out2, err := eng.Impact(snap, []string{"g.go", "a.go"}, graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	out3, err := eng.Impact(snap, []string{"a.go", "g.go"}, graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out2, out3) {
		t.Fatalf("input order changed the outcome:\n%v\nvs\n%v", out2.AffectedFiles, out3.AffectedFiles)
	}
	t.Logf("order independence: [g.go a.go] == [a.go g.go]: %v", reflect.DeepEqual(out2, out3))

	// Depth cap is a dial: MaxDepth=3 reaches g.go.
	deep, err := eng.Impact(snap, []string{"a.go"}, graph.Budget{MaxDepth: 3})
	if err != nil {
		t.Fatal(err)
	}
	foundG := false
	for _, af := range deep.AffectedFiles {
		if af.Path == "g.go" {
			foundG = true
		}
	}
	if !foundG {
		t.Fatalf("MaxDepth=3 must reach g.go: %+v", deep.AffectedFiles)
	}
	t.Logf("MaxDepth=3 reaches g.go: %v", foundG)
}

// --- unresolved names -------------------------------------------------------

// A name with references but no definition must land entirely in the
// Unresolved list, never mixed into Resolved.
func TestUnresolvedNameHasNoDefinition(t *testing.T) {
	files := map[string]string{
		"c.go": "package main\n\nfunc UseMissing() int { return Missing() }\n",
	}
	root, storeDir := writeRepo(t, files)
	idx := newIndexer(t, root, storeDir)
	snap, _ := mustIndex(t, idx, root)
	eng := graph.NewEngine()

	out, err := eng.References(snap, "Missing", graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("query=Missing definitions=%d resolved=%d unresolved=%d",
		len(out.Definitions), len(out.Resolved), len(out.Unresolved))
	for _, u := range out.Unresolved {
		t.Logf("unresolved site %s:%d:%d (name matched, no definition located)", u.Path, u.Line, u.Column)
	}
	if len(out.Definitions) != 0 {
		t.Fatalf("Missing has no definition, got %+v", out.Definitions)
	}
	if len(out.Resolved) != 0 {
		t.Fatalf("nothing may resolve without a definition, got %+v", out.Resolved)
	}
	if len(out.Unresolved) != 1 || out.Unresolved[0].Path != "c.go" || out.Unresolved[0].Line != 3 {
		t.Fatalf("the Missing() call at c.go:3 must be the single unresolved site, got %+v", out.Unresolved)
	}
}

// --- excluded & non-text files ----------------------------------------------

// Files with unusual extensions and non-UTF-8 files must appear as typed
// unanalyzable entries in both refs and impact outcomes — never vanish.
func TestExcludedAndBinaryFilesAreAccountedFor(t *testing.T) {
	files := map[string]string{
		"a.go":        "package main\n\nfunc Helper() int { return 1 }\n",
		"notes.weird": "Helper is mentioned here but the extension is not indexed\n",
		"bin/data.go": "package main\n\nfunc Broken( {\n\xff\xfe binary garbage",
	}
	root, storeDir := writeRepo(t, files)
	idx := newIndexer(t, root, storeDir)
	snap, report := mustIndex(t, idx, root)
	for _, f := range report.Failed {
		t.Logf("index failure: [%s] %s: %s", f.Category, f.Path, f.Message)
	}

	cat := map[string]model.FailureCategory{}
	for _, f := range snap.Failures {
		cat[f.Path] = f.Category
	}
	// Judgment basis: .weird is outside source.DefaultIncludeExt =>
	// "excluded"; data.go is selected but not valid UTF-8 => "invalid_utf8".
	if cat["notes.weird"] != model.FailureExcluded {
		t.Fatalf("notes.weird must be categorized excluded, got %q", cat["notes.weird"])
	}
	if cat["bin/data.go"] != model.FailureInvalidUTF8 {
		t.Fatalf("bin/data.go must be categorized invalid_utf8, got %q", cat["bin/data.go"])
	}

	eng := graph.NewEngine()
	refs, err := eng.References(snap, "Helper", graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range refs.Unanalyzable {
		t.Logf("refs unanalyzable: [%s] %s", u.Category, u.Path)
	}
	if len(refs.Unanalyzable) != 2 {
		t.Fatalf("both bad files must be listed as unanalyzable in refs, got %+v", refs.Unanalyzable)
	}

	// Impact: a changed input that is unanalyzable gets an explicit note;
	// a path unknown to the snapshot is reported, not dropped.
	imp, err := eng.Impact(snap, []string{"notes.weird", "ghost.go", "a.go"}, graph.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range imp.Notes {
		t.Logf("impact note: %s", n)
	}
	if len(imp.UnknownInputs) != 1 || imp.UnknownInputs[0] != "ghost.go" {
		t.Fatalf("ghost.go must be reported as unknown input, got %+v", imp.UnknownInputs)
	}
	noted := false
	for _, n := range imp.Notes {
		if strings.Contains(n, "notes.weird") && strings.Contains(n, "unanalyzable") && strings.Contains(n, "excluded") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("changed input notes.weird must be explained as unanalyzable/excluded, notes=%v", imp.Notes)
	}
	if len(imp.Unanalyzable) != 2 {
		t.Fatalf("both bad files must be listed as unanalyzable in impact, got %+v", imp.Unanalyzable)
	}
}
