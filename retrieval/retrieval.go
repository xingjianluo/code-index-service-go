// Package retrieval assembles explainable, budget-bounded search results.
//
// Ranking is fully deterministic and explained per hit via Breakdown:
//
//	score = 3.0*titleHits + 1.5*distinctQueryTokensMatched
//	      + 0.25*totalTokenHits + 1.0*declarationBoost + 0.01*symbolDensity
//
// Ties break by (score desc, path asc, startLine asc, chunkID asc), so the
// same snapshot+query always produces the same answer.
//
// Budget enforcement never silently corrupts a result:
//   - candidates are capped by Budget.MaxCandidates (omissions are explained);
//   - each chunk's content is cut to MaxChunkChars on a line boundary;
//   - results are admitted in rank order while MaxResults and MaxTotalChars
//     hold; non-admission is recorded in Truncation with its reason;
//   - if the budget admits zero results though matches exist, Search returns
//     ErrBudgetUnsatisfiable with an explanatory message.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
)

// Budget bounds retrieval resource use and result size.
type Budget struct {
	MaxResults    int // 0 => default 10
	MaxTotalChars int // 0 => default 20000; hard ceiling on assembled content
	MaxCandidates int // 0 => default 400; max chunks considered
	MaxChunkChars int // 0 => default 4000; per-chunk content ceiling
}

func (b Budget) withDefaults() Budget {
	if b.MaxResults <= 0 {
		b.MaxResults = 10
	}
	if b.MaxTotalChars <= 0 {
		b.MaxTotalChars = 20000
	}
	if b.MaxCandidates <= 0 {
		b.MaxCandidates = 400
	}
	if b.MaxChunkChars <= 0 {
		b.MaxChunkChars = 4000
	}
	return b
}

// ErrBudgetUnsatisfiable indicates matches exist but the given budget cannot
// admit any result (never returned as a silent empty answer).
var ErrBudgetUnsatisfiable = errors.New("retrieval: budget unsatisfiable for any matching result")

// Result is one assembled, budget-checked hit.
type Result struct {
	ChunkID   string   `json:"chunk_id"`
	Path      string   `json:"path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Title     string   `json:"title,omitempty"`
	Score     float64  `json:"score"`
	Matched   []string `json:"matched"`
	Content   string   `json:"content"`
	Chars     int      `json:"chars"`
}

// Truncation records deterministic, explainable budget-based omission.
type Truncation struct {
	Reason  string  `json:"reason"`
	ChunkID string  `json:"chunk_id"`
	Score   float64 `json:"score,omitempty"`
	Detail  string  `json:"detail"`
}

// Outcome is a complete, explainable retrieval answer.
type Outcome struct {
	Query      string       `json:"query"`
	SnapshotID string       `json:"snapshot_id"`
	Results    []Result     `json:"results"`
	Truncated  []Truncation `json:"truncated"`
	Notes      []string     `json:"notes"`
}

type scored struct {
	chunk     *model.Chunk
	score     float64
	breakdown map[string]float64
	matched   []string
}

// Engine searches an immutable snapshot under a budget.
type Engine struct{}

// NewEngine creates a retrieval Engine.
func NewEngine() *Engine { return &Engine{} }

// Search returns deterministically ordered, explainable results.
func (e *Engine) Search(ctx context.Context, snap *model.Snapshot, query string, b Budget) (*Outcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errors.New("retrieval: nil snapshot (no index published)")
	}
	b = b.withDefaults()
	outcome := &Outcome{
		Query:      query,
		SnapshotID: snap.ID,
		Results:    []Result{},
		Truncated:  []Truncation{},
		Notes:      []string{},
	}

	queryTokens := parser.Tokenize(query)
	outcome.Notes = append(outcome.Notes,
		fmt.Sprintf("query=%q normalized=%v budget={results:%d chars:%d candidates:%d chunkChars:%d}",
			query, queryTokens, b.MaxResults, b.MaxTotalChars, b.MaxCandidates, b.MaxChunkChars))
	if len(queryTokens) == 0 {
		return nil, errors.New("retrieval: query normalizes to no tokens")
	}

	// Enumerate chunks in a fixed (path,startLine) order before capping
	// candidates, so the cap is itself deterministic.
	var chunks []*model.Chunk
	for _, rec := range snap.Files {
		for i := range rec.Chunks {
			chunks = append(chunks, &rec.Chunks[i])
		}
	}
	sort.Slice(chunks, func(i, j int) bool {
		a, c := chunks[i], chunks[j]
		if a.Path != c.Path {
			return a.Path < c.Path
		}
		if a.StartLine != c.StartLine {
			return a.StartLine < c.StartLine
		}
		return a.ID < c.ID
	})
	if len(chunks) > b.MaxCandidates {
		outcome.Truncated = append(outcome.Truncated, Truncation{
			Reason: "candidate_cap",
			Detail: fmt.Sprintf("%d chunks present, only first %d considered in deterministic (path,line) order",
				len(chunks), b.MaxCandidates),
		})
		chunks = chunks[:b.MaxCandidates]
	}

	var hits []scored
	for _, ch := range chunks {
		freq := tokenFreq(ch.Tokens)
		var (
			distinct   float64
			totalHits  float64
			titleHits  float64
			matchedSet = map[string]bool{}
		)
		for _, qt := range queryTokens {
			n := float64(freq[qt])
			if n > 0 {
				totalHits += n
				distinct++
				matchedSet[qt] = true
			}
			if ch.Title != "" {
				for _, tt := range parser.Tokenize(ch.Title) {
					if tt == qt {
						titleHits++
						break
					}
				}
			}
		}
		if distinct == 0 {
			continue
		}
		declBoost := 0.0
		if ch.Kind == "declaration" {
			declBoost = 1
		}
		symbolDensity := 0.0
		if len(ch.Tokens) > 0 {
			symbolDensity = float64(len(ch.SymbolIDs)) / float64(len(ch.Tokens)) * 100
		}
		score := 3.0*titleHits + 1.5*distinct + 0.25*totalHits + declBoost + 0.01*symbolDensity
		matched := make([]string, 0, len(matchedSet))
		for m := range matchedSet {
			matched = append(matched, m)
		}
		sort.Strings(matched)
		hits = append(hits, scored{
			chunk: ch,
			score: score,
			breakdown: map[string]float64{
				"title_hits":          3.0 * titleHits,
				"distinct_terms":      1.5 * distinct,
				"term_frequency":      0.25 * totalHits,
				"declaration_boost":   declBoost,
				"symbol_density_0.01": 0.01 * symbolDensity,
			},
			matched: matched,
		})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		a, c := hits[i], hits[j]
		if a.score != c.score {
			return a.score > c.score
		}
		if a.chunk.Path != c.chunk.Path {
			return a.chunk.Path < c.chunk.Path
		}
		if a.chunk.StartLine != c.chunk.StartLine {
			return a.chunk.StartLine < c.chunk.StartLine
		}
		return a.chunk.ID < c.chunk.ID
	})

	usedChars := 0
	matchedCount := len(hits)
	for rank, h := range hits {
		if len(outcome.Results) >= b.MaxResults {
			outcome.Truncated = append(outcome.Truncated, Truncation{
				Reason: "max_results", ChunkID: h.chunk.ID, Score: h.score,
				Detail: fmt.Sprintf("rank %d dropped: MaxResults=%d reached", rank+1, b.MaxResults),
			})
			continue
		}
		body, fits := truncateOnLine(h.chunk.Content, b.MaxChunkChars)
		if !fits {
			outcome.Truncated = append(outcome.Truncated, Truncation{
				Reason: "max_chunk_chars", ChunkID: h.chunk.ID, Score: h.score,
				Detail: fmt.Sprintf("rank %d cannot fit even the truncation marker under MaxChunkChars=%d (chunk %d bytes)",
					rank+1, b.MaxChunkChars, len(h.chunk.Content)),
			})
			continue
		}
		cost := len(body)
		if usedChars+cost > b.MaxTotalChars {
			outcome.Truncated = append(outcome.Truncated, Truncation{
				Reason: "max_total_chars", ChunkID: h.chunk.ID, Score: h.score,
				Detail: fmt.Sprintf("rank %d needs %d chars, remaining budget %d", rank+1, cost, b.MaxTotalChars-usedChars),
			})
			continue
		}
		usedChars += cost
		outcome.Results = append(outcome.Results, Result{
			ChunkID:   h.chunk.ID,
			Path:      h.chunk.Path,
			StartLine: h.chunk.StartLine,
			EndLine:   h.chunk.EndLine,
			Title:     h.chunk.Title,
			Score:     h.score,
			Matched:   h.matched,
			Content:   body,
			Chars:     cost,
		})
	}

	if matchedCount > 0 && len(outcome.Results) == 0 {
		return nil, fmt.Errorf("%w: %d chunks matched but budget {MaxTotalChars:%d MaxChunkChars:%d} admits none",
			ErrBudgetUnsatisfiable, matchedCount, b.MaxTotalChars, b.MaxChunkChars)
	}
	outcome.Notes = append(outcome.Notes, fmt.Sprintf("matched_chunks=%d returned=%d used_chars=%d/%d",
		matchedCount, len(outcome.Results), usedChars, b.MaxTotalChars))
	return outcome, nil
}

func tokenFreq(tokens []string) map[string]int {
	m := make(map[string]int, len(tokens))
	for _, t := range tokens {
		m[t]++
	}
	return m
}

// truncateOnLine cuts content to at most max bytes, ending on a line
// boundary, with an explicit marker counted against the limit. ok is false
// when the budget is too small to hold even the explanation marker: callers
// must treat that as non-admission (never as an empty/silent result).
func truncateOnLine(content string, max int) (body string, ok bool) {
	if len(content) <= max {
		return content, true
	}
	marker := fmt.Sprintf("\n…[truncated; chunk has %d bytes]", len(content))
	if max <= len(marker) {
		return "", false
	}
	budget := max - len(marker)
	cut := content[:budget]
	if idx := strings.LastIndexByte(cut, '\n'); idx > 0 {
		cut = cut[:idx]
	}
	return cut + marker, true
}
