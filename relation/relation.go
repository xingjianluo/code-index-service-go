// Package relation answers "follow the symbol" questions on top of an
// immutable indexed snapshot:
//
//   - References: every occurrence of a symbol name, split into occurrences
//     that resolved to a concrete definition and occurrences whose name
//     matched nothing (unresolved). Never mixed.
//   - Impact: given a set of changed files, which other files/symbols may be
//     affected, each explained by the reference chain that connects it to a
//     changed file.
//
// Everything is derived from the local snapshot only; no external services.
// All ordering is deterministic and all truncation is explicit and explained.
package relation

import (
	"github.com/xingjianluo/code-index-service-go/model"
)

// Limits bounds relation queries. Zero fields take defaults.
type Limits struct {
	MaxDefinitions int // 0 => default 64; definitions of one name considered
	MaxReferences  int // 0 => default 500; reference occurrences returned per class
	MaxImpacted    int // 0 => default 200; impacted files returned
	MaxChainEdges  int // 0 => default 8; edges kept per impact explanation
}

func (l Limits) withDefaults() Limits {
	if l.MaxDefinitions <= 0 {
		l.MaxDefinitions = 64
	}
	if l.MaxReferences <= 0 {
		l.MaxReferences = 500
	}
	if l.MaxImpacted <= 0 {
		l.MaxImpacted = 200
	}
	if l.MaxChainEdges <= 0 {
		l.MaxChainEdges = 8
	}
	return l
}

// Truncation records a deterministic, explained omission.
type Truncation struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// DefinitionView is a symbol definition annotated with its rename-stable key.
type DefinitionView struct {
	model.Symbol
	Key string `json:"key"` // StableSymbolKey: survives pure renames
}

// ReferenceHit is one occurrence of the queried name.
type ReferenceHit struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	SymbolID   string `json:"symbol_id,omitempty"`   // resolved target (path-based, per snapshot)
	TargetKey  string `json:"target_key,omitempty"`  // rename-stable key of the resolved definition
	TargetPath string `json:"target_path,omitempty"` // where the resolved definition lives
	TargetLine int    `json:"target_line,omitempty"`
}

// UnanalyzedFile accounts for one scanned-but-never-parsed file in a
// references answer, so unusual-extension or non-text files never vanish
// silently. NamePresent is "yes" (the name provably occurs, from the
// index-time token list) or "unknown" (non-text content or a truncated /
// unavailable token list; the name may still occur). Files whose recorded
// tokens provably do NOT contain the name are omitted entirely.
type UnanalyzedFile struct {
	Path        string `json:"path"`
	Category    string `json:"category"`     // "extension_not_indexed" | "non_text"
	NamePresent string `json:"name_present"` // "yes" | "unknown"
	Detail      string `json:"detail"`
}

// ReferencesOutcome is the complete answer to "where is NAME referenced".
type ReferencesOutcome struct {
	Name        string           `json:"name"`
	SnapshotID  string           `json:"snapshot_id"`
	Definitions []DefinitionView `json:"definitions"` // every definition of the name, deterministic order
	Resolved    []ReferenceHit   `json:"resolved"`    // occurrences bound to a definition
	Unresolved  []ReferenceHit   `json:"unresolved"`  // name occurrences with no definition found
	// Unanalyzed lists scanned-but-not-parsed files relevant to this name,
	// each with its skip category. Kept strictly separate from Resolved and
	// Unresolved: nothing here is a parsed occurrence.
	Unanalyzed []UnanalyzedFile `json:"unanalyzed"`
	Truncated  []Truncation     `json:"truncated"`
	Notes      []string         `json:"notes"`
}

// Edge is one hop in an impact explanation chain.
type Edge struct {
	Kind   string `json:"kind"` // "defines" | "references" | "mentions_unresolved"
	From   string `json:"from"` // file path (or file:line for reference edges)
	To     string `json:"to"`   // symbol name or file path
	Detail string `json:"detail"`
}

// ImpactedFile is one file possibly affected by the change set.
type ImpactedFile struct {
	Path    string   `json:"path"`
	Symbols []string `json:"symbols"` // changed-file symbols this file depends on
	Chains  [][]Edge `json:"chains"`  // one chain per distinct dependency, deterministic order
}

// ChangedFileStatus explains how one input path was classified, so unusual
// or non-text files are never silently dropped.
type ChangedFileStatus struct {
	Path     string `json:"path"`
	Status   string `json:"status"`             // "indexed" | "failed" | "skipped" | "unknown"
	Category string `json:"category,omitempty"` // failure/skip category when Status is "failed"/"skipped"
	Detail   string `json:"detail,omitempty"`
}

// ImpactOutcome is the complete answer to "who is affected by these changes".
type ImpactOutcome struct {
	Changed    []ChangedFileStatus `json:"changed"`
	Impacted   []ImpactedFile      `json:"impacted"`
	Suspected  []ImpactedFile      `json:"suspected"` // via unresolved same-name references
	SnapshotID string              `json:"snapshot_id"`
	Truncated  []Truncation        `json:"truncated"`
	Notes      []string            `json:"notes"`
}
