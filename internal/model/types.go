// Package model defines the core data model of the code index service:
// parsed files, symbols, references, searchable chunks, parse failures
// and immutable index snapshots.
package model

// Format versions of the on-disk index storage layout.
const (
	FormatV1 uint32 = 1
)

// Rule versions of the indexing/normalization rules. Bumping this version
// changes how IDs and chunks are produced, so old snapshots must be rebuilt.
const (
	RulesV1 uint32 = 1
)

// FailureCategory distinguishes why a file could not be indexed.
type FailureCategory string

const (
	// FailureUnreadable: file cannot be read (permission, I/O error, vanished).
	FailureUnreadable FailureCategory = "unreadable"
	// FailureOversized: file exceeds the configured size limit.
	FailureOversized FailureCategory = "oversized"
	// FailureInvalidEncoding: file content is not valid UTF-8.
	FailureInvalidEncoding FailureCategory = "invalid_encoding"
	// FailureUnsupported: file type is not supported by any parser.
	FailureUnsupported FailureCategory = "unsupported_type"
	// FailureParse: file type is supported but its content cannot be parsed.
	FailureParse FailureCategory = "parse_error"
)

// Language of a source file.
type Language string

const (
	LangGo       Language = "go"
	LangText     Language = "text"
	LangMarkdown Language = "markdown"
	LangUnknown  Language = "unknown"
)

// SymbolKind classifies a declared symbol.
type SymbolKind string

const (
	SymbolFunc   SymbolKind = "function"
	SymbolType   SymbolKind = "type"
	SymbolVar    SymbolKind = "variable"
	SymbolConst  SymbolKind = "constant"
	SymbolMethod SymbolKind = "method"
)

// Symbol is a named declaration extracted from a file.
type Symbol struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      SymbolKind `json:"kind"`
	Line      int        `json:"line"`
	Column    int        `json:"column"`
	Receiver  string     `json:"receiver,omitempty"` // for methods
	Signature string     `json:"signature,omitempty"`
}

// Reference is a usage of a (possibly external) name inside a file.
type Reference struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// Chunk is a retrievable, self-contained fragment of a file.
type Chunk struct {
	ID         string   `json:"id"`
	FileID     string   `json:"file_id"`
	Path       string   `json:"path"`
	Language   Language `json:"language"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	Kind       string   `json:"kind"` // "symbol" or "text"
	SymbolName string   `json:"symbol_name,omitempty"`
	Content    string   `json:"content"`
	Tokens     int      `json:"tokens"`
}

// File is the normalized representation of one repository path.
type File struct {
	ID       string      `json:"id"`
	Path     string      `json:"path"`
	Language Language    `json:"language"`
	Size     int64       `json:"size"`
	Hash     string      `json:"hash"` // SHA-256 of normalized content
	Lines    int         `json:"lines"`
	Symbols  []Symbol    `json:"symbols"`
	Refs     []Reference `json:"refs"`
	Chunks   []Chunk     `json:"chunks"`
}

// Failure records a file that could not be indexed, with a distinguishable
// category and an explanation. Failures are first-class snapshot members,
// never silently dropped.
type Failure struct {
	ID       string          `json:"id"`
	Path     string          `json:"path"`
	Category FailureCategory `json:"category"`
	Reason   string          `json:"reason"`
	SizeHint int64           `json:"size_hint,omitempty"`
}

// Snapshot is an immutable, point-in-time view of a whole repository index.
type Snapshot struct {
	FormatVersion uint32              `json:"format_version"`
	RuleVersion   uint32              `json:"rule_version"`
	Files         map[string]*File    `json:"files"`    // keyed by path
	Failures      map[string]*Failure `json:"failures"` // keyed by path
	// Renames maps newPath -> oldPath for paths detected as renames
	// (identical content hash, old path removed, new path added).
	Renames map[string]string `json:"renames"`
	// Generation increases only when snapshot content actually changes.
	Generation int64 `json:"generation"`
}

// Manifest is persisted alongside the snapshot and describes its versions.
type Manifest struct {
	FormatVersion uint32 `json:"format_version"`
	RuleVersion   uint32 `json:"rule_version"`
	Generation    int64  `json:"generation"`
}
