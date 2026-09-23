package service

import "errors"

// ErrNotIndexed is returned by Search before the first successful index.
var ErrNotIndexed = errors.New("index not built yet; run reindex first")
