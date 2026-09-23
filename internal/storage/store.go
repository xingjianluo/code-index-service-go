// Package storage persists index snapshots to the local file system with
// atomic replacement, version gating, unknown-field rejection and a
// deterministic v0 -> v1 migration path.
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xingjianluo/code-index-service-go/internal/model"
)

// File names inside the store directory.
const (
	snapshotName = "snapshot.json"
	manifestName = "manifest.json"
	tmpSuffix    = ".tmp"
)

// ErrNotFound is returned when no snapshot exists at the store location.
var ErrNotFound = errors.New("index snapshot not found")

// ErrUnsupportedVersion is returned when persisted data carries a format or
// rule version that cannot be read or migrated. Silent downgrade never
// happens.
var ErrUnsupportedVersion = errors.New("unsupported index version")

// MigrationRequired wraps ErrUnsupportedVersion for cases whose documented
// resolution is a full rebuild from source (rule-version incompatibility).
type MigrationRequired struct {
	HaveFormat uint32
	HaveRule   uint32
	WantFormat uint32
	WantRule   uint32
}

func (m *MigrationRequired) Error() string {
	return fmt.Sprintf(
		"index built with format v%d/rules v%d cannot serve format v%d/rules v%d; "+
			"migration path: rebuild index from source (reindex)",
		m.HaveFormat, m.HaveRule, m.WantFormat, m.WantRule)
}

func (m *MigrationRequired) Unwrap() error { return ErrUnsupportedVersion }

// Store persists snapshots under a local directory.
type Store struct{ dir string }

// New constructs a Store rooted at dir, creating it on first save.
func New(dir string) *Store { return &Store{dir: dir} }

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// Save atomically writes snap (normalizing versions) plus its manifest.
// Readers concurrently calling Load always observe one complete snapshot.
func (s *Store) Save(snap *model.Snapshot) error {
	if snap == nil {
		return errors.New("cannot save nil snapshot")
	}
	snap.FormatVersion = model.FormatV1
	snap.RuleVersion = model.RulesV1
	if snap.Files == nil {
		snap.Files = map[string]*model.File{}
	}
	if snap.Failures == nil {
		snap.Failures = map[string]*model.Failure{}
	}
	if snap.Renames == nil {
		snap.Renames = map[string]string{}
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create index dir: %w", err)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWrite(filepath.Join(s.dir, snapshotName), data); err != nil {
		return err
	}
	man := model.Manifest{
		FormatVersion: snap.FormatVersion,
		RuleVersion:   snap.RuleVersion,
		Generation:    snap.Generation,
	}
	mdata, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	mdata = append(mdata, '\n')
	return atomicWrite(filepath.Join(s.dir, manifestName), mdata)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + tmpSuffix
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit %s: %w", filepath.Base(path), err)
	}
	return nil
}

// versionHeader probes which on-disk layout a snapshot uses without fully
// decoding it. v0 used "version" + top-level arrays; v1+ uses
// "format_version" + path-keyed maps.
type versionHeader struct {
	FormatVersion *uint32         `json:"format_version"`
	RuleVersion   *uint32         `json:"rule_version"`
	LegacyVersion *uint32         `json:"version"`
	Generation    json.RawMessage `json:"generation"`
	Files         json.RawMessage `json:"files"`
	Failures      json.RawMessage `json:"failures"`
	Renames       json.RawMessage `json:"renames"`
}

// Load reads the current snapshot, gating versions strictly:
//   - v0 snapshots are migrated to v1 via MigrateV0 (deterministic);
//   - unknown/higher format versions are rejected;
//   - rule-version mismatches are rejected with MigrationRequired;
//   - unknown JSON fields are rejected (no silent ignoring).
func (s *Store) Load() (*model.Snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, snapshotName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	return DecodeSnapshot(raw)
}

// DecodeSnapshot validates versioning and decodes raw snapshot bytes. It is
// exported so migrations and tests can exercise the exact load pipeline.
func DecodeSnapshot(raw []byte) (*model.Snapshot, error) {
	var hdr versionHeader
	dec := json.NewDecoder(bytesReader(raw))
	if err := dec.Decode(&hdr); err != nil {
		return nil, fmt.Errorf("decode snapshot header: %w", err)
	}

	if hdr.FormatVersion == nil {
		if hdr.LegacyVersion != nil && *hdr.LegacyVersion == 0 {
			return MigrateV0Bytes(raw)
		}
		return nil, fmt.Errorf("%w: missing format_version", ErrUnsupportedVersion)
	}

	switch fv := *hdr.FormatVersion; {
	case fv == model.FormatV1:
		return decodeV1(raw, hdr.RuleVersion)
	case fv < model.FormatV1:
		// Only v0 has a defined in-place migration; others must not appear.
		return nil, fmt.Errorf("%w: format v%d predates supported range",
			ErrUnsupportedVersion, fv)
	default:
		return nil, fmt.Errorf("%w: format v%d is newer than supported v%d",
			ErrUnsupportedVersion, fv, model.FormatV1)
	}
}

func decodeV1(raw []byte, ruleVersion *uint32) (*model.Snapshot, error) {
	if ruleVersion == nil {
		return nil, fmt.Errorf("%w: v1 snapshot missing rule_version",
			ErrUnsupportedVersion)
	}
	if rv := *ruleVersion; rv != model.RulesV1 {
		return nil, &MigrationRequired{
			HaveFormat: model.FormatV1, HaveRule: rv,
			WantFormat: model.FormatV1, WantRule: model.RulesV1,
		}
	}
	d := json.NewDecoder(bytesReader(raw))
	d.DisallowUnknownFields()
	var snap model.Snapshot
	if err := d.Decode(&snap); err != nil {
		return nil, fmt.Errorf("%w: decode v1 snapshot: %v", ErrUnsupportedVersion, err)
	}
	if snap.FormatVersion != model.FormatV1 || snap.RuleVersion != model.RulesV1 {
		return nil, fmt.Errorf("%w: header/body version mismatch", ErrUnsupportedVersion)
	}
	if snap.Files == nil {
		snap.Files = map[string]*model.File{}
	}
	if snap.Failures == nil {
		snap.Failures = map[string]*model.Failure{}
	}
	if snap.Renames == nil {
		snap.Renames = map[string]string{}
	}
	return &snap, nil
}

// Exists reports whether a snapshot has been saved.
func (s *Store) Exists() bool {
	_, err := os.Stat(filepath.Join(s.dir, snapshotName))
	return err == nil
}
