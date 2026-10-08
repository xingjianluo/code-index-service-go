// Package source scans a local filesystem tree (the simulated code origin).
//
// The scanner makes no network access and requires no credentials: the local
// filesystem is the sole "code source". Scan results are sorted for
// determinism, and unreadable directories surface as errors rather than being
// silently skipped.
package source

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DiscoveredFile describes one file found during a scan.
type DiscoveredFile struct {
	// RelPath is the repository-relative, slash-separated path (stable key).
	RelPath string
	// AbsPath is the absolute path on the local filesystem.
	AbsPath string
	Size    int64
}

// Options controls repository scanning.
type Options struct {
	Root        string
	MaxFileSize int64 // informational at scan time; hard enforcement happens in parser
	// SkipDirs holds directory basenames that are pruned during the walk.
	SkipDirs map[string]bool
	// IncludeExt lists included lower-cased extensions (with leading dot).
	// A nil map selects the built-in default set.
	IncludeExt map[string]bool
}

// DefaultSkipDirs is the built-in directory prune set.
var DefaultSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".idea":        true,
	".vscode":      true,
}

// DefaultIncludeExt is the built-in source-file extension set.
var DefaultIncludeExt = map[string]bool{
	".go": true, ".mod": true, ".md": true, ".txt": true,
	".py": true, ".js": true, ".ts": true, ".java": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
	".rs": true, ".rb": true, ".sh": true, ".json": true, ".yaml": true, ".yml": true,
}

// Scanner enumerates source files from the local filesystem.
type Scanner struct {
	opts Options
}

// NewScanner creates a Scanner with the given options, applying defaults.
func NewScanner(opts Options) *Scanner {
	if opts.SkipDirs == nil {
		opts.SkipDirs = DefaultSkipDirs
	}
	if opts.IncludeExt == nil {
		opts.IncludeExt = DefaultIncludeExt
	}
	return &Scanner{opts: opts}
}

// Scan lists candidate source files in deterministic (path-sorted) order.
// A missing root or an unreadable directory is returned as an error and is
// never silently skipped.
func (s *Scanner) Scan() ([]DiscoveredFile, error) {
	included, _, err := s.ScanWithExcluded()
	return included, err
}

// ScanWithExcluded additionally reports the regular files the walk saw but
// rejected by the extension filter, also in deterministic path order. Every
// file on disk is therefore accounted for: either it is a parse candidate or
// it appears in the excluded list (directories in SkipDirs are still pruned
// wholesale, and non-regular files are still ignored).
func (s *Scanner) ScanWithExcluded() (included, excluded []DiscoveredFile, err error) {
	root := s.opts.Root
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("source: stat root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("source: root %q is not a directory", root)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("source: resolve root %q: %w", root, err)
	}

	var files []DiscoveredFile
	var skipped []DiscoveredFile
	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission/IO errors on a directory entry are fatal to the scan.
			if d != nil && d.IsDir() {
				return fmt.Errorf("source: read directory %q: %w", path, err)
			}
			return fmt.Errorf("source: access %q: %w", path, err)
		}
		if d.IsDir() {
			name := d.Name()
			if path != absRoot && s.opts.SkipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, devices, sockets etc. are not source files
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			return fmt.Errorf("source: relative path for %q: %w", path, relErr)
		}
		size := int64(-1)
		if fi, fiErr := d.Info(); fiErr == nil {
			size = fi.Size()
		}
		df := DiscoveredFile{
			RelPath: filepath.ToSlash(rel),
			AbsPath: path,
			Size:    size,
		}
		if !s.opts.IncludeExt[ext] {
			skipped = append(skipped, df)
			return nil
		}
		files = append(files, df)
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].RelPath < skipped[j].RelPath })
	return files, skipped, nil
}
