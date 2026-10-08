// Package source scans a local filesystem tree (the simulated code origin).
//
// The scanner makes no network access and requires no credentials: the local
// filesystem is the sole "code source". Scan results are sorted for
// determinism, unreadable directories surface as errors rather than being
// silently skipped, and regular files outside the include set are returned
// in a separate excluded list so they never vanish without a trace.
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

// ReasonExcludedExtension marks a regular file that was seen during the walk
// but not selected because its extension is outside the include set.
const ReasonExcludedExtension = "excluded_extension"

// ExcludedFile is a regular file the scanner saw but did not select. It is
// reported separately so downstream layers can account for it explicitly
// instead of the file vanishing without a trace.
type ExcludedFile struct {
	RelPath string
	AbsPath string
	Size    int64
	Reason  string // currently always ReasonExcludedExtension
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
// never silently skipped. Regular files whose extension is outside the
// include set are returned in the second list (also path-sorted) so callers
// can account for them explicitly.
func (s *Scanner) Scan() ([]DiscoveredFile, []ExcludedFile, error) {
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
	var excluded []ExcludedFile
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
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			return fmt.Errorf("source: relative path for %q: %w", path, relErr)
		}
		size := int64(-1)
		if fi, fiErr := d.Info(); fiErr == nil {
			size = fi.Size()
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !s.opts.IncludeExt[ext] {
			excluded = append(excluded, ExcludedFile{
				RelPath: filepath.ToSlash(rel),
				AbsPath: path,
				Size:    size,
				Reason:  ReasonExcludedExtension,
			})
			return nil
		}
		files = append(files, DiscoveredFile{
			RelPath: filepath.ToSlash(rel),
			AbsPath: path,
			Size:    size,
		})
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].RelPath < excluded[j].RelPath })
	return files, excluded, nil
}
