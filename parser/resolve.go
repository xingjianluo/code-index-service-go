package parser

import (
	"sort"

	"github.com/xingjianluo/code-index-service-go/model"
)

// kindPriority gives a deterministic resolution order when several symbols
// share a name (e.g. a function and a method). The resolution is purely
// name-based and intentionally conservative; unresolved references retain an
// empty SymbolID and remain queryable by their Name token.
var kindPriority = map[model.SymbolKind]int{
	model.SymbolFunc:   0,
	model.SymbolType:   1,
	model.SymbolMethod: 2,
	model.SymbolVar:    3,
	model.SymbolConst:  4,
	model.SymbolField:  5,
}

// LessSymbolPrecedence is the single source of truth for the deterministic
// ordering of same-name symbols: kind priority, then defining path, then
// line, then symbol ID. Reference resolution (below) and read-side
// attribution reporting (package graph) both sort with it, so a name always
// maps to the same definition for a given snapshot regardless of query
// order or repetition.
func LessSymbolPrecedence(a, b model.Symbol) bool {
	if pa, pb := kindPriority[a.Kind], kindPriority[b.Kind]; pa != pb {
		return pa < pb
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.ID < b.ID
}

// ResolveReferences assigns reference.SymbolID targets across all records
// using deterministic precedence so repeated indexings resolve identically:
//
//  1. a symbol with the same name in the SAME file wins;
//  2. among same-file matches: lower line, then kind priority, then symbol ID;
//  3. otherwise a symbol in another file wins, ordered by (kind priority,
//     defining path, line, symbol ID).
func (p *Parser) ResolveReferences(records []*model.FileRecord) {
	byName := make(map[string][]model.Symbol, 64)
	for _, rec := range records {
		for _, sym := range rec.Symbols {
			byName[sym.Name] = append(byName[sym.Name], sym)
		}
	}
	for name := range byName {
		set := byName[name]
		sort.Slice(set, func(i, j int) bool { return LessSymbolPrecedence(set[i], set[j]) })
		byName[name] = set
	}

	for _, rec := range records {
		cands := make(map[string][]model.Symbol)
		for name, set := range byName {
			for _, sym := range set {
				if sym.Path == rec.Path {
					cands[name] = append(cands[name], sym)
				}
			}
			if len(cands[name]) == 0 {
				cands[name] = set // global fallback ordering
			}
		}
		for idx := range rec.References {
			ref := &rec.References[idx]
			if set := cands[ref.Name]; len(set) > 0 {
				ref.SymbolID = set[0].ID
			}
		}
	}
}
