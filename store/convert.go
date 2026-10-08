package store

import (
	"bytes"
	"io"
	"time"

	"github.com/xingjianluo/code-index-service-go/model"
)

// strictReader trims a single trailing BOM/whitespace-free JSON payload; the
// decoder itself performs all structural validation.
func strictReader(data []byte) io.Reader {
	return bytes.NewReader(bytes.TrimSpace(data))
}

func toEnvelope(snap *model.Snapshot) *envelope {
	env := &envelope{
		ID:          snap.ID,
		StoreFormat: snap.StoreFormat,
		IndexRules:  snap.IndexRules,
		Root:        snap.Root,
		CreatedAt:   snap.CreatedAt.UTC().Format(time.RFC3339Nano),
		Files:       make(map[string]*fileEnvelope, len(snap.Files)),
		Failures:    snap.Failures,
		Skipped:     snap.Skipped,
	}
	for path, rec := range snap.Files {
		fe := &fileEnvelope{
			Path:        rec.Path,
			Language:    rec.Language,
			Size:        rec.Size,
			ContentHash: rec.ContentHash,
			Lines:       rec.Lines,
			Symbols:     rec.Symbols,
			References:  rec.References,
			Chunks:      rec.Chunks,
			Metadata:    rec.Metadata,
		}
		if fe.Symbols == nil {
			fe.Symbols = []model.Symbol{}
		}
		if fe.References == nil {
			fe.References = []model.Reference{}
		}
		if fe.Chunks == nil {
			fe.Chunks = []model.Chunk{}
		}
		env.Files[path] = fe
	}
	return env
}

func fromEnvelope(env *envelope) *model.Snapshot {
	snap := &model.Snapshot{
		ID:          env.ID,
		StoreFormat: env.StoreFormat,
		IndexRules:  env.IndexRules,
		Root:        env.Root,
		Files:       make(map[string]*model.FileRecord, len(env.Files)),
		Failures:    env.Failures,
		Skipped:     env.Skipped,
		Extra:       nil,
	}
	if t, err := time.Parse(time.RFC3339Nano, env.CreatedAt); err == nil {
		snap.CreatedAt = t
	}
	for path, fe := range env.Files {
		rec := &model.FileRecord{
			Path:        fe.Path,
			Language:    fe.Language,
			Size:        fe.Size,
			ContentHash: fe.ContentHash,
			Lines:       fe.Lines,
			Symbols:     fe.Symbols,
			References:  fe.References,
			Chunks:      fe.Chunks,
			Metadata:    fe.Metadata,
		}
		if rec.Symbols == nil {
			rec.Symbols = []model.Symbol{}
		}
		if rec.References == nil {
			rec.References = []model.Reference{}
		}
		if rec.Chunks == nil {
			rec.Chunks = []model.Chunk{}
		}
		snap.Files[path] = rec
	}
	if snap.Failures == nil {
		snap.Failures = []model.Failure{}
	}
	if snap.Skipped == nil {
		snap.Skipped = []model.SkippedFile{}
	}
	return snap
}
