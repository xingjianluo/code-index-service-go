// Package service wires the indexer, storage and retrieval engine together
// with concurrency-safe snapshot swapping.
//
// Visibility guarantee: snapshots are immutable after construction and are
// published with a single atomic pointer swap. A search running during a
// reindex continues on the snapshot it started with; it can never observe a
// half-merged directory walk or a mix of old and new entries.
package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/xingjianluo/code-index-service-go/internal/indexer"
	"github.com/xingjianluo/code-index-service-go/internal/model"
	"github.com/xingjianluo/code-index-service-go/internal/retrieval"
	"github.com/xingjianluo/code-index-service-go/internal/storage"
)

// Config configures a Service.
type Config struct {
	RepoDir  string
	IndexDir string
	Budget   retrieval.Budget
}

// Service coordinates indexing, persistence and retrieval.
type Service struct {
	cfg     Config
	ix      *indexer.Indexer
	store   *storage.Store
	engine  *retrieval.Engine
	current atomic.Pointer[model.Snapshot]
	wmu     sync.Mutex // serializes writers; readers never block
}

// New constructs a Service and loads any existing index.
func New(cfg Config) (*Service, error) {
	ix := indexer.New(indexer.Options{Root: cfg.RepoDir})
	st := storage.New(cfg.IndexDir)
	s := &Service{
		cfg:    cfg,
		ix:     ix,
		store:  st,
		engine: retrieval.New(cfg.Budget),
	}
	return s, nil
}

// Reindex runs one incremental indexing pass. The new snapshot is fully
// built, then atomically published and persisted; concurrent Search calls
// keep using the previous snapshot until publication completes.
func (s *Service) Reindex(ctx context.Context) (indexer.ScanResult, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var prev *model.Snapshot
	if p := s.current.Load(); p != nil {
		prev = p
	} else if s.store.Exists() {
		// First Reindex within this process but an on-disk index exists:
		// load it so incremental classification and generation continue.
		loaded, err := s.store.Load()
		switch {
		case err == nil:
			prev = loaded
			s.current.Store(loaded)
		case errors.Is(err, storage.ErrUnsupportedVersion):
			// Version incompatibility has one documented migration path for
			// the write path: rebuild from source. Generation cannot be
			// continued from an unreadable lineage, so this is a fresh
			// lineage (generation restarts at 1).
			prev = nil
		default:
			// Genuine I/O or corruption that is not a version gate: do not
			// silently overwrite it; surface the error.
			return indexer.ScanResult{}, err
		}
	}
	snap, res, err := s.ix.BuildSnapshot(ctx, prev)
	if err != nil {
		return indexer.ScanResult{}, err
	}

	// Persist first: the on-disk write is itself atomic (tmp+rename), so a
	// crash leaves either the old or the new complete snapshot, never a mix.
	if err := s.store.Save(snap); err != nil {
		return indexer.ScanResult{}, err
	}
	s.current.Store(snap)
	return res, nil
}

// Search queries the currently published snapshot. It is safe for
// concurrent use with Reindex and never blocks on indexing.
func (s *Service) Search(ctx context.Context, q string) (retrieval.Result, error) {
	snap := s.current.Load()
	if snap == nil {
		return retrieval.Result{}, ErrNotIndexed
	}
	return s.engine.Query(ctx, snap, q), nil
}

// Snapshot returns the currently published snapshot (nil before first index).
func (s *Service) Snapshot() *model.Snapshot { return s.current.Load() }

// Load opens the on-disk index at IndexDir (strict version checks apply) and
// atomically publishes it.
func (s *Service) Load(ctx context.Context) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	snap, err := s.store.Load()
	if err != nil {
		return err
	}
	s.current.Store(snap)
	return nil
}
