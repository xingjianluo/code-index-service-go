// Package store provides versioned, atomically-persisted snapshot storage.
//
// On-disk layout (all local, no external services):
//
//	<dir>/index.json.tmp  – staged write (fsync + rename target)
//	<dir>/index.json      – the single committed envelope
//
// The envelope carries explicit format/rule versions. Loading:
//   - unknown or too-new versions are REJECTED (no silent downgrade);
//   - unknown JSON fields are REJECTED (DisallowUnknownFields);
//   - supported older data is migrated along a fixed, documented path.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xingjianluo/code-index-service-go/model"
)

// DefaultFileName is the committed envelope file name.
const DefaultFileName = "index.json"

var (
	// ErrNotFound indicates no committed snapshot exists yet.
	ErrNotFound = errors.New("store: no committed snapshot")
	// ErrUnsupportedVersion is returned for an unknown/too-new version.
	ErrUnsupportedVersion = errors.New("store: unsupported version")
)

// Migrator re-normalizes a loaded older snapshot to the current rules.
// It is injected to avoid a store -> parser import cycle.
type Migrator func(*model.Snapshot) error

// Store persists snapshots on the local filesystem. A save is a single
// atomic temp-file + rename replacement; readers never observe a partial
// file.
type Store struct {
	dir  string
	file string
}

// New creates/opens a store rooted at dir, creating the directory if needed.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir %q: %w", dir, err)
	}
	return &Store{dir: dir, file: filepath.Join(dir, DefaultFileName)}, nil
}

// envelope is the persisted representation. Extra fields are impossible:
// Load rejects them via DisallowUnknownFields.
type envelope struct {
	ID          string                   `json:"id"`
	StoreFormat int                      `json:"store_format"`
	IndexRules  int                      `json:"index_rules"`
	Root        string                   `json:"root"`
	CreatedAt   string                   `json:"created_at"` // RFC3339Nano
	Files       map[string]*fileEnvelope `json:"files"`
	Failures    []model.Failure          `json:"failures"`
}

type fileEnvelope struct {
	Path        string            `json:"path"`
	Language    string            `json:"language"`
	Size        int64             `json:"size"`
	ContentHash string            `json:"content_hash"`
	Lines       int               `json:"lines"`
	Symbols     []model.Symbol    `json:"symbols"`
	References  []model.Reference `json:"references"`
	Chunks      []model.Chunk     `json:"chunks"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Save atomically persists a snapshot tagged with the current versions.
func (s *Store) Save(ctx context.Context, snap *model.Snapshot) error {
	if snap == nil {
		return errors.New("store: nil snapshot")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	env := toEnvelope(snap)
	env.StoreFormat = model.StoreFormatV2
	env.IndexRules = model.IndexRulesV2

	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}

	tmp := s.file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("store: open temp: %w", err)
	}
	writeOK := false
	defer func() {
		if !writeOK {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close temp: %w", err)
	}
	if err := os.Rename(tmp, s.file); err != nil {
		return fmt.Errorf("store: atomic rename: %w", err)
	}
	writeOK = true
	return nil
}

// Load reads and validates the committed snapshot.
func (s *Store) Load(ctx context.Context) (*model.Snapshot, error) {
	return s.LoadWithMigration(ctx, nil)
}

// LoadWithMigration loads, validates versions/unknown fields, then applies
// migrate to supported older snapshots to bring them to the current rules.
func (s *Store) LoadWithMigration(ctx context.Context, migrate Migrator) (*model.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: read: %w", err)
	}

	dec := json.NewDecoder(strictReader(data))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("store: decode envelope (unknown/corrupt field?): %w", err)
	}
	if dec.More() {
		return nil, errors.New("store: multiple JSON values in envelope")
	}

	if env.StoreFormat < model.MinSupportedStoreFormat || env.StoreFormat > model.StoreFormatV2 {
		return nil, fmt.Errorf("%w: store_format=%d (supported %d..%d)",
			ErrUnsupportedVersion, env.StoreFormat, model.MinSupportedStoreFormat, model.StoreFormatV2)
	}
	if env.IndexRules < model.MinSupportedIndexRules || env.IndexRules > model.IndexRulesV2 {
		return nil, fmt.Errorf("%w: index_rules=%d (supported %d..%d)",
			ErrUnsupportedVersion, env.IndexRules, model.MinSupportedIndexRules, model.IndexRulesV2)
	}

	snap := fromEnvelope(&env)
	migrated := env.StoreFormat == 1 || env.IndexRules == 1
	// Fixed migration path: v1 -> v2. A v1 store had index_rules=1 and chunks
	// tokenized under v1 rules; the migrator re-tokenizes them with v2 and the
	// envelope is rewritten on the next Save.
	if migrated {
		if migrate == nil {
			return nil, fmt.Errorf("%w: v1 data requires a migrator but none configured", ErrUnsupportedVersion)
		}
		if err := migrate(snap); err != nil {
			return nil, fmt.Errorf("store: migrate v1->v2: %w", err)
		}
		snap.StoreFormat = model.StoreFormatV2
		snap.IndexRules = model.IndexRulesV2
	}
	// Integrity check: the stored ID must equal the deterministic identity
	// derived from files and failures. v1 carries no id field, so it is simply
	// assigned the recomputed identity; any v2 mismatch means corruption or a
	// broken migration and is rejected rather than silently trusted.
	want := model.SnapshotID(snap.Files, snap.Failures)
	switch {
	case migrated:
		snap.ID = want
	case snap.ID != want:
		return nil, fmt.Errorf("store: snapshot id mismatch: stored=%q computed=%q (corrupt index?)", snap.ID, want)
	}
	return snap, nil
}

// Path reports the on-disk directory (useful for local reproduction).
func (s *Store) Path() string { return s.dir }

// File reports the committed envelope path.
func (s *Store) File() string { return s.file }
