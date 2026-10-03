// Package graph answers symbol-relationship questions over an immutable
// snapshot: "where is this name referenced" (References) and "who is
// affected by these changed files" (Impact).
//
// Everything is derived from the local index only; no external services.
// All enumeration orders are deterministic (path/line/column, then IDs), so
// the same snapshot + the same query always yields the same answer, and
// budget caps drop a deterministic, explained suffix rather than an
// arbitrary subset.
package graph

import (
	"errors"
	"fmt"
	"sort"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
)

// Budget bounds graph queries. Zero fields take the defaults documented in
// withDefaults. Caps are hard: exceeding items are dropped in deterministic
// order and recorded in Truncated with their reason.
type Budget struct {
	MaxDefinitions    int // 0 => default 50; definitions listed per refs query
	MaxReferences     int // 0 => default 500; reference sites per refs query
	MaxAffectedFiles  int // 0 => default 200; files listed per impact query
	MaxAffectedSymbol int // 0 => default 200; symbols listed per impact query
	MaxDepth          int // 0 => default 2; reference-chain depth for impact
	MaxEdges          int // 0 => default 2000; total edges traversed by impact BFS
	MaxUnanalyzable   int // 0 => default 100; failed/excluded files listed
}

func (b Budget) withDefaults() Budget {
	if b.MaxDefinitions <= 0 {
		b.MaxDefinitions = 50
	}
	if b.MaxReferences <= 0 {
		b.MaxReferences = 500
	}
	if b.MaxAffectedFiles <= 0 {
		b.MaxAffectedFiles = 200
	}
	if b.MaxAffectedSymbol <= 0 {
		b.MaxAffectedSymbol = 200
	}
	if b.MaxDepth <= 0 {
		b.MaxDepth = 2
	}
	if b.MaxEdges <= 0 {
		b.MaxEdges = 2000
	}
	if b.MaxUnanalyzable <= 0 {
		b.MaxUnanalyzable = 100
	}
	return b
}

// Truncation records a deterministic, explained budget-based omission.
type Truncation struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Definition is one definition site of a queried name.
type Definition struct {
	SymbolID string           `json:"symbol_id"`
	Key      string           `json:"key"` // path-independent; survives pure renames
	Name     string           `json:"name"`
	Kind     model.SymbolKind `json:"kind"`
	Path     string           `json:"path"`
	Line     int              `json:"line"`
	Column   int              `json:"column"`
	// RefCount counts resolved references pointing at this definition. A
	// same-name definition with RefCount 0 exists but never wins the
	// deterministic attribution rule (see Notes).
	RefCount int `json:"ref_count"`
}

// RefSite is one occurrence of the queried name in indexed source.
type RefSite struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	SymbolID string `json:"symbol_id,omitempty"` // resolved target; empty => unresolved
}

// RefsOutcome is the answer to "where is NAME referenced". Resolved and
// unresolved sites are kept in separate lists: a resolved site is backed by
// a definition in Definitions; an unresolved site only matched the name.
type RefsOutcome struct {
	Query         string          `json:"query"`
	SnapshotID    string          `json:"snapshot_id"`
	Definitions   []Definition    `json:"definitions"`
	Resolved      []RefSite       `json:"resolved"`
	Unresolved    []RefSite       `json:"unresolved"`
	Unanalyzable  []model.Failure `json:"unanalyzable"` // files whose references are unknowable
	Truncated     []Truncation    `json:"truncated"`
	Notes         []string        `json:"notes"`
}

// Engine answers graph queries over one immutable snapshot.
type Engine struct{}

// NewEngine creates a graph Engine.
func NewEngine() *Engine { return &Engine{} }

// References lists definition and reference sites for name, split into
// resolved (attributed to a definition by the fixed precedence rule) and
// unresolved (name matched, no definition located).
//
// Attribution rule (identical to index-time resolution, see
// parser.LessSymbolPrecedence): a reference resolves to a same-file
// definition when one exists (lowest line, then kind priority, then ID);
// otherwise to the global first definition by (kind priority, path, line,
// ID). The rule depends only on snapshot content, so repeated queries and
// query order never change the outcome.
func (e *Engine) References(snap *model.Snapshot, name string, b Budget) (*RefsOutcome, error) {
	if snap == nil {
		return nil, errors.New("graph: nil snapshot (no index published)")
	}
	if name == "" {
		return nil, errors.New("graph: empty symbol name")
	}
	b = b.withDefaults()
	out := &RefsOutcome{
		Query:      name,
		SnapshotID: snap.ID,
	}
	out.Notes = append(out.Notes, fmt.Sprintf(
		"query=%q snapshot=%s budget={defs:%d refs:%d unanalyzable:%d}",
		name, snap.ID, b.MaxDefinitions, b.MaxReferences, b.MaxUnanalyzable))
	out.Notes = append(out.Notes,
		"attribution rule: same-file definition wins (lowest line, kind priority func<type<method<var<const<field, symbol ID); "+
			"otherwise global first by (kind priority, path, line, ID)")

	// Definitions: every symbol with this name, in precedence order.
	var defs []model.Symbol
	// Reference sites, split by resolvability.
	var resolved, unresolved []RefSite
	refCount := map[string]int{}

	paths := make([]string, 0, len(snap.Files))
	for p := range snap.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		rec := snap.Files[p]
		for _, sym := range rec.Symbols {
			if sym.Name == name {
				defs = append(defs, sym)
			}
		}
		for _, ref := range rec.References {
			if ref.Name != name {
				continue
			}
			site := RefSite{Path: ref.Path, Line: ref.Line, Column: ref.Column, SymbolID: ref.SymbolID}
			if ref.SymbolID == "" {
				unresolved = append(unresolved, site)
			} else {
				resolved = append(resolved, site)
				refCount[ref.SymbolID]++
			}
		}
	}
	sort.Slice(defs, func(i, j int) bool { return parser.LessSymbolPrecedence(defs[i], defs[j]) })
	sortSites(resolved)
	sortSites(unresolved)

	if len(defs) > b.MaxDefinitions {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_definitions",
			Detail: fmt.Sprintf("%d definitions of %q, keeping first %d in precedence order; omitted %d (all with zero or equal precedence rank)",
				len(defs), name, b.MaxDefinitions, len(defs)-b.MaxDefinitions),
		})
		defs = defs[:b.MaxDefinitions]
	}
	for _, sym := range defs {
		out.Definitions = append(out.Definitions, Definition{
			SymbolID: sym.ID,
			Key:      sym.Key,
			Name:     sym.Name,
			Kind:     sym.Kind,
			Path:     sym.Path,
			Line:     sym.Line,
			Column:   sym.Column,
			RefCount: refCount[sym.ID],
		})
	}
	if len(resolved) > b.MaxReferences {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_references",
			Detail: fmt.Sprintf("%d resolved reference sites, keeping first %d in (path,line,column) order; omitted %d",
				len(resolved), b.MaxReferences, len(resolved)-b.MaxReferences),
		})
		resolved = resolved[:b.MaxReferences]
	}
	if len(unresolved) > b.MaxReferences {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_references",
			Detail: fmt.Sprintf("%d unresolved name matches, keeping first %d in (path,line,column) order; omitted %d",
				len(unresolved), b.MaxReferences, len(unresolved)-b.MaxReferences),
		})
		unresolved = unresolved[:b.MaxReferences]
	}
	out.Resolved = resolved
	out.Unresolved = unresolved

	// Files the index could not analyze: references to this name may exist
	// there but are unknowable from the index. Listed explicitly (bounded),
	// never silently absent.
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
		"definitions=%d resolved=%d unresolved=%d unanalyzable_files=%d",
		len(out.Definitions), len(out.Resolved), len(out.Unresolved), len(out.Unanalyzable)))
	return out, nil
}

// sortSites orders reference sites deterministically by (path, line, column).
func sortSites(sites []RefSite) {
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].Path != sites[j].Path {
			return sites[i].Path < sites[j].Path
		}
		if sites[i].Line != sites[j].Line {
			return sites[i].Line < sites[j].Line
		}
		return sites[i].Column < sites[j].Column
	})
}
