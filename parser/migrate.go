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
	snap.ID = model.SnapshotID(snap.Files, snap.Failures, snap.Skipped)
	return nil
}
