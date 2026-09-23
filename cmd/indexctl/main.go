// Command indexctl is a small local CLI to index a repository and query it.
// It depends only on the local file system and the standard library.
//
// Usage:
//
//	indexctl -repo ./demo -index ./.index index
//	indexctl -repo ./demo -index ./.index status
//	indexctl -repo ./demo -index ./.index query "parse symbols"
//	indexctl -repo ./demo -index ./.index check
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/xingjianluo/code-index-service-go/internal/model"
	"github.com/xingjianluo/code-index-service-go/internal/retrieval"
	"github.com/xingjianluo/code-index-service-go/internal/service"
)

func main() {
	repo := flag.String("repo", ".", "repository root to index")
	indexDir := flag.String("index", ".index", "directory holding the index")
	maxChunks := flag.Int("max-chunks", retrieval.DefaultMaxChunks, "per-query chunk budget")
	maxTokens := flag.Int("max-tokens", retrieval.DefaultMaxTokens, "per-query token budget")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: indexctl -repo DIR -index DIR <index|status|query|check> [query text]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	svc, err := service.New(service.Config{
		RepoDir:  *repo,
		IndexDir: *indexDir,
		Budget: retrieval.Budget{
			MaxChunks: *maxChunks,
			MaxTokens: *maxTokens,
		},
	})
	if err != nil {
		fatal(err)
	}
	ctx := context.Background()

	// index/status/check load an existing snapshot when present.
	switch args[0] {
	case "index":
		runIndex(ctx, svc)
	case "status":
		runStatus(ctx, svc)
	case "query":
		if len(args) < 2 {
			fatal(fmt.Errorf("query requires a query string"))
		}
		runQuery(ctx, svc, joinArgs(args[1:]))
	case "check":
		runCheck(ctx, svc)
	default:
		fatal(fmt.Errorf("unknown subcommand %q", args[0]))
	}
}

func runIndex(ctx context.Context, svc *service.Service) {
	res, err := svc.Reindex(ctx)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("indexed: generation=%d added=%d updated=%d deleted=%d renamed=%d unchanged=%d failures=%d\n",
		svc.Snapshot().Generation,
		len(res.Added), len(res.Updated), len(res.Deleted),
		len(res.Renames), len(res.Unchanged), len(res.Failures))
	printList("added", res.Added)
	printList("updated", res.Updated)
	printList("deleted", res.Deleted)
	renamed := make([]string, 0, len(res.Renames))
	for newP, oldP := range res.Renames {
		renamed = append(renamed, oldP+" -> "+newP)
	}
	sort.Strings(renamed)
	printList("renamed", renamed)
	if len(res.Failures) > 0 {
		paths := make([]string, 0, len(res.Failures))
		for p := range res.Failures {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			f := svc.Snapshot().Failures[p]
			fmt.Printf("  failure %s: [%s] %s\n", p, f.Category, f.Reason)
		}
	}
}

func runStatus(ctx context.Context, svc *service.Service) {
	if err := svc.Load(ctx); err != nil {
		fatal(err)
	}
	snap := svc.Snapshot()
	fmt.Printf("format=v%d rules=v%d generation=%d files=%d failures=%d renames=%d\n",
		snap.FormatVersion, snap.RuleVersion, snap.Generation,
		len(snap.Files), len(snap.Failures), len(snap.Renames))
	paths := make([]string, 0, len(snap.Files))
	for p := range snap.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var chunks int
	for _, p := range paths {
		f := snap.Files[p]
		chunks += len(f.Chunks)
		fmt.Printf("  %s id=%s lang=%s symbols=%d refs=%d chunks=%d hash=%s\n",
			p, f.ID, f.Language, len(f.Symbols), len(f.Refs), len(f.Chunks), f.Hash[:12])
	}
	for _, p := range sortedFailPaths(snap) {
		x := snap.Failures[p]
		fmt.Printf("  %s FAILURE [%s] %s\n", p, x.Category, x.Reason)
	}
	fmt.Printf("total chunks=%d\n", chunks)
}

func runQuery(ctx context.Context, svc *service.Service, q string) {
	if err := svc.Load(ctx); err != nil {
		fatal(err)
	}
	res, err := svc.Search(ctx, q)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("query=%q terms=%v matching=%d returned=%d truncated_by=%q\n",
		q, res.Terms, res.TotalHits, len(res.Hits), res.TruncatedBy)
	for i, h := range res.Hits {
		fmt.Printf("  #%d score=%d %s:%d-%d (%s)\n",
			i+1, h.Score, h.Chunk.Path, h.Chunk.StartLine, h.Chunk.EndLine, h.Chunk.Kind)
		for _, w := range h.Why {
			fmt.Printf("      - %s\n", w)
		}
		fmt.Printf("      matched=%v tokens=%d\n", h.MatchedTerms, h.Chunk.Tokens)
	}
	for _, d := range res.Dropped {
		fmt.Printf("  DROPPED %s (%s) reason=%s\n", d.ChunkID, d.Path, d.Reason)
	}
}

// runCheck validates internal invariants of the published snapshot.
func runCheck(ctx context.Context, svc *service.Service) {
	if err := svc.Load(ctx); err != nil {
		fatal(err)
	}
	snap := svc.Snapshot()
	var problems []string
	for path, f := range snap.Files {
		if f.Path != path {
			problems = append(problems, fmt.Sprintf("map key %q != file.Path %q", path, f.Path))
		}
		if want := model.FileID(f.Hash); want != f.ID {
			problems = append(problems, fmt.Sprintf("%s: file id %s != content-derived %s", path, f.ID, want))
		}
		seenChunks := map[string]bool{}
		for _, c := range f.Chunks {
			if seenChunks[c.ID] {
				problems = append(problems, fmt.Sprintf("%s: duplicate chunk id %s", path, c.ID))
			}
			seenChunks[c.ID] = true
			want := model.ChunkID(f.Hash, c.Kind, c.SymbolName, c.StartLine, c.EndLine, c.Content)
			if want != c.ID {
				problems = append(problems, fmt.Sprintf("%s: chunk id %s != recomputed %s", path, c.ID, want))
			}
		}
	}
	for path, x := range snap.Failures {
		if x.Path != path {
			problems = append(problems, fmt.Sprintf("failure key %q != path %q", path, x.Path))
		}
		if model.FailureID(x.Path, x.Category) != x.ID {
			problems = append(problems, fmt.Sprintf("%s: unstable failure id", path))
		}
	}
	if len(problems) == 0 {
		fmt.Println("check OK: all IDs are content-derived and indexes consistent")
		return
	}
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "check PROBLEM: %s\n", p)
	}
	os.Exit(1)
}

func sortedFailPaths(snap *model.Snapshot) []string {
	out := make([]string, 0, len(snap.Failures))
	for p := range snap.Failures {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func printList(label string, items []string) {
	if len(items) == 0 {
		return
	}
	sort.Strings(items)
	for _, it := range items {
		fmt.Printf("  %s: %s\n", label, it)
	}
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "indexctl: %v\n", err)
	os.Exit(1)
}
