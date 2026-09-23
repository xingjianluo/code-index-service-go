// Package retrieval assembles explainable, budget-bounded context from
// immutable snapshots.
//
// Ranking is deterministic and explainable:
//
//	symbol-name exact term match ............ +15
//	content exact phrase match (whole query)  +30
//	term occurrence in content .............. +2 each
//	term occurrence in path ................. +10 once
//	tie-break: higher score, then path, then start line, then chunk ID.
//
// Budget handling never silently truncates: candidates that do not fit are
// recorded in Result.Dropped with the reason that excluded them
// ("token_budget", "chunk_budget", "scan_cap"), and Result.TruncatedBy names
// the first limiting resource when applicable.
package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/xingjianluo/code-index-service-go/internal/model"
)

// Default budget values applied for zero-valued fields.
const (
	DefaultMaxChunks        = 20
	DefaultMaxTokens        = 4000
	DefaultMaxChunksScanned = 10000
)

// Budget limits the resources a single query may consume.
type Budget struct {
	MaxChunks        int // maximum number of returned chunks
	MaxTokens        int // maximum total estimated tokens of returned chunks
	MaxChunksScanned int // hard cap on chunks examined
}

func (b Budget) withDefaults() Budget {
	if b.MaxChunks <= 0 {
		b.MaxChunks = DefaultMaxChunks
	}
	if b.MaxTokens <= 0 {
		b.MaxTokens = DefaultMaxTokens
	}
	if b.MaxChunksScanned <= 0 {
		b.MaxChunksScanned = DefaultMaxChunksScanned
	}
	return b
}

// Hit is one retrieved chunk with score and an explanation.
type Hit struct {
	Chunk        model.Chunk
	Score        int
	MatchedTerms []string
	Why          []string
}

// Dropped records a candidate omitted due to the budget, with a reason.
type Dropped struct {
	ChunkID string
	Path    string
	Reason  string
}

// Result is the deterministic outcome of a query.
type Result struct {
	Hits        []Hit
	TotalHits   int // candidates with score > 0 before budgeting
	Dropped     []Dropped
	Terms       []string
	TruncatedBy string // "tokens" | "chunks" | "scan_cap" | ""
}

// Engine runs queries against snapshots.
type Engine struct {
	budget Budget
}

// New constructs an Engine with defaults filled in.
func New(b Budget) *Engine {
	return &Engine{budget: b.withDefaults()}
}

// Budget returns the configured (defaulted) budget.
func (e *Engine) Budget() Budget { return e.budget }

// tokenize splits text into lowercase alphanumeric word tokens, de-duplicating
// while preserving first-seen order.
func tokenize(text string) []string {
	var toks []string
	seen := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		w := strings.ToLower(cur.String())
		cur.Reset()
		if !seen[w] {
			seen[w] = true
			toks = append(toks, w)
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return toks
}

// Query runs a deterministic, explainable search over snap. An empty query
// (no tokens) returns an empty result, not the whole index.
func (e *Engine) Query(ctx context.Context, snap *model.Snapshot, query string) Result {
	terms := tokenize(query)
	res := Result{
		Hits:    []Hit{},
		Dropped: []Dropped{},
		Terms:   terms,
	}
	if snap == nil || len(terms) == 0 {
		return res
	}

	type scored struct {
		hit Hit
	}
	var candidates []scored

	// Enumerate chunks in a stable order (path, start line, kind, id).
	paths := make([]string, 0, len(snap.Files))
	for p := range snap.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	scanned := 0
	scanLimited := false
outer:
	for _, p := range paths {
		chunks := append([]model.Chunk(nil), snap.Files[p].Chunks...)
		sort.Slice(chunks, func(i, j int) bool {
			if chunks[i].StartLine != chunks[j].StartLine {
				return chunks[i].StartLine < chunks[j].StartLine
			}
			if chunks[i].EndLine != chunks[j].EndLine {
				return chunks[i].EndLine < chunks[j].EndLine
			}
			return chunks[i].ID < chunks[j].ID
		})
		for _, ch := range chunks {
			if scanned >= e.budget.MaxChunksScanned {
				scanLimited = true
				break outer
			}
			scanned++
			if err := ctx.Err(); err != nil {
				return res
			}
			if hit, ok := scoreChunk(ch, terms, query); ok {
				candidates = append(candidates, scored{hit: hit})
			}
		}
	}

	res.TotalHits = len(candidates)
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].hit, candidates[j].hit
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Chunk.Path != b.Chunk.Path {
			return a.Chunk.Path < b.Chunk.Path
		}
		if a.Chunk.StartLine != b.Chunk.StartLine {
			return a.Chunk.StartLine < b.Chunk.StartLine
		}
		return a.Chunk.ID < b.Chunk.ID
	})

	usedTokens := 0
	tokenLimited, chunkLimited := false, false
	for _, c := range candidates {
		h := c.hit
		switch {
		case usedTokens+h.Chunk.Tokens > e.budget.MaxTokens:
			res.Dropped = append(res.Dropped, Dropped{
				ChunkID: h.Chunk.ID, Path: h.Chunk.Path, Reason: "token_budget"})
			tokenLimited = true
		case len(res.Hits) >= e.budget.MaxChunks:
			res.Dropped = append(res.Dropped, Dropped{
				ChunkID: h.Chunk.ID, Path: h.Chunk.Path, Reason: "chunk_budget"})
			chunkLimited = true
		default:
			res.Hits = append(res.Hits, h)
			usedTokens += h.Chunk.Tokens
		}
	}

	switch {
	case scanLimited:
		res.TruncatedBy = "scan_cap"
	case tokenLimited:
		res.TruncatedBy = "tokens"
	case chunkLimited:
		res.TruncatedBy = "chunks"
	}
	return res
}

// scoreChunk computes the explainable score of one chunk against the query.
func scoreChunk(ch model.Chunk, terms []string, rawQuery string) (Hit, bool) {
	content := strings.ToLower(ch.Content)
	path := strings.ToLower(ch.Path)
	sym := strings.ToLower(ch.SymbolName)

	score := 0
	matched := map[string]bool{}
	var why []string

	if ch.Kind == "symbol" {
		symTerms := map[string]bool{}
		for _, t := range tokenize(ch.SymbolName) {
			symTerms[t] = true
		}
		n := 0
		for _, t := range terms {
			if symTerms[t] {
				n++
				matched[t] = true
			}
		}
		if n > 0 {
			score += 15 * n
			why = append(why, whyf("symbol name matches %d query term(s): +%d", n, 15*n))
		}
	}

	// Exact phrase: the normalized raw query appears verbatim in content.
	phrase := strings.ToLower(strings.Join(terms, " "))
	if phrase != "" && strings.Contains(content, phrase) {
		score += 30
		why = append(why, "exact phrase match in content: +30")
		for _, t := range terms {
			matched[t] = true
		}
	}

	tf := 0
	for _, t := range terms {
		c := strings.Count(content, t)
		if c > 0 {
			tf += c
			matched[t] = true
		}
	}
	if tf > 0 {
		score += 2 * tf
		why = append(why, whyf("content term occurrences x%d: +%d", tf, 2*tf))
	}

	pathHits := 0
	for _, t := range terms {
		if strings.Contains(path, t) {
			pathHits++
			matched[t] = true
		}
	}
	if pathHits > 0 {
		score += 10
		why = append(why, whyf("path matches %d query term(s): +10", pathHits))
	}
	_ = sym

	if score == 0 {
		return Hit{}, false
	}
	mt := make([]string, 0, len(matched))
	for t := range matched {
		mt = append(mt, t)
	}
	sort.Strings(mt)
	why = append(why, whyf("total score %d", score))
	return Hit{
		Chunk:        ch,
		Score:        score,
		MatchedTerms: mt,
		Why:          why,
	}, true
}

func whyf(format string, args ...any) string {
	return "score: " + fmt.Sprintf(format, args...)
}
