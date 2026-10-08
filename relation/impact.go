package relation

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/xingjianluo/code-index-service-go/model"
)

// dep is one collected dependency of a file on a changed-file symbol name.
type dep struct {
	name        string // symbol name depended on
	changedFrom string // a changed file defining this name (deterministic pick)
	bound       *DefinitionView
	line        int
	col         int
	occurrences int
}

// Impact computes which other files may be affected by changes to
// changedFiles, each with the reference chain that connects it.
//
// Semantics:
//   - A changed file that is indexed contributes its defined symbols as
//     change sources. Any OTHER file holding a reference resolved (at index
//     time) to one of those symbols is directly impacted; the chain is
//     "changedFile --defines--> symbol --referenced-at--> impactedFile:line".
//   - suspected: an occurrence whose name is also defined in a changed file
//     but which was bound to a definition elsewhere (multi-definition name,
//     global fallback). Editing the changed file can shift that attribution,
//     so these are listed separately and never mixed into the direct list.
//     Occurrences bound to a definition in the SAME file are provably
//     unaffected (same-file wins regardless of other files) and excluded.
//   - Changed files that failed indexing keep their typed failure category;
//     changed files the scan saw but never parsed are reported as "skipped"
//     with their skip category (extension_not_indexed / non_text); changed
//     files the scan never saw at all are "unknown" (wrong path, deleted, or
//     under a pruned directory). None is silently dropped, and "skipped" is
//     never conflated with "unknown".
//
// Determinism: changed inputs are normalized, deduped and sorted; files and
// references are visited in (path, line, column) order; all caps cut a
// deterministically ordered list and record a Truncation.
func (e *Engine) Impact(ctx context.Context, snap *model.Snapshot, changedFiles []string, l Limits) (*ImpactOutcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errNilSnapshot
	}
	l = l.withDefaults()

	out := &ImpactOutcome{
		Changed:    []ChangedFileStatus{},
		Impacted:   []ImpactedFile{},
		Suspected:  []ImpactedFile{},
		SnapshotID: snap.ID,
		Truncated:  []Truncation{},
		Notes:      []string{},
	}

	// Normalize + dedupe + sort the input so the answer does not depend on
	// the order the caller listed the files in.
	changed := normalizePaths(changedFiles)
	if len(changed) == 0 {
		return nil, fmt.Errorf("relation: at least one changed file is required")
	}

	failedByPath := make(map[string]model.Failure, len(snap.Failures))
	for _, f := range snap.Failures {
		failedByPath[f.Path] = f
	}
	skippedByPath := make(map[string]model.SkippedFile, len(snap.Skipped))
	for _, sk := range snap.Skipped {
		skippedByPath[sk.Path] = sk
	}

	// Classify every changed file explicitly.
	changedSet := make(map[string]bool, len(changed))
	for _, p := range changed {
		changedSet[p] = true
		switch {
		case snap.Files[p] != nil:
			out.Changed = append(out.Changed, ChangedFileStatus{Path: p, Status: "indexed"})
		case failedByPath[p].Path != "":
			f := failedByPath[p]
			out.Changed = append(out.Changed, ChangedFileStatus{
				Path: p, Status: "failed", Category: string(f.Category), Detail: f.Message,
			})
		case skippedByPath[p].Path != "":
			sk := skippedByPath[p]
			out.Changed = append(out.Changed, ChangedFileStatus{
				Path: p, Status: "skipped", Category: string(sk.Category),
				Detail: sk.Detail + "; contributes no symbol-based impact",
			})
		default:
			out.Changed = append(out.Changed, ChangedFileStatus{
				Path: p, Status: "unknown",
				Detail: "path not seen during the last scan (does not exist, was deleted since, or lives under a pruned directory); contributes no symbol-based impact",
			})
		}
	}

	// Change sources: symbols defined in indexed changed files.
	// sources: symbol ID -> definition view; nameSource: name -> first (in
	// sorted changed-file order) changed path defining it.
	sources := make(map[string]DefinitionView)
	nameSource := make(map[string]string)
	for _, p := range changed {
		rec := snap.Files[p]
		if rec == nil {
			continue
		}
		for _, sym := range rec.Symbols {
			sources[sym.ID] = DefinitionView{
				Symbol: sym,
				Key:    model.StableSymbolKey(rec.ContentHash, sym.Name, sym.Kind, sym.Line),
			}
			if _, ok := nameSource[sym.Name]; !ok {
				nameSource[sym.Name] = p
			}
		}
	}
	idx := buildSymbolIndex(snap)

	// Walk all other files in deterministic order and collect dependencies.
	direct := map[string][]dep{}
	suspected := map[string][]dep{}
	paths := make([]string, 0, len(snap.Files))
	for p := range snap.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if changedSet[p] {
			continue // a changed file depending on itself is not "impact"
		}
		rec := snap.Files[p]
		refs := append([]model.Reference(nil), rec.References...)
		sort.SliceStable(refs, func(i, j int) bool {
			if refs[i].Line != refs[j].Line {
				return refs[i].Line < refs[j].Line
			}
			return refs[i].Column < refs[j].Column
		})
		for _, ref := range refs {
			if ref.SymbolID == "" {
				continue // unresolved occurrences depend on no definition
			}
			from, isSourceName := nameSource[ref.Name]
			if !isSourceName {
				continue
			}
			if def, ok := sources[ref.SymbolID]; ok {
				d := dep{name: ref.Name, changedFrom: def.Path, line: ref.Line, col: ref.Column, occurrences: 1}
				d.bound = &def
				direct[p] = append(direct[p], d)
				continue
			}
			// Bound elsewhere though a changed file defines the same name:
			// attribution-sensitive. Same-file binding is provably stable.
			if def, ok := idx[ref.SymbolID]; ok && def.Path != p {
				d := dep{name: ref.Name, changedFrom: from, line: ref.Line, col: ref.Column, occurrences: 1}
				d.bound = &def
				suspected[p] = append(suspected[p], d)
			}
		}
	}

	out.Impacted = buildImpacted(direct, false, l, &out.Truncated)
	out.Suspected = buildImpacted(suspected, true, l, &out.Truncated)

	out.Notes = append(out.Notes, fmt.Sprintf(
		"changed=%d indexed=%d skipped=%d unknown=%d source_symbols=%d impacted=%d suspected=%d limits={impacted:%d chainsPerFile:%d}",
		len(changed), countStatus(out.Changed, "indexed"), countStatus(out.Changed, "skipped"),
		countStatus(out.Changed, "unknown"), len(sources),
		len(out.Impacted), len(out.Suspected), l.MaxImpacted, l.MaxChainEdges))
	out.Notes = append(out.Notes,
		"rule: direct = reference resolved at index time to a symbol defined in a changed file; suspected = same-name occurrence bound to a definition in another unchanged file (attribution may shift)")
	out.Notes = append(out.Notes,
		"changed-file classes: indexed (parsed) / failed (typed index failure) / skipped (scanned but never parsed: extension_not_indexed or non_text) / unknown (never seen by the scan)")
	return out, nil
}

// buildImpacted turns collected deps into sorted ImpactedFile entries with
// per-file chains, applying MaxImpacted / MaxChainEdges deterministically.
func buildImpacted(deps map[string][]dep, suspected bool, l Limits, trunc *[]Truncation) []ImpactedFile {
	label := "direct"
	refKind := "references"
	if suspected {
		label = "suspected"
		refKind = "references_bound_elsewhere"
	}
	paths := make([]string, 0, len(deps))
	for p := range deps {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) > l.MaxImpacted {
		*trunc = append(*trunc, Truncation{
			Reason: "max_impacted",
			Detail: fmt.Sprintf("%d %s-impacted files, kept first %d in path order",
				len(paths), label, l.MaxImpacted),
		})
		paths = paths[:l.MaxImpacted]
	}
	out := make([]ImpactedFile, 0, len(paths))
	for _, p := range paths {
		list := deps[p]
		// One chain per distinct symbol, keeping the earliest occurrence as
		// evidence; extra occurrences are counted in the edge detail.
		bySym := map[string][]dep{}
		var symOrder []string
		for _, d := range list {
			if _, ok := bySym[d.name]; !ok {
				symOrder = append(symOrder, d.name)
			}
			bySym[d.name] = append(bySym[d.name], d)
		}
		sort.Strings(symOrder)
		imp := ImpactedFile{Path: p, Symbols: symOrder, Chains: [][]Edge{}}
		kept := symOrder
		if len(kept) > l.MaxChainEdges {
			*trunc = append(*trunc, Truncation{
				Reason: "max_chain_edges",
				Detail: fmt.Sprintf("file %s has %d %s symbol dependencies, kept first %d chains (symbol name order)",
					p, len(symOrder), label, l.MaxChainEdges),
			})
			kept = kept[:l.MaxChainEdges]
		}
		for _, name := range kept {
			ds := bySym[name]
			first := ds[0]
			defEdge := Edge{
				Kind: "defines",
				From: first.changedFrom,
				To:   name,
			}
			refEdge := Edge{
				Kind: refKind,
				From: fmt.Sprintf("%s:%d", p, first.line),
				To:   name,
			}
			if suspected && first.bound != nil {
				defEdge.Detail = "name also defined in this changed file"
				refEdge.Detail = fmt.Sprintf("bound to same-name definition at %s:%d; editing %s may re-attribute this occurrence (%d occurrence(s) in file)",
					first.bound.Path, first.bound.Line, first.changedFrom, len(ds))
			} else if first.bound != nil {
				defEdge.Detail = fmt.Sprintf("defined at %s:%d key=%s",
					first.bound.Path, first.bound.Line, first.bound.Key)
				refEdge.Detail = fmt.Sprintf("resolved to %s:%d; %d occurrence(s) in this file, earliest at line %d",
					first.bound.Path, first.bound.Line, len(ds), first.line)
			}
			imp.Chains = append(imp.Chains, []Edge{defEdge, refEdge})
		}
		out = append(out, imp)
	}
	return out
}

func normalizePaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
		p = strings.TrimPrefix(p, "./")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func countStatus(list []ChangedFileStatus, status string) int {
	n := 0
	for _, c := range list {
		if c.Status == status {
			n++
		}
	}
	return n
}
