package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/xingjianluo/code-index-service-go/internal/model"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// legacyV0 mirrors the pre-versioned on-disk layout:
//
//	{
//	  "version": 0,
//	  "generation": N,
//	  "files": [ <model.File> ... ],
//	  "failures": [ {path, category, reason} ... ]
//	}
//
// The v0 layout had no rule version and stored entries as arrays. Migration
// re-keys by path and stamps format/rules v1. IDs embedded in v0 entries are
// preserved when present (hashing scheme is unchanged since v0); missing IDs
// are recomputed deterministically.
type legacyV0 struct {
	Version    uint32            `json:"version"`
	Generation int64             `json:"generation"`
	Files      []legacyV0File    `json:"files"`
	Failures   []legacyV0Failure `json:"failures"`
}

type legacyV0File struct {
	// Explicit v0 surface: any unknown field rejects the migration.
	ID       string            `json:"id"`
	Path     string            `json:"path"`
	Language model.Language    `json:"language"`
	Size     int64             `json:"size"`
	Hash     string            `json:"hash"`
	Lines    int               `json:"lines"`
	Symbols  []model.Symbol    `json:"symbols"`
	Refs     []model.Reference `json:"refs"`
	Chunks   []model.Chunk     `json:"chunks"`
}

type legacyV0Failure struct {
	Path     string                `json:"path"`
	Category model.FailureCategory `json:"category"`
	Reason   string                `json:"reason"`
	SizeHint int64                 `json:"size_hint,omitempty"`
}

// MigrateV0Bytes strictly decodes v0 bytes and returns the v1 snapshot.
// It is exported so the migration semantics are independently testable.
func MigrateV0Bytes(raw []byte) (*model.Snapshot, error) {
	var old legacyV0
	d := json.NewDecoder(bytesReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&old); err != nil {
		return nil, fmt.Errorf("%w: invalid v0 payload: %v",
			ErrUnsupportedVersion, err)
	}
	if old.Version != 0 {
		return nil, fmt.Errorf("%w: not a v0 payload (version=%d)",
			ErrUnsupportedVersion, old.Version)
	}
	if err := d.Decode(&struct{}{}); err != nil && err.Error() != "EOF" {
		return nil, fmt.Errorf("%w: trailing data in v0 payload: %v",
			ErrUnsupportedVersion, err)
	}

	snap := &model.Snapshot{
		FormatVersion: model.FormatV1,
		RuleVersion:   model.RulesV1,
		Generation:    old.Generation,
		Files:         map[string]*model.File{},
		Failures:      map[string]*model.Failure{},
		Renames:       map[string]string{},
	}

	for i := range old.Files {
		of := &old.Files[i]
		if of.Path == "" {
			return nil, fmt.Errorf("%w: v0 file entry %d has empty path",
				ErrUnsupportedVersion, i)
		}
		if _, dup := snap.Files[of.Path]; dup {
			return nil, fmt.Errorf("%w: duplicate path %q in v0 payload",
				ErrUnsupportedVersion, of.Path)
		}
		if of.ID == "" && of.Hash != "" {
			of.ID = model.FileID(of.Hash)
		}
		f := &model.File{
			ID:       of.ID,
			Path:     of.Path,
			Language: of.Language,
			Size:     of.Size,
			Hash:     of.Hash,
			Lines:    of.Lines,
			Symbols:  of.Symbols,
			Refs:     of.Refs,
			Chunks:   of.Chunks,
		}
		if f.Symbols == nil {
			f.Symbols = []model.Symbol{}
		}
		if f.Refs == nil {
			f.Refs = []model.Reference{}
		}
		if f.Chunks == nil {
			f.Chunks = []model.Chunk{}
		}
		for j := range f.Chunks {
			if f.Chunks[j].FileID == "" {
				f.Chunks[j].FileID = f.ID
			}
			if f.Chunks[j].Path == "" {
				f.Chunks[j].Path = f.Path
			}
		}
		snap.Files[of.Path] = f
	}

	for i := range old.Failures {
		xf := &old.Failures[i]
		if xf.Path == "" {
			return nil, fmt.Errorf("%w: v0 failure entry %d has empty path",
				ErrUnsupportedVersion, i)
		}
		if !validCategory(xf.Category) {
			return nil, fmt.Errorf("%w: unknown failure category %q",
				ErrUnsupportedVersion, xf.Category)
		}
		if _, dup := snap.Failures[xf.Path]; dup {
			return nil, fmt.Errorf("%w: duplicate failure path %q in v0 payload",
				ErrUnsupportedVersion, xf.Path)
		}
		snap.Failures[xf.Path] = &model.Failure{
			ID:       model.FailureID(xf.Path, xf.Category),
			Path:     xf.Path,
			Category: xf.Category,
			Reason:   xf.Reason,
			SizeHint: xf.SizeHint,
		}
	}

	// Determinize embedded slices: migrations of the same bytes must be equal.
	for _, f := range snap.Files {
		sort.Slice(f.Symbols, func(i, j int) bool { return f.Symbols[i].ID < f.Symbols[j].ID })
		sort.Slice(f.Refs, func(i, j int) bool { return f.Refs[i].ID < f.Refs[j].ID })
		sort.Slice(f.Chunks, func(i, j int) bool { return f.Chunks[i].ID < f.Chunks[j].ID })
	}
	return snap, nil
}

func validCategory(c model.FailureCategory) bool {
	switch c {
	case model.FailureUnreadable, model.FailureOversized,
		model.FailureInvalidEncoding, model.FailureUnsupported,
		model.FailureParse:
		return true
	}
	return false
}
