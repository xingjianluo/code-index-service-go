package relation

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/xingjianluo/code-index-service-go/model"
)

// symbolIndex maps symbol ID -> (symbol, its file's content hash) so resolved
// references can be annotated with the definition's rename-stable key.
type symbolIndex map[string]DefinitionView

func buildSymbolIndex(snap *model.Snapshot) symbolIndex {
	idx := make(symbolIndex)
	for _, rec := range snap.Files {
		for _, sym := range rec.Symbols {
			idx[sym.ID] = DefinitionView{
				Symbol: sym,
				Key:    model.StableSymbolKey(rec.ContentHash, sym.Name, sym.Kind, sym.Line),
			}
		}
	}
	return idx
}

// References lists every occurrence of name, split by resolution status.
//
// Determinism: files are visited in path order, occurrences in (line, column)
// order, definitions in (path, line, kind, id) order. The same snapshot and
// name therefore always produce byte-identical output regardless of map
// iteration or call order. Attribution of a resolved occurrence to one of
// several same-name definitions was fixed at index time by
// parser.ResolveReferences (same-file wins, then kind priority, path, line);
// this query only reports that fixed attribution, it never re-decides it.
func (e *Engine) References(ctx context.Context, snap *model.Snapshot, name string, l Limits) (*ReferencesOutcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errNilSnapshot
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("relation: symbol name is required")
	}
	l = l.withDefaults()
	idx := buildSymbolIndex(snap)

	out := &ReferencesOutcome{
		Name:        name,
		SnapshotID:  snap.ID,
		Definitions: []DefinitionView{},
		Resolved:    []ReferenceHit{},
		Unresolved:  []ReferenceHit{},
		Truncated:   []Truncation{},
		Notes:       []string{},
	}

	// Definitions of this name, in deterministic (path, line, kind, id) order.
	for _, rec := range snap.Files {
		for _, sym := range rec.Symbols {
			if sym.Name == name {
				out.Definitions = append(out.Definitions, idx[sym.ID])
			}
		}
	}
	sort.Slice(out.Definitions, func(i, j int) bool {
		a, b := out.Definitions[i], out.Definitions[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
	if len(out.Definitions) > l.MaxDefinitions {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_definitions",
			Detail: fmt.Sprintf("%d definitions of %q, kept first %d in (path,line,kind,id) order",
				len(out.Definitions), name, l.MaxDefinitions),
		})
		out.Definitions = out.Definitions[:l.MaxDefinitions]
	}

	// Occurrences: walk files in path order, references in (line, column)
	// order, and split strictly by whether a definition was bound at index time.
	paths := make([]string, 0, len(snap.Files))
	for p := range snap.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		rec := snap.Files[p]
		refs := append([]model.Reference(nil), rec.References...)
		sort.SliceStable(refs, func(i, j int) bool {
			if refs[i].Line != refs[j].Line {
				return refs[i].Line < refs[j].Line
			}
			return refs[i].Column < refs[j].Column
		})
		for _, ref := range refs {
			if ref.Name != name {
				continue
			}
			hit := ReferenceHit{Path: ref.Path, Line: ref.Line, Column: ref.Column}
			if ref.SymbolID == "" {
				out.Unresolved = append(out.Unresolved, hit)
				continue
			}
			hit.SymbolID = ref.SymbolID
			if def, ok := idx[ref.SymbolID]; ok {
				hit.TargetKey = def.Key
				hit.TargetPath = def.Path
				hit.TargetLine = def.Line
			}
			out.Resolved = append(out.Resolved, hit)
		}
	}

	if len(out.Resolved) > l.MaxReferences {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_references",
			Detail: fmt.Sprintf("%d resolved occurrences of %q, kept first %d in (path,line,column) order",
				len(out.Resolved), name, l.MaxReferences),
		})
		out.Resolved = out.Resolved[:l.MaxReferences]
	}
	if len(out.Unresolved) > l.MaxReferences {
		out.Truncated = append(out.Truncated, Truncation{
			Reason: "max_references",
			Detail: fmt.Sprintf("%d unresolved occurrences of %q, kept first %d in (path,line,column) order",
				len(out.Unresolved), name, l.MaxReferences),
		})
		out.Unresolved = out.Unresolved[:l.MaxReferences]
	}

	out.Notes = append(out.Notes, fmt.Sprintf(
		"name=%q definitions=%d resolved=%d unresolved=%d limits={definitions:%d references:%d}",
		name, len(out.Definitions), len(out.Resolved), len(out.Unresolved),
		l.MaxDefinitions, l.MaxReferences))
	out.Notes = append(out.Notes,
		"attribution: fixed at index time (same-file definition wins, then kind priority func<type<method<var<const<field, then path, line); query never re-decides")
	return out, nil
}
