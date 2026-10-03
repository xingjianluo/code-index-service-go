package relation

import "errors"

// Engine answers relation queries against an immutable snapshot.
type Engine struct{}

// NewEngine creates a relation Engine.
func NewEngine() *Engine { return &Engine{} }

// errNilSnapshot is returned when no index has been published yet.
var errNilSnapshot = errors.New("relation: nil snapshot (no index published)")
