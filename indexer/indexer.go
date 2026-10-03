// Package indexer performs incremental indexing from a local source tree.
//
// Incremental semantics:
//   - Files are keyed by repository-relative path; content is keyed by the
//     SHA-256 of raw bytes.
//   - Same path + same hash  -> unchanged, record reused verbatim (every
//     derived symbol/chunk identity stays stable).
//   - Same path, new hash    -> modified; old chunks are replaced, never
//     recalled.
//   - Disappeared path       -> deleted from the new snapshot.
//   - New path with a hash that an old deleted path had -> rename (reported,
//     content identities preserved).
//   - Unreadable/invalid/oversize/unparseable inputs become typed failures.
//
// The committed result is a brand-new immutable Snapshot; publication happens
// atomically in the service layer.
package indexer

import (
	"context"
	"sort"
	"time"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/parser"
	"github.com/xingjianluo/code-index-service-go/source"
	"github.com/xingjianluo/code-index-service-go/store"
)

// Indexer builds snapshots incrementally on top of the previous snapshot.
type Indexer struct {
	scanner *source.Scanner
	parser  *parser.Parser
	store   *store.Store
}

// New creates an Indexer.
func New(sc *source.Scanner, pa *parser.Parser, st *store.Store) *Indexer {
	return &Indexer{scanner: sc, parser: pa, store: st}
}

// Index runs one incremental pass and atomically persists the new snapshot.
// Re-indexing an unchanged tree yields an equal snapshot ID and identical
// records, regardless of how many times it is repeated.
func (i *Indexer) Index(ctx context.Context, root string) (*model.Snapshot, *model.IndexReport, error) {
	previous, err := i.store.LoadWithMigration(ctx, parser.MigrateToCurrent)
	if err != nil && err != store.ErrNotFound {
		return nil, nil, err
	}
	return i.indexWith(ctx, root, previous)
}

// indexWith is the incremental core (previous may be nil).
func (i *Indexer) indexWith(ctx context.Context, root string, previous *model.Snapshot) (*model.Snapshot, *model.IndexReport, error) {
	discovered, excluded, err := i.scanner.ScanDetailed()
	if err != nil {
		return nil, nil, err
	}

	var oldFiles map[string]*model.FileRecord
	if previous != nil {
		oldFiles = previous.Files
	}
	oldHashPaths := mapHashToPaths(oldFiles)

	report := &model.IndexReport{
		Root:      root,
		Added:     []string{},
		Modified:  []string{},
		Deleted:   []string{},
		Renamed:   []model.Rename{},
		Unchanged: []string{},
		Failed:    []model.Failure{},
	}

	// First pass: parse every candidate (re-read + hash from raw bytes; the
	// hash is the sole authority for "unchanged", so mtime lies cannot fool
	// the index). Collect fresh content-hash -> new paths.
	type parsed struct {
		rec  *model.FileRecord
		fail *model.Failure
	}
	parsedList := make([]parsed, 0, len(discovered))
	var failures []model.Failure
	newHashPaths := make(map[string][]string)
	// Files outside the indexed extension set are accounted for as typed
	// "excluded" failures: they carry no symbols/references, but they must
	// not vanish from reports, impact analysis or the snapshot identity.
	for _, ex := range excluded {
		failures = append(failures, model.Failure{
			Path:     ex.RelPath,
			Category: model.FailureExcluded,
			Message:  ex.Detail,
			Size:     ex.Size,
		})
	}
	for _, df := range discovered {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rec, fail := i.parser.ParseFile(ctx, df)
		parsedList = append(parsedList, parsed{rec: rec, fail: fail})
		if fail != nil {
			failures = append(failures, *fail)
			continue
		}
		newHashPaths[rec.ContentHash] = append(newHashPaths[rec.ContentHash], rec.Path)
	}

	// Pure rename: identical bytes occur at exactly one old and one new path,
	// the old path is gone, and the new path did not exist before. Requiring
	// 1:1 avoids ambiguous classifications when duplicates exist.
	renameOld := make(map[string]bool)
	renameNew := make(map[string]bool)
	for hash, newPaths := range newHashPaths {
		oldPaths := oldHashPaths[hash]
		if len(newPaths) != 1 || len(oldPaths) != 1 {
			continue
		}
		np, op := newPaths[0], oldPaths[0]
		if np == op {
			continue
		}
		if oldFiles[np] != nil {
			continue // destination path already existed (content also present elsewhere)
		}
		renameOld[op] = true
		renameNew[np] = true
		report.Renamed = append(report.Renamed, model.Rename{OldPath: op, NewPath: np})
	}

	// Second pass: assemble the new file map and classify each path.
	seen := make(map[string]bool, len(parsedList))
	newFiles := make(map[string]*model.FileRecord, len(parsedList))
	for _, pr := range parsedList {
		if pr.fail != nil {
			continue
		}
		rec := pr.rec
		seen[rec.Path] = true
		old := oldFiles[rec.Path]
		newFiles[rec.Path] = rec
		switch {
		case renameNew[rec.Path]:
			// pure rename; counted in report.Renamed only
		case old == nil:
			report.Added = append(report.Added, rec.Path)
		case old.ContentHash == rec.ContentHash:
			// Identical content: preserve every derived identity byte-for-byte,
			// but never share the record pointer with the previous snapshot:
			// reference resolution below mutates the new snapshot's copy, so
			// the old immutable snapshot (possibly mid-search) stays untouched.
			newFiles[rec.Path] = cloneRecord(old)
			report.Unchanged = append(report.Unchanged, rec.Path)
		default:
			report.Modified = append(report.Modified, rec.Path)
		}
	}

	// Deletions: old paths absent now, excluding rename sources.
	for path := range oldFiles {
		if !seen[path] && !renameOld[path] {
			report.Deleted = append(report.Deleted, path)
		}
	}

	// Resolve cross-file references over the complete new set. Resolution is
	// deterministic and order-independent, so doing it for all files is safe.
	i.parser.ResolveReferences(recordsOf(newFiles))

	sortFailures(failures)
	snap := &model.Snapshot{
		StoreFormat: model.StoreFormatV2,
		IndexRules:  model.IndexRulesV3,
		Files:       newFiles,
		Failures:    failures,
		CreatedAt:   time.Now().UTC(),
		Root:        root,
	}
	snap.ID = model.SnapshotID(snap.Files, snap.Failures)
	report.SnapshotID = snap.ID
	report.Failed = failures
	sortReport(report)

	if err := i.store.Save(ctx, snap); err != nil {
		return nil, nil, err
	}
	return snap, report, nil
}

// cloneRecord returns a snapshot-private copy of rec. Slice contents are
// immutable values (symbols/chunks are never rewritten), so a shallow copy of
// the backing arrays suffices; the important effect is that the new snapshot
// owns its record struct and its References slice header.
func cloneRecord(rec *model.FileRecord) *model.FileRecord {
	cp := *rec
	cp.Symbols = append([]model.Symbol(nil), rec.Symbols...)
	cp.References = append([]model.Reference(nil), rec.References...)
	cp.Chunks = append([]model.Chunk(nil), rec.Chunks...)
	if rec.Metadata != nil {
		cp.Metadata = make(map[string]string, len(rec.Metadata))
		for k, v := range rec.Metadata {
			cp.Metadata[k] = v
		}
	}
	return &cp
}

func mapHashToPaths(files map[string]*model.FileRecord) map[string][]string {
	m := make(map[string][]string)
	for path, rec := range files {
		m[rec.ContentHash] = append(m[rec.ContentHash], path)
	}
	for h := range m {
		sort.Strings(m[h])
	}
	return m
}

func recordsOf(files map[string]*model.FileRecord) []*model.FileRecord {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]*model.FileRecord, 0, len(paths))
	for _, p := range paths {
		out = append(out, files[p])
	}
	return out
}

func sortFailures(fs []model.Failure) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Path != fs[j].Path {
			return fs[i].Path < fs[j].Path
		}
		return fs[i].Category < fs[j].Category
	})
}

func sortReport(r *model.IndexReport) {
	sort.Strings(r.Added)
	sort.Strings(r.Modified)
	sort.Strings(r.Deleted)
	sort.Strings(r.Unchanged)
	sort.Slice(r.Renamed, func(i, j int) bool {
		if r.Renamed[i].OldPath != r.Renamed[j].OldPath {
			return r.Renamed[i].OldPath < r.Renamed[j].OldPath
		}
		return r.Renamed[i].NewPath < r.Renamed[j].NewPath
	})
}
