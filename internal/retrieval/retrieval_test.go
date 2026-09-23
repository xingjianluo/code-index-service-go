package retrieval_test

import (
	"context"
	"testing"

	"github.com/xingjianluo/code-index-service-go/internal/model"
	"github.com/xingjianluo/code-index-service-go/internal/retrieval"
)

func ch(id, path, kind, sym, content string, start, tokens int) model.Chunk {
	return model.Chunk{
		ID: id, FileID: "f", Path: path, Language: model.LangGo,
		StartLine: start, EndLine: start + 1, Kind: kind,
		SymbolName: sym, Content: content, Tokens: tokens,
	}
}

func snapWith(chunks ...model.Chunk) *model.Snapshot {
	return &model.Snapshot{
		FormatVersion: model.FormatV1,
		RuleVersion:   model.RulesV1,
		Generation:    1,
		Files: map[string]*model.File{
			"file": {ID: "f", Path: "file", Chunks: chunks},
		},
	}
}

func TestQueryRankingIsDeterministicAndExplained(t *testing.T) {
	symChunk := ch("c1", "parser/parse.go", "symbol", "ParseSymbols",
		"func ParseSymbols() { tokens := parse() }", 10, 5)
	weakChunk := ch("c2", "other/readme.txt", "text", "",
		"parse appeared once long ago", 1, 5)
	phraseChunk := ch("c3", "guide/doc.md", "text", "",
		"here we parse symbols carefully across parse symbols twice", 4, 12)
	snap := snapWith(symChunk, weakChunk, phraseChunk)

	e := retrieval.New(retrieval.Budget{})
	r1 := e.Query(context.Background(), snap, "parse symbols")
	r2 := e.Query(context.Background(), snap, "parse symbols")

	t.Logf("KEY INPUT query=%q candidates=%d order=%v", "parse symbols", r1.TotalHits, order(r1))
	if len(r1.Hits) != 3 || r1.TotalHits != 3 {
		t.Fatalf("expected 3 hits, got %d (total=%d)", len(r1.Hits), r1.TotalHits)
	}
	if !equalOrder(r1, r2) {
		t.Fatalf("repeated query gives different order: %v vs %v", order(r1), order(r2))
	}
	// c3: exact phrase +30 plus four term occurrences +8 = 38.
	// c1: path bonus (+10, "parser/parse.go" contains "parse") plus two
	// substring occurrences of "parse" (+4) = 14 (SymbolName is one word
	// "parsesymbols", so no exact symbol-term match; this is documented).
	// c2: single content occurrence (+2).
	wantOrder := []string{"c3", "c1", "c2"}
	if got := order(r1); got[0] != wantOrder[0] || got[1] != wantOrder[1] || got[2] != wantOrder[2] {
		t.Fatalf("order = %v, want %v (scores c3=%d c1=%d c2=%d)",
			got, wantOrder, r1.Hits[0].Score, r1.Hits[1].Score, r1.Hits[2].Score)
	}
	for _, h := range r1.Hits {
		if len(h.Why) == 0 {
			t.Fatalf("hit %s has no explanation", h.Chunk.ID)
		}
		if len(h.MatchedTerms) == 0 {
			t.Fatalf("hit %s has no matched terms", h.Chunk.ID)
		}
	}
	t.Logf("JUDGMENT top hit c1 score=%d why=%v", r1.Hits[0].Score, r1.Hits[0].Why)
}

func TestBudgetDropsAreRecordedNotSilent(t *testing.T) {
	// 3 candidates each 100 tokens; chunk budget 2, token budget 150.
	snap := snapWith(
		ch("a", "a.go", "text", "", "alpha token", 1, 100),
		ch("b", "b.go", "text", "", "alpha token", 2, 100),
		ch("c", "c.go", "text", "", "alpha token", 3, 100),
	)
	e := retrieval.New(retrieval.Budget{MaxChunks: 2, MaxTokens: 150, MaxChunksScanned: 1000})
	r := e.Query(context.Background(), snap, "alpha")

	t.Logf("KEY INPUT 3 candidates x100 tokens, budgets chunks=2 tokens=150 -> "+
		"JUDGMENT returned=%d dropped=%d truncated=%q",
		len(r.Hits), len(r.Dropped), r.TruncatedBy)
	if len(r.Hits) != 1 || r.Hits[0].Chunk.ID != "a" {
		t.Fatalf("token budget should allow exactly the first chunk: %v", order(r))
	}
	if r.TruncatedBy != "tokens" {
		t.Fatalf("TruncatedBy = %q, want tokens", r.TruncatedBy)
	}
	reasons := map[string]string{}
	for _, d := range r.Dropped {
		reasons[d.ChunkID] = d.Reason
	}
	if reasons["b"] != "token_budget" || reasons["c"] != "token_budget" {
		t.Fatalf("drop reasons not recorded: %v", reasons)
	}
	if len(r.Hits)+len(r.Dropped) != r.TotalHits {
		t.Fatalf("accounting broken: hits+drops != total")
	}
}

func TestChunkBudgetCap(t *testing.T) {
	snap := snapWith(
		ch("a", "a.go", "text", "", "alpha", 1, 1),
		ch("b", "b.go", "text", "", "alpha", 2, 1),
	)
	e := retrieval.New(retrieval.Budget{MaxChunks: 1, MaxTokens: 1000, MaxChunksScanned: 1000})
	r := e.Query(context.Background(), snap, "alpha")
	t.Logf("KEY INPUT chunk budget 1 -> JUDGMENT returned=%d dropped=%v truncated=%q",
		len(r.Hits), r.Dropped, r.TruncatedBy)
	if len(r.Hits) != 1 || len(r.Dropped) != 1 || r.TruncatedBy != "chunks" {
		t.Fatalf("chunk budget mis-enforced: hits=%d drops=%v trunc=%s",
			len(r.Hits), r.Dropped, r.TruncatedBy)
	}
}

func TestEmptyAndNilQueries(t *testing.T) {
	snap := snapWith(ch("a", "a.go", "text", "", "alpha", 1, 1))
	e := retrieval.New(retrieval.Budget{})
	if r := e.Query(context.Background(), snap, "!!!  "); len(r.Hits) != 0 || len(r.Terms) != 0 {
		t.Fatalf("punctuation-only query must yield no terms/hits")
	}
	if r := e.Query(context.Background(), nil, "alpha"); len(r.Hits) != 0 {
		t.Fatalf("nil snapshot must yield no hits")
	}
}

func order(r retrieval.Result) []string {
	out := make([]string, len(r.Hits))
	for i, h := range r.Hits {
		out[i] = h.Chunk.ID
	}
	return out
}

func equalOrder(a, b retrieval.Result) bool {
	oa, ob := order(a), order(b)
	if len(oa) != len(ob) {
		return false
	}
	for i := range oa {
		if oa[i] != ob[i] {
			return false
		}
	}
	return true
}
