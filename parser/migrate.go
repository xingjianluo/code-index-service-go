package parser

import (
	"fmt"

	"github.com/xingjianluo/code-index-service-go/model"
)

// MigrateV1ToV2 is the fixed, documented migration path for old data.
//
// Semantics of v1 -> v2:
//   - structure (files/symbols/references/chunks) is unchanged and remains
//     usable as-is;
//   - chunk token lists were produced by the v1 tokenizer (no camel-case
//     splitting). They are rebuilt deterministically from each chunk's stored
//     Content using the v2 rules (Tokenize), so retrieval behavior matches a
//     freshly built v2 index;
//   - content hashes and symbol IDs are preserved (their definitions did not
//     change); chunk IDs embed only path/line-range/content-hash and are
//     preserved too;
//   - the snapshot identity is recomputed under v2 (identical content state
//     indexes to the same ID as a native v2 build, since the ID is derived
//     from file content hashes and failures);
//   - a chunk lacking content cannot be re-normalized: this is reported as a
//     hard migration error instead of silent degradation.
//
// The caller (store) tags the snapshot StoreFormat/IndexRules = 2 afterwards.
func MigrateV1ToV2(snap *model.Snapshot) error {
	if snap == nil {
		return fmt.Errorf("migrate: nil snapshot")
	}
	for path, rec := range snap.Files {
		for i := range rec.Chunks {
			ch := &rec.Chunks[i]
			if ch.Content == "" && ch.ContentHash != "" {
				return fmt.Errorf("migrate: chunk %s (%s:%d-%d) has no content to re-tokenize",
					ch.ID, path, ch.StartLine, ch.EndLine)
			}
			ch.Tokens = Tokenize(ch.Content)
		}
		if rec.Metadata == nil {
			rec.Metadata = map[string]string{}
		}
		rec.Metadata["migrated_from"] = "v1"
	}
	snap.StoreFormat = model.StoreFormatV2
	snap.IndexRules = model.IndexRulesV2
	snap.ID = model.SnapshotID(snap.Files, snap.Failures)
	return nil
}

// MigrateToCurrent is the fixed migration path to the current rules
// (StoreFormatV2 / IndexRulesV3). It chains the documented steps:
//
//   - v1 -> v2: chunk tokens rebuilt with the v2 tokenizer (see
//     MigrateV1ToV2);
//   - v2 -> v3: every symbol gains its path-independent Key, recomputed
//     deterministically as StableSymbolKey(name, kind, line, contentHash)
//     from data the v2 record already carries. Symbol IDs, chunk IDs and
//     content hashes are untouched, so the snapshot identity is unchanged.
//
// Already-current snapshots pass through unchanged (idempotent).
func MigrateToCurrent(snap *model.Snapshot) error {
	if snap == nil {
		return fmt.Errorf("migrate: nil snapshot")
	}
	if snap.StoreFormat == 1 || snap.IndexRules == 1 {
		if err := MigrateV1ToV2(snap); err != nil {
			return err
		}
	}
	for _, rec := range snap.Files {
		for i := range rec.Symbols {
			sym := &rec.Symbols[i]
			if sym.Key == "" {
				sym.Key = model.StableSymbolKey(sym.Name, sym.Kind, sym.Line, rec.ContentHash)
			}
		}
		if rec.Metadata == nil {
			rec.Metadata = map[string]string{}
		}
		if rec.Metadata["migrated_from"] == "" {
			rec.Metadata["migrated_from"] = "v2"
		}
	}
	snap.StoreFormat = model.StoreFormatV2
	snap.IndexRules = model.IndexRulesV3
	snap.ID = model.SnapshotID(snap.Files, snap.Failures)
	return nil
}
