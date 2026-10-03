package graph

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xingjianluo/code-index-service-go/model"
)

// Edge is one resolved reference link: FromPath contains a reference to the
// symbol Name defined at ToPath. A chain of edges explains how an impact
// propagates from a changed file to an affected one.
type Edge struct {
	FromPath    string `json:"from_path"`
	FromLine    int    `json:"from_line"`
	FromColumn  int    `json:"from_column"`
	Name        string `json:"name"`
	ToPath      string `json:"to_path"`
	ToSymbolID  string `json:"to_symbol_id"`
	ToSymbolKey string `json:"to_symbol_key"`
}

// AffectedFile is a non-changed file in the blast radius, with the
// reference chain that connects it back to a changed file.
type AffectedFile struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"` // 1 = directly references a changed file's symbol
	Via   []Edge `json:"via"`   // chain from the changed file to this file
}

// AffectedSymbol is a definition whose consumers place it in the blast
// radius. Depth 0 means defined in a changed file and referenced elsewhere.
type AffectedSymbol struct {
	SymbolID string           `json:"symbol_id"`
	Key      string           `json:"key"`
	Name     string           `json:"name"`
	Kind     model.SymbolKind `json:"kind"`
	Path     string           `json:"path"`
	Line     int              `json:"line"`
	Depth    int              `json:"depth"`
	Via      []Edge           `json:"via,omitempty"` // chain by which a consumer reaches it (empty at depth 0)
}

// ImpactOutcome is the answer to "these files changed; who may break".
type ImpactOutcome struct {
	Changed         []string         `json:"changed"`
	SnapshotID      string           `json:"snapshot_id"`
	AffectedFiles   []AffectedFile   `json:"affected_files"`
	AffectedSymbols []AffectedSymbol `json:"affected_symbols"`
	// UnknownInputs are changed paths absent from the snapshot (deleted or
	// never discovered); they are reported, not silently dropped.
	UnknownInputs []string `json:"unknown_inputs,omitempty"`
	// Unanalyzable covers changed inputs (and any other files) the index
	// holds only as typed failures (excluded extension, invalid UTF-8, ...):
	// their symbols/references are unknowable, so impact through them is
	// bounded to this explicit note.
	Unanalyzable []model.Failure `json:"unanalyzable"`
	Truncated    []Truncation    `json:"truncated"`
	Notes        []string        `json:"notes"`
}

// Impact computes the blast radius of the given changed files by walking
// resolved reference edges backwards (consumer -> definition), up to
// b.MaxDepth hops, under deterministic caps.
//
// Method:
//  1. changed paths are normalized (slash-separated, deduped, sorted) and
//     classified: indexed file / typed failure (unanalyzable) / unknown;
//  2. BFS: every resolved reference whose target symbol is defined in a
//     reached file makes the referencing file affected at depth+1; the
//     traversed edge is appended to that file's Via chain, so each affected
//     entry shows exactly which reference links connect it to a changed
//     file;
//  3. traversal order is fully deterministic (files and symbols in
//     path/line order, consumer edges in (fromPath, line, column) order),
//     so caps always drop the same, explained suffix.
func (e *Engine) Impact(snap *model.Snapshot, changed []string, b Budget) (*ImpactOutcome, error) {
	if snap == nil {
		return nil, errors.New("graph: nil snapshot (no index published)")
	}
	b = b.withDefaults()

	// 1. Normalize and classify inputs.
	norm := map[string]bool{}
	for _, c := range changed {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		norm[strings.ReplaceAll(c, "\\", "/")] = true
	}
	inputs := make([]string, 0, len(norm))
	for c := range norm {
		inputs = append(inputs, c)
	}
	sort.Strings(inputs)

	out := &ImpactOutcome{Changed: inputs, SnapshotID: snap.ID}
	out.Notes = append(out.Notes, fmt.Sprintf(
		"changed_inputs=%d snapshot=%s budget={files:%d symbols:%d depth:%d edges:%d unanalyzable:%d}",
		len(inputs), snap.ID, b.MaxAffectedFiles, b.MaxAffectedSymbol, b.MaxDepth, b.MaxEdges, b.MaxUnanalyzable))

	failureByPath := map[string]model.Failure{}
	for _, f := range snap.Failures {
		if _, ok := failureByPath[f.Path]; !ok {
			failureByPath[f.Path] = f
		}
	}
	changedSet := map[string]bool{}
	for _, c := range inputs {
		switch {
		case snap.Files[c] != nil:
			changedSet[c] = true
		case failureByPath[c].Path != "":
			out.Notes = append(out.Notes, fmt.Sprintf(
				"changed input %q is unanalyzable [%s]: %s", c, failureByPath[c].Category, failureByPath[c].Message))
		default:
			out.UnknownInputs = append(out.UnknownInputs, c)
		}
	}
	if len(out.UnknownInputs) > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%d changed inputs are not in the snapshot (deleted or never indexed): %s",
			len(out.UnknownInputs), strings.Join(out.UnknownInputs, ", ")))
	}

	// 2. Index symbols by ID and consumer edges by target symbol.
	symByID := map[string]model.Symbol{}
	symsByPath := map[string][]model.Symbol{}
	for _, rec := range snap.Files {
		for _, sym := range rec.Symbols {
			symByID[sym.ID] = sym
			symsByPath[rec.Path] = append(symsByPath[rec.Path], sym)
		}
	}
	for p := range symsByPath {
		sort.Slice(symsByPath[p], func(i, j int) bool {
			a, c := symsByPath[p][i], symsByPath[p][j]
			if a.Line != c.Line {
				return a.Line < c.Line
			}
			return a.ID < c.ID
		})
	}
	edgesByTarget := map[string][]Edge{}
	for _, rec := range snap.Files {
		for _, ref := range rec.References {
			if ref.SymbolID == "" {
				continue
			}
			tgt, ok := symByID[ref.SymbolID]
			if !ok {
				continue
			}
			edgesByTarget[ref.SymbolID] = append(edgesByTarget[ref.SymbolID], Edge{
				FromPath:    ref.Path,
				FromLine:    ref.Line,
				FromColumn:  ref.Column,
				Name:        ref.Name,
				ToPath:      tgt.Path,
				ToSymbolID:  tgt.ID,
				ToSymbolKey: tgt.Key,
			})
		}
	}
	for id := range edgesByTarget {
		sort.Slice(edgesByTarget[id], func(i, j int) bool {
			a, c := edgesByTarget[id][i], edgesByTarget[id][j]
			if a.FromPath != c.FromPath {
				return a.FromPath < c.FromPath
			}
			if a.FromLine != c.FromLine {
				return a.FromLine < c.FromLine
			}
			return a.FromColumn < c.FromColumn
		})
	}

	// 3. BFS from changed files.
	type reach struct {
		depth int
		via   []Edge
	}
	reached := map[string]reach{} // file path -> how it was reached (changed files: depth 0, empty via)
	queue := []string{}
	for _, c := range inputs {
		if changedSet[c] {
			reached[c] = reach{depth: 0}
			queue = append(queue, c)
		}
	}
	symSeen := map[string]bool{}
	edgesUsed := 0
	edgesCapHit := false
	filesOmitted, symsOmitted := 0, 0

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		rc := reached[cur]
		if rc.depth >= b.MaxDepth {
			continue
		}
		for _, sym := range symsByPath[cur] {
			for _, edge := range edgesByTarget[sym.ID] {
				if edgesUsed >= b.MaxEdges {
					edgesCapHit = true
					continue
				}
				edgesUsed++
				// The referenced symbol itself is in the blast radius.
				if !symSeen[sym.ID] {
					symSeen[sym.ID] = true
					if len(out.AffectedSymbols) < b.MaxAffectedSymbol {
						out.AffectedSymbols = append(out.AffectedSymbols, AffectedSymbol{
							SymbolID: sym.ID,
							Key:      sym.Key,
							Name:     sym.Name,
							Kind:     sym.Kind,
							Path:     sym.Path,
							Line:     sym.Line,
							Depth:    rc.depth,
							Via:      append([]Edge(nil), rc.via...),
						})
					} else {
						symsOmitted++
					}
				}
				consumer := edge.FromPath
				if _, ok := reached[consumer]; ok {
					continue // already reached at an equal-or-shallower depth
				}
				via := append(append([]Edge(nil), rc.via...), edge)
				reached[consumer] = reach{depth: rc.depth + 1, via: via}
				queue = append(queue, consumer)
				if len(out.AffectedFiles) < b.MaxAffectedFiles {
					out.AffectedFiles = append(out.AffectedFiles, AffectedFile{
						Path:  consumer,
						Depth: rc.depth + 1,
						Via:   via,
					})
				} else {
					filesOmitted++
				}
			}
		}
	}
	if edgesCapHit {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_edges",
			Detail: fmt.Sprintf("edge traversal stopped at MaxEdges=%d; remaining consumer edges were not explored", b.MaxEdges),
		})
	}
	if filesOmitted > 0 {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_affected_files",
			Detail: fmt.Sprintf("%d affected files omitted beyond MaxAffectedFiles=%d (deterministic BFS order)", filesOmitted, b.MaxAffectedFiles),
		})
	}
	if symsOmitted > 0 {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_affected_symbols",
			Detail: fmt.Sprintf("%d affected symbols omitted beyond MaxAffectedSymbol=%d (deterministic BFS order)", symsOmitted, b.MaxAffectedSymbol),
		})
	}

	// 4. Unanalyzable files (failed or excluded at index time): impact
	// through them is unknowable, so they are surfaced explicitly, bounded.
	failures := append([]model.Failure(nil), snap.Failures...)
	sort.SliceStable(failures, func(i, j int) bool {
		if failures[i].Path != failures[j].Path {
			return failures[i].Path < failures[j].Path
		}
		return failures[i].Category < failures[j].Category
	})
	if len(failures) > b.MaxUnanalyzable {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_unanalyzable",
			Detail: fmt.Sprintf("%d unanalyzable files, listing first %d in (path,category) order; omitted %d",
				len(failures), b.MaxUnanalyzable, len(failures)-b.MaxUnanalyzable),
		})
		failures = failures[:b.MaxUnanalyzable]
	}
	out.Unanalyzable = failures

	out.Notes = append(out.Notes, fmt.Sprintf(
		"affected_files=%d affected_symbols=%d edges_traversed=%d unknown_inputs=%d unanalyzable_files=%d",
		len(out.AffectedFiles), len(out.AffectedSymbols), edgesUsed, len(out.UnknownInputs), len(out.Unanalyzable)))
	return out, nil
}
