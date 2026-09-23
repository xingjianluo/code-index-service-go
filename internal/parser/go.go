package parser

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"sort"
	"strings"

	"github.com/xingjianluo/code-index-service-go/internal/model"
)

// chunkSpan is a 1-based inclusive line span of a function/method body.
type chunkSpan struct {
	name, recv string
	start, end int
}

// parseGoSource parses Go source. A syntax error is reported as a
// categorized parse_error failure (with position) rather than dropped.
func parseGoSource(relPath string, data []byte, hash string) Outcome {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, data, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return fail(relPath, model.FailureParse, err.Error(), int64(len(data)))
	}

	offsets := lineOffsets(data)
	totalLines := len(offsets) - 1
	f := &model.File{
		ID:       model.FileID(hash),
		Path:     relPath,
		Language: model.LangGo,
		Size:     int64(len(data)),
		Hash:     hash,
		Lines:    totalLines,
		Symbols:  []model.Symbol{},
		Refs:     []model.Reference{},
	}

	// declPositions records name -> (line,col) of declaration sites, so that
	// identifier occurrences at those spots are not double-counted as refs.
	type pos struct{ line, col int }
	declPositions := map[string][]pos{}

	addSymbol := func(name string, kind model.SymbolKind, start token.Pos, receiver, sig string) {
		p := fset.Position(start)
		sym := model.Symbol{
			ID:        model.SymbolID(hash, kind, receiver, name, p.Line, p.Column),
			Name:      name,
			Kind:      kind,
			Line:      p.Line,
			Column:    p.Column,
			Receiver:  receiver,
			Signature: sig,
		}
		f.Symbols = append(f.Symbols, sym)
		declPositions[name] = append(declPositions[name], pos{p.Line, p.Column})
	}

	var spans []chunkSpan

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			kind := model.SymbolFunc
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = model.SymbolMethod
				recv = receiverName(d.Recv.List[0].Type)
			}
			addSymbol(name, kind, d.Name.Pos(), recv, nodeString(fset, d.Type))
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			spans = append(spans, chunkSpan{name: name, recv: recv, start: start, end: end})
		case *ast.GenDecl:
			kind := genKind(d.Tok)
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					addSymbol(s.Name.Name, model.SymbolType, s.Name.Pos(), "", nodeString(fset, s.Type))
				case *ast.ValueSpec:
					for _, n := range s.Names {
						addSymbol(n.Name, kind, n.Pos(), "", "")
					}
				}
			}
		}
	}

	// References: every identifier occurrence that is not a declaration site.
	type rawRef struct {
		name      string
		line, col int
	}
	var rawRefs []rawRef
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		p := fset.Position(id.Pos())
		for _, dp := range declPositions[id.Name] {
			if dp.line == p.Line && dp.col == p.Column {
				return true // declaration occurrence
			}
		}
		rawRefs = append(rawRefs, rawRef{id.Name, p.Line, p.Column})
		return true
	})
	for _, rr := range rawRefs {
		f.Refs = append(f.Refs, model.Reference{
			ID:     model.RefID(hash, rr.name, rr.line, rr.col),
			Name:   rr.name,
			Line:   rr.line,
			Column: rr.col,
		})
	}

	// Symbol chunks for functions/methods.
	for _, sp := range spans {
		content := sliceLines(data, offsets, sp.start, sp.end)
		label := sp.name
		if sp.recv != "" {
			label = "(" + sp.recv + ")." + sp.name
		}
		f.Chunks = append(f.Chunks, model.Chunk{
			ID:         model.ChunkID(hash, "symbol", label, sp.start, sp.end, content),
			FileID:     f.ID,
			Path:       relPath,
			Language:   model.LangGo,
			StartLine:  sp.start,
			EndLine:    sp.end,
			Kind:       "symbol",
			SymbolName: label,
			Content:    content,
			Tokens:     model.EstimateTokens(content),
		})
	}

	// Text chunks for all lines not already covered by a symbol chunk, so the
	// whole file (imports, types, vars, package clause) remains searchable.
	cov := coveredRanges(spans, totalLines)
	lines := bytes.Split(data, []byte("\n"))
	isBlank := func(ln int) bool {
		if ln < 1 || ln > len(lines) {
			return false
		}
		return len(bytes.TrimSpace(lines[ln-1])) == 0
	}
	for _, gap := range cov {
		for _, blk := range chunkBlocks(gap[1]-gap[0]+1, func(ln int) bool {
			return isBlank(gap[0] + ln - 1)
		}) {
			start := gap[0] + blk[0] - 1
			end := gap[0] + blk[1] - 1
			content := sliceLines(data, offsets, start, end)
			f.Chunks = append(f.Chunks, model.Chunk{
				ID:        model.ChunkID(hash, "text", "", start, end, content),
				FileID:    f.ID,
				Path:      relPath,
				Language:  model.LangGo,
				StartLine: start,
				EndLine:   end,
				Kind:      "text",
				Content:   content,
				Tokens:    model.EstimateTokens(content),
			})
		}
	}

	// Deterministic ordering of all extracted slices.
	sort.Slice(f.Symbols, func(i, j int) bool {
		if f.Symbols[i].Line != f.Symbols[j].Line {
			return f.Symbols[i].Line < f.Symbols[j].Line
		}
		return f.Symbols[i].Column < f.Symbols[j].Column
	})
	sort.Slice(f.Refs, func(i, j int) bool {
		if f.Refs[i].Line != f.Refs[j].Line {
			return f.Refs[i].Line < f.Refs[j].Line
		}
		return f.Refs[i].Column < f.Refs[j].Column
	})
	sort.Slice(f.Chunks, func(i, j int) bool {
		if f.Chunks[i].StartLine != f.Chunks[j].StartLine {
			return f.Chunks[i].StartLine < f.Chunks[j].StartLine
		}
		if f.Chunks[i].EndLine != f.Chunks[j].EndLine {
			return f.Chunks[i].EndLine < f.Chunks[j].EndLine
		}
		return f.Chunks[i].Kind < f.Chunks[j].Kind
	})
	return Outcome{File: f}
}

func genKind(tok token.Token) model.SymbolKind {
	switch tok {
	case token.TYPE:
		return model.SymbolType
	case token.CONST:
		return model.SymbolConst
	default:
		return model.SymbolVar
	}
}

// receiverName extracts a bare receiver type name, stripping pointers and
// package qualifiers: *ast.Node -> Node, []T -> T.
func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", expr), "*ast.")
}

func nodeString(fset *token.FileSet, n any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, n); err != nil {
		return ""
	}
	return buf.String()
}

// coveredRanges returns the 1-based inclusive line ranges NOT covered by any
// chunkSpan, i.e. the gaps available for plain text chunks.
func coveredRanges(spans []chunkSpan, total int) [][2]int {
	type iv struct{ lo, hi int }
	var ivs []iv
	for _, s := range spans {
		if s.end < s.start {
			continue
		}
		ivs = append(ivs, iv{s.start, s.end})
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].lo < ivs[j].lo })
	var gaps [][2]int
	cursor := 1
	for _, in := range ivs {
		if in.lo > cursor {
			gaps = append(gaps, [2]int{cursor, in.lo - 1})
		}
		if in.hi >= cursor {
			cursor = in.hi + 1
		}
	}
	if cursor <= total {
		gaps = append(gaps, [2]int{cursor, total})
	}
	return gaps
}
