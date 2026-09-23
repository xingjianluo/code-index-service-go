// Package parser parses and normalizes source files into model records.
//
// Every input ends in exactly one of two outcomes: a FileRecord or a typed
// Failure (unreadable / invalid_utf8 / oversize / unparseable / missing).
// Nothing is silently skipped. Records contain stable symbols, references and
// retrievable chunks.
package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/xingjianluo/code-index-service-go/model"
	"github.com/xingjianluo/code-index-service-go/source"
)

// Options configures parsing limits.
type Options struct {
	MaxFileSize   int64
	MaxFileLines  int // 0 => no line cap
	MaxChunkLines int // 0 => default 60
}

// Parser turns discovered files into normalized records (or typed failures).
type Parser struct {
	opts Options
}

// New creates a Parser.
func New(opts Options) *Parser {
	if opts.MaxChunkLines <= 0 {
		opts.MaxChunkLines = 60
	}
	if opts.MaxFileLines <= 0 {
		opts.MaxFileLines = 200000
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = 2 << 20 // 2 MiB default
	}
	return &Parser{opts: opts}
}

// ParseFile reads and parses one file. It returns exactly one of record or
// failure. A failure is never silent: its category is distinguishable and its
// message records the concrete evidence (size, error text).
func (p *Parser) ParseFile(ctx context.Context, f source.DiscoveredFile) (*model.FileRecord, *model.Failure) {
	if err := ctx.Err(); err != nil {
		return nil, p.fail(f, model.FailureUnreadable, "context: %v", err)
	}

	// Authoritative stat at parse time (scan-time size is only advisory).
	fi, statErr := os.Stat(f.AbsPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, p.fail(f, model.FailureMissing, "file vanished between scan and parse: %v", statErr)
		}
		return nil, p.fail(f, model.FailureUnreadable, "stat: %v", statErr)
	}
	if fi.IsDir() {
		return nil, p.fail(f, model.FailureUnreadable, "path is a directory, expected regular file")
	}
	size := fi.Size()

	if size > p.opts.MaxFileSize {
		return nil, p.failSize(f, size, "size %d exceeds limit %d bytes", size, p.opts.MaxFileSize)
	}

	data, readErr := os.ReadFile(f.AbsPath)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return nil, p.fail(f, model.FailureMissing, "file vanished at read: %v", readErr)
		}
		return nil, &model.Failure{
			Path:     f.RelPath,
			Category: model.FailureUnreadable,
			Message:  fmt.Sprintf("read: %v", readErr),
			Size:     size,
		}
	}
	// A file that grew past the limit between stat and read is still bounded.
	if int64(len(data)) > p.opts.MaxFileSize {
		return nil, p.failSize(f, int64(len(data)), "read size %d exceeds limit %d bytes", int64(len(data)), p.opts.MaxFileSize)
	}
	if !utf8.Valid(data) {
		return nil, &model.Failure{
			Path:     f.RelPath,
			Category: model.FailureInvalidUTF8,
			Message:  "content is not valid UTF-8",
			Size:     int64(len(data)),
		}
	}

	content := string(data)
	if lineCount := strings.Count(content, "\n") + 1; lineCount > p.opts.MaxFileLines {
		return nil, &model.Failure{
			Path:     f.RelPath,
			Category: model.FailureOversize,
			Message:  fmt.Sprintf("line count %d exceeds limit %d", lineCount, p.opts.MaxFileLines),
			Size:     int64(len(data)),
		}
	}

	var rec *model.FileRecord
	var parseErr *parseFailure
	fileHash := model.HashBytes(data)
	switch strings.ToLower(filepath.Ext(f.RelPath)) {
	case ".go":
		rec, parseErr = parseGo(f, content, int64(len(data)), fileHash, p.opts)
	default:
		rec, parseErr = parseText(f, content, int64(len(data)), fileHash, p.opts)
	}
	if parseErr != nil {
		return nil, &model.Failure{
			Path:     f.RelPath,
			Category: model.FailureUnparseable,
			Message:  parseErr.message,
			Size:     int64(len(data)),
		}
	}
	// Raw-content hash keys the record and every chunk identity.
	rec.ContentHash = fileHash
	return rec, nil
}

type parseFailure struct{ message string }

func (p *Parser) fail(f source.DiscoveredFile, cat model.FailureCategory, format string, args ...any) *model.Failure {
	return &model.Failure{
		Path:     f.RelPath,
		Category: cat,
		Message:  fmt.Sprintf(format, args...),
		Size:     f.Size,
	}
}

func (p *Parser) failSize(f source.DiscoveredFile, size int64, format string, args ...any) *model.Failure {
	fail := p.fail(f, model.FailureOversize, format, args...)
	fail.Size = size
	return fail
}
