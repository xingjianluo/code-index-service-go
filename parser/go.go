package parser

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/source"
)

// parseGo parses Go source with the strict syntax parser. Any syntax error
// produces an "unparseable" failure carrying parser position/text; no AST is
// returned and nothing is silently dropped.
func parseGo(f source.DiscoveredFile, content string, size int64, fileHash string, opts Options) (*model.FileRecord, *parseFailure) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f.RelPath, content, parser.ParseComments|parser.AllErrors|parser.SkipObjectResolution)
	if err != nil {
		return nil, &parseFailure{message: fmt.Sprintf("go parser: %v", err)}
	}

	lines := splitLines(content)
	rec := &model.FileRecord{
		Path:       f.RelPath,
		Language:   "go",
		Size:       size,
		Lines:      len(lines),
		Symbols:    []model.Symbol{},
		References: []model.Reference{},
		Metadata: map[string]string{
			"parser":  "go-ast-v2",
			"package": file.Name.Name,
		},
	}
	if strings.HasSuffix(f.RelPath, "_test.go") {
		rec.Metadata["test"] = "true"
	}

	pos := func(p token.Pos) (int, int) {
		pp := fset.Position(p)
		return pp.Line, pp.Column
	}

	addSym := func(name string, kind model.SymbolKind, p token.Pos) model.Symbol {
		line, col := pos(p)
		sym := model.Symbol{
			Name:   name,
			Kind:   kind,
			Path:   f.RelPath,
			Line:   line,
			Column: col,
		}
		sym.ID = model.StableSymbolID(f.RelPath, name, kind, line)
		sym.Key = model.StableSymbolKey(name, kind, line, fileHash)
		rec.Symbols = append(rec.Symbols, sym)
		return sym
	}

	var spans []goSpan

	flushGenDecl := func(gd *ast.GenDecl) {
		startLine := fset.Position(gd.Pos()).Line
		endLine := fset.Position(gd.End()).Line
		ids := map[string]bool{}
		for _, spec := range gd.Specs {
			switch ss := spec.(type) {
			case *ast.TypeSpec:
				sym := addSym(ss.Name.Name, model.SymbolType, ss.Name.Pos())
				ids[sym.ID] = true
				if st, ok := ss.Type.(*ast.StructType); ok && st.Fields != nil {
					for _, field := range st.Fields.List {
						for _, n := range field.Names {
							fsym := addSym(n.Name, model.SymbolField, n.Pos())
							ids[fsym.ID] = true
						}
					}
				}
				if it, ok := ss.Type.(*ast.InterfaceType); ok && it.Methods != nil {
					for _, field := range it.Methods.List {
						for _, n := range field.Names {
							msym := addSym(n.Name, model.SymbolMethod, n.Pos())
							ids[msym.ID] = true
						}
					}
				}
			case *ast.ValueSpec:
				kind := model.SymbolVar
				if gd.Tok == token.CONST {
					kind = model.SymbolConst
				}
				for _, n := range ss.Names {
					if n.Name == "_" {
						continue
					}
					sym := addSym(n.Name, kind, n.Pos())
					ids[sym.ID] = true
				}
			}
		}
		spans = append(spans, goSpan{
			startLine: startLine,
			endLine:   endLine,
			kind:      "declaration",
			title:     fmt.Sprintf("%s %s (line %d)", strings.ToLower(gd.Tok.String()), f.RelPath, startLine),
			symbolIDs: mapKeys(ids),
		})
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			kind := model.SymbolFunc
			title := "func " + name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = model.SymbolMethod
				recv := receiverName(d.Recv.List[0].Type)
				title = "method (" + recv + ")." + name
			}
			sym := addSym(name, kind, d.Name.Pos())
			spans = append(spans, goSpan{
				startLine: fset.Position(d.Pos()).Line,
				endLine:   fset.Position(d.End()).Line,
				kind:      "declaration",
				title:     title,
				symbolIDs: []string{sym.ID},
			})
		case *ast.GenDecl:
			flushGenDecl(d)
		}
	}

	// References: every identifier node. The package declaration name is not a
	// use of a file-level symbol, so skip it.
	pkgNameLine := fset.Position(file.Name.Pos()).Line
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if id.Name == "_" {
			return true
		}
		line, col := pos(id.Pos())
		if line == pkgNameLine && id.Name == file.Name.Name {
			// Only skip the package clause identifier itself (column match).
			pkgCol := fset.Position(file.Name.Pos()).Column
			if col == pkgCol {
				return true
			}
		}
		rec.References = append(rec.References, model.Reference{
			Name:   id.Name,
			Path:   f.RelPath,
			Line:   line,
			Column: col,
		})
		return true
	})

	// Build chunks from declaration spans, splitting spans larger than the
	// chunk line cap. Leading lines before the first declaration form a
	// "file" header chunk (package/imports).
	chunks := buildGoChunks(f.RelPath, lines, fileHash, spans, opts.MaxChunkLines)
	rec.Chunks = chunks
	return rec, nil
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver[T]
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	}
	return ""
}

// goSpan marks one top-level declaration's line extent.
type goSpan struct {
	startLine int
	endLine   int
	kind      string
	title     string
	symbolIDs []string
}

func buildGoChunks(path string, lines []string, fileHash string, spans []goSpan, maxLines int) []model.Chunk {
	if maxLines <= 0 {
		maxLines = 60
	}
	var chunks []model.Chunk
	cursor := 1
	flush := func(start, end int, kind, title string, symIDs []string) {
		if start > end || start < 1 || end > len(lines) {
			return
		}
		body := strings.Join(lines[start-1:end], "\n")
		if strings.TrimSpace(body) == "" {
			return // blank gap between declarations carries no searchable content
		}
		ch := model.Chunk{
			Path:        path,
			StartLine:   start,
			EndLine:     end,
			Kind:        kind,
			Title:       title,
			Content:     body,
			Tokens:      Tokenize(body),
			SymbolIDs:   symIDs,
			ContentHash: model.HashBytes([]byte(body)),
		}
		ch.ID = model.StableChunkID(path, start, end, fileHash, ch.ContentHash)
		chunks = append(chunks, ch)
	}

	for _, sp := range spans {
		if sp.startLine > cursor {
			flush(cursor, sp.startLine-1, "file", path+" (header)", nil)
		}
		start := sp.startLine
		for {
			end := start + maxLines - 1
			if end >= sp.endLine {
				flush(start, sp.endLine, sp.kind, sp.title, sp.symbolIDs)
				break
			}
			flush(start, end, sp.kind, sp.title, sp.symbolIDs)
			start = end + 1
		}
		cursor = sp.endLine + 1
	}
	if cursor <= len(lines) {
		flush(cursor, len(lines), "file", path+" (tail)", nil)
	}
	return chunks
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
