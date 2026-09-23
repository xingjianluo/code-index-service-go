package service_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xingjianluo/code-index-service-go/indexer"
	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/retrieval"
	"github.com/xingjianluo/code-index-service-go/service"
	"github.com/xingjianluo/code-index-service-go/source"
	"github.com/xingjianluo/code-index-service-go/store"
)

func setup(t *testing.T, files map[string]string, maxSize int64) (*service.Service, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	storeDir := filepath.Join(dir, "store")
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sc := source.NewScanner(source.Options{Root: root})
	pa := parser.New(parser.Options{MaxFileSize: maxSize})
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(indexer.New(sc, pa, st), st, retrieval.NewEngine())
	if err != nil {
		t.Fatal(err)
	}
	return svc, root
}

func write(root, p, c string) {
	full := filepath.Join(root, filepath.FromSlash(p))
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	_ = os.WriteFile(full, []byte(c), 0o644)
}

func TestConcurrentIndexAndSearchNeverMixed(t *testing.T) {
	svc, root := setup(t, map[string]string{
		"v1.go": "package main\n\nfunc VersionOneAlpha() string { return \"one alpha\" }\n",
	}, 1<<20)
	if rep, err := svc.Reindex(root); err != nil {
		t.Fatalf("initial index: %v", err)
	} else {
		t.Logf("initial snapshot=%s", rep.SnapshotID)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var seenStates sync.Map // snapshotID -> query term that was searchable in it

	// Reader hammer: every observed answer must be self-consistent: if the
	// result set references a term, the snapshot id and its contents must tell
	// one coherent story (we verify against published ids after the fact).
	reader := func(term string) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			out, err := svc.Search(term, retrieval.Budget{MaxResults: 50})
			if err != nil {
				t.Errorf("search %s: %v", term, err)
				return
			}
			seenStates.Store(out.SnapshotID, true)
			for _, r := range out.Results {
				// A result for this term must actually contain a chunk of a
				// file version that includes it; stale content means mixing.
				switch term {
				case "one":
					if r.Path == "v2.go" {
						t.Errorf("snapshot %s mixes old search 'one' with new file v2.go", out.SnapshotID)
					}
				case "two":
					if r.Path == "v1.go" {
						t.Errorf("snapshot %s mixes new search 'two' with stale file v1.go", out.SnapshotID)
					}
				}
			}
		}
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go reader("one")
		wg.Add(1)
		go reader("two")
	}

	// Writer: overwrite v1 away, add v2, then revert, repeatedly. Each state
	// is a valid complete snapshot of its own.
	var publishedIDs []string
	for round := 0; round < 6; round++ {
		if round%2 == 0 {
			write(root, "v2.go", fmt.Sprintf("package main\n\nfunc VersionTwoBeta() string { return \"two beta %d\" }\n", round))
			_ = os.Remove(filepath.Join(root, "v1.go"))
		} else {
			write(root, "v1.go", "package main\n\nfunc VersionOneAlpha() string { return \"one alpha\" }\n")
			_ = os.Remove(filepath.Join(root, "v2.go"))
		}
		rep, err := svc.Reindex(root)
		if err != nil {
			t.Fatalf("reindex round %d: %v", round, err)
		}
		publishedIDs = append(publishedIDs, rep.SnapshotID)
	}
	close(stop)
	wg.Wait()

	// Every snapshot id readers observed must be one the writer published.
	pub := map[string]bool{}
	for _, id := range publishedIDs {
		pub[id] = true
	}
	seenStates.Range(func(k, _ any) bool {
		if !pub[k.(string)] {
			t.Errorf("reader observed unpublished/hybrid snapshot id %s", k)
		} else {
			t.Logf("observed published snapshot %s", k)
		}
		return true
	})

	// After settling on a state that deleted v1, it must never be recallable.
	write(root, "v2.go", "package main\n\nfunc VersionTwoBeta() string { return \"final two\" }\n")
	_ = os.Remove(filepath.Join(root, "v1.go"))
	if _, err := svc.Reindex(root); err != nil {
		t.Fatal(err)
	}
	out, err := svc.Search("alpha", retrieval.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Results {
		if r.Path == "v1.go" {
			t.Fatalf("deleted v1.go content recalled: %+v", r)
		}
	}
	t.Logf("final search 'alpha' returned %d results (want 0)", len(out.Results))
}

func TestConcurrentReindexIsSerialized(t *testing.T) {
	svc, root := setup(t, map[string]string{"a.go": "package main\n"}, 1<<20)
	if _, err := svc.Reindex(root); err != nil {
		t.Fatal(err)
	}
	// Add enough files to widen the indexing window so released goroutines
	// reliably overlap rather than all completing serially by luck.
	for i := 0; i < 200; i++ {
		write(root, fmt.Sprintf("pkg/f%d.go", i),
			fmt.Sprintf("package pkg\n\nfunc F%d() int { return %d }\n", i, i))
	}
	totalRejected := 0
	for round := 0; round < 10; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		rejected := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := svc.Reindex(root)
				if err != nil {
					if !errors.Is(err, service.ErrIndexing) {
						t.Errorf("unexpected err: %v", err)
						return
					}
					mu.Lock()
					rejected++
					mu.Unlock()
				}
			}(i)
		}
		close(start)
		wg.Wait()
		totalRejected += rejected
		if rejected > 0 {
			break
		}
	}
	t.Logf("concurrent reindex calls rejected=%d (over rounds)", totalRejected)
	if totalRejected == 0 {
		t.Fatal("expected some concurrent reindex calls to be rejected with ErrIndexing")
	}
}

func TestSnapshotImmutabilityAndStableIDs(t *testing.T) {
	content := "package main\n\nfunc StableOne() int { return 1 }\n"
	svc, root := setup(t, map[string]string{"a.go": content}, 1<<20)
	r1, err := svc.Reindex(root)
	if err != nil {
		t.Fatal(err)
	}
	id1 := svc.CurrentSnapshotID()
	r2, err := svc.Reindex(root)
	if err != nil {
		t.Fatal(err)
	}
	id2 := svc.CurrentSnapshotID()
	t.Logf("ids %s == %s reports same=%v", id1, id2, r1.SnapshotID == r2.SnapshotID)
	if id1 != id2 {
		t.Fatalf("same source state must map to the same snapshot id")
	}
	var _ = model.Failure{} // keep model import if report types evolve
}
