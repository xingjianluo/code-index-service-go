// Package service orchestrates indexing and retrieval with a consistent
// snapshot visibility model under concurrency.
//
// Visibility guarantee: a *model.Snapshot is immutable. The service holds an
// atomic.Value to the "current" snapshot and swaps it only after a full index
// pass has been committed to disk. Therefore:
//   - search readers observe either the previous complete snapshot or the new
//     complete snapshot, never a half-updated or mixed state;
//   - an in-flight search keeps reading the snapshot it started on even while
//     a reindex runs;
//   - indexing passes are serialized with an atomic gate (a concurrent pass
//     is rejected with ErrIndexing rather than interleaving).
package service

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/xingjianluo/code-index-service-go/indexer"
	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/retrieval"
	"github.com/xingjianluo/code-index-service-go/store"
)

// ErrIndexing indicates an indexing pass is already running.
var ErrIndexing = errors.New("service: indexing already in progress")

// ErrNoSnapshot indicates no index has been published yet.
var ErrNoSnapshot = errors.New("service: no indexed snapshot available")

// Service coordinates concurrent index writes and search reads.
type Service struct {
	idx      *indexer.Indexer
	st       *store.Store
	engine   *retrieval.Engine
	current  atomic.Pointer[model.Snapshot]
	indexing atomic.Bool
}

// New creates a Service and loads the last committed snapshot if present.
func New(idx *indexer.Indexer, st *store.Store, engine *retrieval.Engine) (*Service, error) {
	s := &Service{idx: idx, st: st, engine: engine}
	snap, err := st.Load(context.Background())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return s, nil // first run: nothing published yet
		}
		return nil, err
	}
	s.current.Store(snap)
	return s, nil
}

// Reindex runs an incremental indexing pass and atomically publishes it.
// Concurrent invocations are rejected with ErrIndexing.
func (s *Service) Reindex(root string) (*model.IndexReport, error) {
	if !s.indexing.CompareAndSwap(false, true) {
		return nil, ErrIndexing
	}
	defer s.indexing.Store(false)

	snap, report, err := s.idx.Index(context.Background(), root)
	if err != nil {
		return nil, err
	}
	// Publish only after the durable commit inside Index has succeeded.
	s.current.Store(snap)
	return report, nil
}

// Search serves a query against the currently published snapshot. The
// snapshot pointer is captured once, guaranteeing a consistent read even if a
// reindex commits mid-search.
func (s *Service) Search(query string, b retrieval.Budget) (*retrieval.Outcome, error) {
	snap := s.current.Load()
	if snap == nil {
		return nil, ErrNoSnapshot
	}
	return s.engine.Search(context.Background(), snap, query, b)
}

// CurrentSnapshotID exposes the active snapshot identifier.
func (s *Service) CurrentSnapshotID() string {
	if snap := s.current.Load(); snap != nil {
		return snap.ID
	}
	return ""
}

// CurrentFailures returns the typed failures recorded in the active snapshot.
func (s *Service) CurrentFailures() []model.Failure {
	if snap := s.current.Load(); snap != nil {
		out := make([]model.Failure, len(snap.Failures))
		copy(out, snap.Failures)
		return out
	}
	return nil
}
