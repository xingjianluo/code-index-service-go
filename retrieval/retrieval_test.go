package retrieval_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/retrieval"
)

func chunk(path string, start, end int, kind, title, content string, syms ...string) model.Chunk {
	ch := model.Chunk{
		Path: path, StartLine: start, EndLine: end, Kind: kind, Title: title,
		Content: content, Tokens: tokenize(content), SymbolIDs: syms,
		ContentHash: model.HashBytes([]byte(content)),
	}
	ch.ID = model.StableChunkID(path, start, end, "filehash", ch.ContentHash)
	return ch
}

func snap(chunks ...model.Chunk) *model.Snapshot {
	s := &model.Snapshot{Files: map[string]*model.FileRecord{}, Failures: []model.Failure{}}
	for _, c := range chunks {
		rec := s.Files[c.Path]
		if rec == nil {
			rec = &model.FileRecord{Path: c.Path, Symbols: []model.Symbol{}, References: []model.Reference{}}
			s.Files[c.Path] = rec
		}
		rec.Chunks = append(rec.Chunks, c)
	}
	s.ID = model.SnapshotID(s.Files, s.Failures)
	return s
}

func TestDeterministicRankingAndExplanation(t *testing.T) {
	s := snap(
		chunk("a.go", 1, 1, "declaration", "func Greeting", "func Greeting(name string) string { return Greeting(name) }"),
		chunk("b.go", 1, 1, "text", "", "the greeting word appears once here"),
	)
	eng := retrieval.NewEngine()
	o1, err := eng.Search(context.Background(), s, "Greeting", retrieval.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	o2, err := eng.Search(context.Background(), s, "greeting", retrieval.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(o1.Results) != 2 || o1.Results[0].Path != "a.go" {
		t.Fatalf("want declaration chunk ranked first, got %+v", paths(o1))
	}
	t.Logf("ranking: %v scores=[%.3f %.3f]", paths(o1), o1.Results[0].Score, o1.Results[1].Score)
	for i := range o1.Results {
		if o1.Results[i].ChunkID != o2.Results[i].ChunkID || o1.Results[i].Score != o2.Results[i].Score {
			t.Fatalf("query casing changed ranking/score: %v vs %v", o1.Results, o2.Results)
		}
	}
}

func paths(o *retrieval.Outcome) []string {
	var p []string
	for _, r := range o.Results {
		p = append(p, r.Path+":"+itoa(r.StartLine))
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestBudgetMaxResultsExplained(t *testing.T) {
	var chunks []model.Chunk
	for i := 0; i < 5; i++ {
		chunks = append(chunks, chunk("f.go", i+1, i+1, "declaration", "", "repeat needle needle"))
	}
	eng := retrieval.NewEngine()
	o, err := eng.Search(context.Background(), snap(chunks...), "needle", retrieval.Budget{MaxResults: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(o.Results))
	}
	if len(o.Truncated) != 3 {
		t.Fatalf("want 3 explained truncations, got %d: %+v", len(o.Truncated), o.Truncated)
	}
	for _, tr := range o.Truncated {
		if tr.Reason != "max_results" || tr.Detail == "" {
			t.Fatalf("truncation must carry reason+detail: %+v", tr)
		}
	}
	t.Logf("kept=%d truncations=%d first=%q", len(o.Results), len(o.Truncated), o.Truncated[0].Detail)
}

func TestBudgetCharsHardCeiling(t *testing.T) {
	big := strings.Repeat("needle line of content\n", 100)
	s := snap(
		chunk("a.go", 1, 200, "text", "", big),
		chunk("b.go", 1, 200, "text", "", big),
	)
	eng := retrieval.NewEngine()
	o, err := eng.Search(context.Background(), s, "needle", retrieval.Budget{
		MaxResults: 10, MaxTotalChars: 1200, MaxChunkChars: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, r := range o.Results {
		total += r.Chars
	}
	t.Logf("results=%d usedChars=%d truncReasons=%v", len(o.Results), total, reasons(o.Truncated))
	if total > 1200 {
		t.Fatalf("hard char ceiling breached: %d > 1200", total)
	}
	for _, r := range o.Results {
		if r.Chars > 1000 {
			t.Fatalf("per-chunk ceiling breached: %d", r.Chars)
		}
	}
	if len(o.Truncated) == 0 {
		t.Fatal("expected at least one explained char-budget truncation")
	}
}

func TestZeroAdmissionIsErrorNotSilence(t *testing.T) {
	big := strings.Repeat("needle ", 500) // ~3500 bytes
	s := snap(chunk("a.go", 1, 1, "text", "", big))
	eng := retrieval.NewEngine()
	_, err := eng.Search(context.Background(), s, "needle", retrieval.Budget{
		MaxResults: 10, MaxTotalChars: 10, MaxChunkChars: 10,
	})
	if !errors.Is(err, retrieval.ErrBudgetUnsatisfiable) {
		t.Fatalf("want ErrBudgetUnsatisfiable, got %v", err)
	}
	t.Logf("zero-admission correctly rejected: %v", err)
}

func TestNoMatchIsEmptyButExplained(t *testing.T) {
	s := snap(chunk("a.go", 1, 1, "text", "", "hello world"))
	o, err := retrieval.NewEngine().Search(context.Background(), s, "zzzznomatch", retrieval.Budget{})
	if err != nil {
		t.Fatalf("no-match should be a valid empty outcome, got %v", err)
	}
	if len(o.Results) != 0 {
		t.Fatalf("want no results, got %d", len(o.Results))
	}
	t.Logf("no-match outcome notes=%v", o.Notes)
}

func TestCandidateCapIsDeterministic(t *testing.T) {
	var chunks []model.Chunk
	for i := 0; i < 20; i++ {
		chunks = append(chunks, chunk("f.go", i+1, i+1, "text", "", "needle"))
	}
	eng := retrieval.NewEngine()
	o, err := eng.Search(context.Background(), snap(chunks...), "needle", retrieval.Budget{MaxCandidates: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Truncated) == 0 || o.Truncated[0].Reason != "candidate_cap" {
		t.Fatalf("expected candidate_cap truncation, got %+v", o.Truncated)
	}
	t.Logf("candidate cap: %q", o.Truncated[0].Detail)
}

func reasons(ts []retrieval.Truncation) []string {
	out := make([]string, len(ts))
	for i, t2 := range ts {
		out[i] = t2.Reason
	}
	return out
}
