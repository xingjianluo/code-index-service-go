// Command codesearch runs the local code index service against a directory.
//
// Subcommands:
//
//	codesearch index  -root DIR [-store DIR] [-max-size BYTES]
//	codesearch search -store DIR -query TEXT [-max-results N] [-max-chars N]
//	codesearch serve  -root DIR -store DIR [-addr :8080]
//
// Everything is local: the source is the local filesystem, the index is a
// single versioned JSON file, and serve only listens on localhost-bound
// endpoints. No external services or credentials are involved.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xingjianluo/code-index-service-go/indexer"
	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/relation"
	"github.com/xingjianluo/code-index-service-go/retrieval"
	"github.com/xingjianluo/code-index-service-go/service"
	"github.com/xingjianluo/code-index-service-go/source"
	"github.com/xingjianluo/code-index-service-go/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "index":
		err = runIndex(args)
	case "search":
		err = runSearch(args)
	case "refs":
		err = runRefs(args)
	case "impact":
		err = runImpact(args)
	case "serve":
		err = runServe(args)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown subcommand %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `codesearch - local repository index & context retrieval

usage:
  codesearch index  -root DIR [-store DIR] [-max-size BYTES]
  codesearch search -store DIR -query TEXT [-max-results N] [-max-chars N]
  codesearch refs   -store DIR -name SYMBOL [-max-refs N]
  codesearch impact -store DIR -files a.go,b.go [-max-impacted N]
  codesearch serve  -root DIR -store DIR [-addr 127.0.0.1:8080]
`)
}

func buildStack(root, storeDir string, maxSize int64) (*source.Scanner, *parser.Parser, *store.Store, error) {
	sc := source.NewScanner(source.Options{Root: root, MaxFileSize: maxSize})
	pa := parser.New(parser.Options{MaxFileSize: maxSize})
	st, err := store.New(storeDir)
	if err != nil {
		return nil, nil, nil, err
	}
	return sc, pa, st, nil
}

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root to index (local)")
	storeDir := fs.String("store", ".codesearch-index", "index storage directory")
	maxSize := fs.Int64("max-size", 2<<20, "per-file size limit in bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sc, pa, st, err := buildStack(*root, *storeDir, *maxSize)
	if err != nil {
		return err
	}
	svc, err := service.New(indexer.New(sc, pa, st), st, retrieval.NewEngine())
	if err != nil {
		return err
	}
	started := time.Now()
	report, err := svc.Reindex(*root)
	if err != nil {
		return err
	}
	printReport(report, svc.CurrentFailures(), time.Since(started))
	return nil
}

func printReport(r *model.IndexReport, failures []model.Failure, d time.Duration) {
	fmt.Printf("snapshot=%s root=%q\n", r.SnapshotID, r.Root)
	fmt.Printf("added=%d modified=%d deleted=%d renamed=%d unchanged=%d failed=%d (%s)\n",
		len(r.Added), len(r.Modified), len(r.Deleted), len(r.Renamed), len(r.Unchanged), len(r.Failed), d)
	for _, p := range r.Added {
		fmt.Println("  +", p)
	}
	for _, p := range r.Modified {
		fmt.Println("  ~", p)
	}
	for _, p := range r.Deleted {
		fmt.Println("  -", p)
	}
	for _, rn := range r.Renamed {
		fmt.Printf("  > rename %s -> %s\n", rn.OldPath, rn.NewPath)
	}
	for _, f := range failures {
		fmt.Printf("  ! [%s] %s: %s (size=%d)\n", f.Category, f.Path, f.Message, f.Size)
	}
}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	storeDir := fs.String("store", ".codesearch-index", "index storage directory")
	query := fs.String("query", "", "search query")
	maxResults := fs.Int("max-results", 10, "maximum results")
	maxChars := fs.Int("max-chars", 20000, "maximum assembled content characters")
	maxCandidates := fs.Int("max-candidates", 400, "maximum candidate chunks considered")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *query == "" {
		return fmt.Errorf("-query is required")
	}
	st, err := store.New(*storeDir)
	if err != nil {
		return err
	}
	snap, err := st.Load(context.Background())
	if err != nil {
		return err
	}
	outcome, err := retrieval.NewEngine().Search(context.Background(), snap, *query, retrieval.Budget{
		MaxResults: *maxResults, MaxTotalChars: *maxChars, MaxCandidates: *maxCandidates,
	})
	if err != nil {
		return err
	}
	for _, n := range outcome.Notes {
		fmt.Println("#", n)
	}
	for i, res := range outcome.Results {
		fmt.Printf("\n[%d] score=%.3f %s:%d-%d matched=%v\n%s\n",
			i+1, res.Score, res.Path, res.StartLine, res.EndLine, res.Matched, res.Content)
	}
	for _, t := range outcome.Truncated {
		fmt.Printf("~ truncation[%s] chunk=%s %.3f %s\n", t.Reason, t.ChunkID, t.Score, t.Detail)
	}
	fmt.Printf("\nreturned=%d snapshot=%s\n", len(outcome.Results), outcome.SnapshotID)
	return nil
}

func loadSnapshot(storeDir string) (*model.Snapshot, error) {
	st, err := store.New(storeDir)
	if err != nil {
		return nil, err
	}
	return st.Load(context.Background())
}

func runRefs(args []string) error {
	fs := flag.NewFlagSet("refs", flag.ContinueOnError)
	storeDir := fs.String("store", ".codesearch-index", "index storage directory")
	name := fs.String("name", "", "symbol name to find references of")
	maxRefs := fs.Int("max-refs", 500, "maximum reference occurrences per class")
	maxDefs := fs.Int("max-defs", 64, "maximum definitions considered")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	snap, err := loadSnapshot(*storeDir)
	if err != nil {
		return err
	}
	out, err := relation.NewEngine().References(context.Background(), snap, *name, relation.Limits{
		MaxReferences: *maxRefs, MaxDefinitions: *maxDefs,
	})
	if err != nil {
		return err
	}
	for _, n := range out.Notes {
		fmt.Println("#", n)
	}
	fmt.Printf("definitions of %q (%d):\n", out.Name, len(out.Definitions))
	for _, d := range out.Definitions {
		fmt.Printf("  def %s %s:%d kind=%s key=%s\n", d.Name, d.Path, d.Line, d.Kind, d.Key)
	}
	fmt.Printf("resolved references (%d):\n", len(out.Resolved))
	for _, h := range out.Resolved {
		fmt.Printf("  -> %s:%d:%d  => %s:%d key=%s\n", h.Path, h.Line, h.Column, h.TargetPath, h.TargetLine, h.TargetKey)
	}
	fmt.Printf("unresolved occurrences (%d):\n", len(out.Unresolved))
	for _, h := range out.Unresolved {
		fmt.Printf("  ?  %s:%d:%d\n", h.Path, h.Line, h.Column)
	}
	for _, t := range out.Truncated {
		fmt.Printf("~ truncation[%s] %s\n", t.Reason, t.Detail)
	}
	fmt.Printf("snapshot=%s\n", out.SnapshotID)
	return nil
}

func runImpact(args []string) error {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	storeDir := fs.String("store", ".codesearch-index", "index storage directory")
	files := fs.String("files", "", "comma-separated changed file paths (repo-relative)")
	maxImpacted := fs.Int("max-impacted", 200, "maximum impacted files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *files == "" {
		return fmt.Errorf("-files is required")
	}
	snap, err := loadSnapshot(*storeDir)
	if err != nil {
		return err
	}
	out, err := relation.NewEngine().Impact(context.Background(), snap, strings.Split(*files, ","), relation.Limits{
		MaxImpacted: *maxImpacted,
	})
	if err != nil {
		return err
	}
	for _, n := range out.Notes {
		fmt.Println("#", n)
	}
	fmt.Println("changed files:")
	for _, c := range out.Changed {
		fmt.Printf("  [%s] %s %s %s\n", c.Status, c.Path, c.Category, c.Detail)
	}
	fmt.Printf("impacted files (%d):\n", len(out.Impacted))
	for _, imp := range out.Impacted {
		fmt.Printf("  %s symbols=%v\n", imp.Path, imp.Symbols)
		for _, chain := range imp.Chains {
			for _, e := range chain {
				fmt.Printf("    %s: %s -> %s (%s)\n", e.Kind, e.From, e.To, e.Detail)
			}
		}
	}
	fmt.Printf("suspected (unresolved name matches) (%d):\n", len(out.Suspected))
	for _, imp := range out.Suspected {
		fmt.Printf("  %s symbols=%v\n", imp.Path, imp.Symbols)
	}
	for _, t := range out.Truncated {
		fmt.Printf("~ truncation[%s] %s\n", t.Reason, t.Detail)
	}
	fmt.Printf("snapshot=%s\n", out.SnapshotID)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root to index (local)")
	storeDir := fs.String("store", ".codesearch-index", "index storage directory")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address (localhost only)")
	maxSize := fs.Int64("max-size", 2<<20, "per-file size limit in bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sc, pa, st, err := buildStack(*root, *storeDir, *maxSize)
	if err != nil {
		return err
	}
	svc, err := service.New(indexer.New(sc, pa, st), st, retrieval.NewEngine())
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"snapshot":%q}`, svc.CurrentSnapshotID())
	})
	mux.HandleFunc("/reindex", func(w http.ResponseWriter, _ *http.Request) {
		report, err := svc.Reindex(*root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, report)
	})
	mux.HandleFunc("/refs", func(w http.ResponseWriter, r *http.Request) {
		out, err := svc.References(r.URL.Query().Get("name"), relation.Limits{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/impact", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("files")
		out, err := svc.Impact(strings.Split(q, ","), relation.Limits{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		outcome, err := svc.Search(q, retrieval.Budget{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, outcome)
	})
	log.Printf("codesearch serving %q on %s (store=%s)", *root, *addr, *storeDir)
	return http.ListenAndServe(*addr, mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
