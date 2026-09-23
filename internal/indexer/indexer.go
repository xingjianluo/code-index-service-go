// Package indexer performs repository walks and incremental indexing,
// producing immutable model.Snapshot values.
//
// Incremental semantics:
//   - entries are compared by content hash; unchanged entries keep the exact
//     same *model.File / IDs between snapshots;
//   - added / updated / deleted / renamed paths are classified against the
//     previous snapshot;
//   - the new snapshot is fully constructed before it is ever returned, so
//     a reader cannot observe a half-merged or mixed-new/old state;
//   - indexing the same tree state twice produces byte-identical results.
package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/xingjianluo/code-index-service-go/internal/model"
	"github.com/xingjianluo/code-index-service-go/internal/parser"
)

// DefaultSkipDirs are directory names never traversed.
var DefaultSkipDirs = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
}

// Options configures the indexer.
type Options struct {
	Root          string
	ParserOptions parser.Options
	// SkipDirNames are directory names skipped during the walk.
	// nil selects DefaultSkipDirs.
	SkipDirNames map[string]bool
}

// Indexer walks a local repository root and builds snapshots.
type Indexer struct {
	opts     Options
	parser   *parser.Parser
	skipDirs map[string]bool
}

// New constructs an Indexer.
func New(opts Options) *Indexer {
	skip := opts.SkipDirNames
	if skip == nil {
		skip = DefaultSkipDirs
	}
	return &Indexer{
		opts:     opts,
		parser:   parser.New(opts.ParserOptions),
		skipDirs: skip,
	}
}

// ScanResult reports what an indexing run did, for explainability/logs.
type ScanResult struct {
	Added     []string
	Updated   []string
	Deleted   []string
	Renames   map[string]string
	Unchanged []string
	Failures  map[string]model.FailureCategory
	// FailureChanged lists paths whose failure record was added, removed or
	// altered this run. Persisting unchanged failures does not appear here.
	FailureChanged []string
}

// BuildSnapshot performs a full walk and builds a fresh snapshot. When prev
// is non-nil it is used to classify adds/updates/deletes and detect renames;
// unchanged files are carried over verbatim (stable IDs). The returned
// snapshot is complete and self-contained, or an error is returned.
func (ix *Indexer) BuildSnapshot(ctx context.Context, prev *model.Snapshot) (*model.Snapshot, ScanResult, error) {
	if ix.opts.Root == "" {
		return nil, ScanResult{}, fmt.Errorf("indexer: empty root directory")
	}
	res := ScanResult{
		Renames:  map[string]string{},
		Failures: map[string]model.FailureCategory{},
	}

	paths, err := ix.walk(ctx)
	if err != nil {
		return nil, ScanResult{}, err
	}

	snap := &model.Snapshot{
		FormatVersion: model.FormatV1,
		RuleVersion:   model.RulesV1,
		Files:         map[string]*model.File{},
		Failures:      map[string]*model.Failure{},
		Renames:       map[string]string{},
	}

	// Phase 1: parse everything into a local staging map. Nothing is visible
	// to readers until the whole map is finished.
	type parsed struct {
		file    *model.File
		failure *model.Failure
	}
	staged := make(map[string]parsed, len(paths))
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return nil, ScanResult{}, err
		}
		abs := filepath.Join(ix.opts.Root, rel)
		out := ix.parser.ParseFile(abs, rel)
		staged[rel] = parsed{file: out.File, failure: out.Failure}
	}

	// Phase 2: classify against prev and populate the new snapshot.
	// First pass on successfully parsed files, to enable rename detection.
	currentHashes := map[string]string{} // path -> hash (successful files)
	for rel, p := range staged {
		if p.file != nil {
			currentHashes[rel] = p.file.Hash
		}
	}
	renames := detectRenames(prev, currentHashes)

	prevFiles := map[string]*model.File{}
	prevFailures := map[string]*model.Failure{}
	if prev != nil {
		prevFiles = prev.Files
		prevFailures = prev.Failures
	}

	for _, rel := range paths {
		p := staged[rel]
		switch {
		case p.file != nil:
			if old, ok := prevFiles[rel]; ok {
				if old.Hash == p.file.Hash {
					snap.Files[rel] = old // byte-identical: stable identity
					res.Unchanged = append(res.Unchanged, rel)
				} else {
					snap.Files[rel] = p.file
					res.Updated = append(res.Updated, rel)
				}
			} else if oldPath, renamed := renames[rel]; renamed {
				snap.Files[rel] = p.file
				res.Renames[rel] = oldPath
				snap.Renames[rel] = oldPath
			} else {
				snap.Files[rel] = p.file
				if _, wasFailure := prevFailures[rel]; wasFailure {
					// Repaired: failed path became a healthy file; retrieval
					// gains its chunks, so this is a visible update.
					res.Updated = append(res.Updated, rel)
					res.FailureChanged = append(res.FailureChanged, rel)
				} else {
					res.Added = append(res.Added, rel)
				}
			}
		case p.failure != nil:
			if old, ok := prevFailures[rel]; ok &&
				old.Category == p.failure.Category && old.Reason == p.failure.Reason {
				snap.Failures[rel] = old // failure record is itself stable
			} else {
				snap.Failures[rel] = p.failure
				res.FailureChanged = append(res.FailureChanged, rel)
			}
			res.Failures[rel] = p.failure.Category
			// Was this path a healthy file before? It counts as an update,
			// because retrieval over the new snapshot must drop its chunks.
			if _, wasFile := prevFiles[rel]; wasFile {
				res.Updated = append(res.Updated, rel)
			}
		}
	}

	// Phase 3: deletions = previously known paths absent now (rename sources
	// are reported as renames, not plain deletions).
	currentAll := map[string]bool{}
	for _, rel := range paths {
		currentAll[rel] = true
	}
	renameSources := map[string]bool{}
	for _, src := range renames {
		renameSources[src] = true
	}
	if prev != nil {
		for rel := range prevFiles {
			if currentAll[rel] || renameSources[rel] {
				continue
			}
			res.Deleted = append(res.Deleted, rel)
		}
		for rel := range prevFailures {
			if currentAll[rel] {
				continue
			}
			res.Deleted = append(res.Deleted, rel)
		}
	}

	// Generation bumps iff any visible content changed.
	sort.Strings(res.Added)
	sort.Strings(res.Updated)
	sort.Strings(res.Deleted)
	sort.Strings(res.Unchanged)
	sort.Strings(res.FailureChanged)
	snap.Generation = nextGeneration(prev, res)

	return snap, res, nil
}

// walk returns repo-relative, slash-free OS paths in sorted order.
func (ix *Indexer) walk(ctx context.Context) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(ix.opts.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Root or entry unreadable: surface as error; the caller decides.
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			if path != ix.opts.Root && ix.skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// Symlinks are not followed; parser reports them as unreadable.
			paths = append(paths, relPath(ix.opts.Root, path))
			return nil
		}
		if !d.Type().IsRegular() {
			paths = append(paths, relPath(ix.opts.Root, path))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		paths = append(paths, relPath(ix.opts.Root, path))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// detectRenames matches removed old paths to added new paths by identical
// content hash. Matching is deterministic: both sides are sorted, and each
// old path claims at most one new path (first hash match in sorted order).
func detectRenames(prev *model.Snapshot, currentHashes map[string]string) map[string]string {
	renames := map[string]string{}
	if prev == nil {
		return renames
	}
	// New paths not present in the previous snapshot, grouped by hash.
	newByHash := map[string][]string{}
	for path, h := range currentHashes {
		if _, existed := prev.Files[path]; !existed {
			newByHash[h] = append(newByHash[h], path)
		}
	}
	for h := range newByHash {
		sort.Strings(newByHash[h])
	}
	// Removed old paths with a hash present among the new ones.
	var removed []string
	for path, f := range prev.Files {
		if _, stillHere := currentHashes[path]; !stillHere {
			if _, hasNew := newByHash[f.Hash]; hasNew {
				removed = append(removed, path)
			}
		}
	}
	sort.Strings(removed)
	claimed := map[string]bool{}
	for _, oldPath := range removed {
		h := prev.Files[oldPath].Hash
		for _, newPath := range newByHash[h] {
			if !claimed[newPath] {
				renames[newPath] = oldPath
				claimed[newPath] = true
				break
			}
		}
	}
	return renames
}

func nextGeneration(prev *model.Snapshot, res ScanResult) int64 {
	changed := len(res.Added)+len(res.Updated)+len(res.Deleted)+len(res.Renames)+len(res.FailureChanged) > 0
	if prev == nil {
		// First scan: generation 1 even when the tree only contains failing files.
		return 1
	}
	if changed {
		return prev.Generation + 1
	}
	return prev.Generation
}
