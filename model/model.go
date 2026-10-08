// Package model defines the core data model for the code index service.
package model

import "time"

// Versioning ------------------------------------------------------------------

const (
	// StoreFormatV2 is the current on-disk envelope format version.
	StoreFormatV2 = 2
	// IndexRulesV2 is the current index/normalization rules version.
	IndexRulesV2 = 2

	MinSupportedStoreFormat = 1
	MinSupportedIndexRules  = 1
)

// Failure categories. Every unreadable, invalid, oversize or unparseable
// input is recorded under one of these distinguishable categories instead of
// being silently skipped.
type FailureCategory string

const (
	FailureUnreadable  FailureCategory = "unreadable"   // file cannot be read (I/O error, permission)
	FailureInvalidUTF8 FailureCategory = "invalid_utf8" // content is not valid UTF-8
	FailureOversize    FailureCategory = "oversize"     // file exceeds the configured size limit
	FailureUnparseable FailureCategory = "unparseable"  // parser rejects the content
	FailureMissing     FailureCategory = "missing"      // scan saw the file but read found it gone
)

// Skip categories. Files that were SEEN during the repository scan but not
// parsed as source are recorded under one of these distinguishable
// categories instead of vanishing from every query answer.
type SkipCategory string

const (
	// SkipExtensionNotIndexed: the file's extension is not in the configured
	// include set, so it was never parsed (its content IS analyzable text).
	SkipExtensionNotIndexed SkipCategory = "extension_not_indexed"
	// SkipNonText: the content is not analyzable text (invalid UTF-8 or
	// NUL bytes found during the sniff), so no symbol-level analysis applies.
	SkipNonText SkipCategory = "non_text"
)

// SkippedFile records a scanned-but-not-parsed file with the reason. For
// text files the distinct raw identifier tokens (case preserved) are kept so
// relation queries can determine name presence without touching the disk;
// Tokens is nil when the content is not text or could not be inspected.
type SkippedFile struct {
	Path        string       `json:"path"`
	Category    SkipCategory `json:"category"`
	Detail      string       `json:"detail"`
	Size        int64        `json:"size"`
	ContentHash string       `json:"content_hash,omitempty"`
	// Tokens holds the sorted distinct raw identifiers seen in the content
	// (text files only). TokensTruncated reports a deterministic cap hit.
	Tokens          []string `json:"tokens,omitempty"`
	TokensTruncated bool     `json:"tokens_truncated,omitempty"`
}

// Content identity ------------------------------------------------------------

// Failure records a rejected input with a distinguishable reason.
type Failure struct {
	Path     string          `json:"path"`
	Category FailureCategory `json:"category"`
	Message  string          `json:"message"`
	Size     int64           `json:"size"`
}

// SymbolKind enumerates extracted symbol kinds.
type SymbolKind string

const (
	SymbolFunc   SymbolKind = "func"
	SymbolType   SymbolKind = "type"
	SymbolVar    SymbolKind = "var"
	SymbolConst  SymbolKind = "const"
	SymbolField  SymbolKind = "field"
	SymbolMethod SymbolKind = "method"
)

// Symbol is a named definition extracted from a file.
type Symbol struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Kind   SymbolKind `json:"kind"`
	Path   string     `json:"path"`
	Line   int        `json:"line"`
	Column int        `json:"column"`
}

// Reference is a use of a (possibly resolved) name in a file.
type Reference struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	SymbolID string `json:"symbol_id,omitempty"` // resolved target, empty if unresolved
}

// Chunk is a retrievable normalized content fragment.
type Chunk struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	StartLine   int      `json:"start_line"`
	EndLine     int      `json:"end_line"`
	Kind        string   `json:"kind"` // declaration / file / text
	Title       string   `json:"title,omitempty"`
	Content     string   `json:"content"`
	Tokens      []string `json:"tokens"`
	SymbolIDs   []string `json:"symbol_ids,omitempty"`
	ContentHash string   `json:"content_hash"`
}

// FileRecord is the normalized parse result of one source file.
type FileRecord struct {
	Path        string            `json:"path"`
	Language    string            `json:"language"`
	Size        int64             `json:"size"`
	ContentHash string            `json:"content_hash"` // stable identity: unchanged content => same hash
	Lines       int               `json:"lines"`
	Symbols     []Symbol          `json:"symbols"`
	References  []Reference       `json:"references"`
	Chunks      []Chunk           `json:"chunks"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Snapshot is an immutable, point-in-time consistent view of an index.
type Snapshot struct {
	ID          string                 `json:"id"`
	StoreFormat int                    `json:"store_format"`
	IndexRules  int                    `json:"index_rules"`
	Files       map[string]*FileRecord `json:"files"`
	Failures    []Failure              `json:"failures"`
	// Skipped lists files the scan saw but never parsed (unusual extension or
	// non-text content), so queries can account for them explicitly.
	Skipped   []SkippedFile  `json:"skipped,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Root      string         `json:"root"`
	Extra     map[string]any `json:"-"` // unknown envelope fields, rejected on load
}

// IndexReport describes the outcome of one incremental indexing pass.
type IndexReport struct {
	SnapshotID string    `json:"snapshot_id"`
	Root       string    `json:"root"`
	Added      []string  `json:"added"`
	Modified   []string  `json:"modified"`
	Deleted    []string  `json:"deleted"`
	Renamed    []Rename  `json:"renamed"`
	Unchanged  []string  `json:"unchanged"`
	Failed     []Failure `json:"failed"`
}

// Rename describes a path change at unchanged content.
type Rename struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
}
