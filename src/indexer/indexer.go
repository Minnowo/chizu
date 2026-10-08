// Package indexer walks a directory and keeps its text files in the index.
package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"chizu/src/database"
	"chizu/src/ignore"

	"github.com/rs/zerolog"
)

// DefaultMaxSize is the largest file indexed when Options.MaxSize is zero.
const DefaultMaxSize = 10 << 20

// sniffLen is how much of a file is checked for NUL bytes.
const sniffLen = 8000

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// indexMu serializes index runs in this process (e.g. from HTTP and MCP at
// once) so they wait on each other rather than on SQLite's busy timeout.
var indexMu sync.Mutex

// Options control which files are indexed. The filter fields are saved
// with the root; when none are set, the saved filters are reused.
type Options struct {
	// Exts limits indexing to these extensions (case-insensitive, dot optional).
	Exts []string `json:"exts,omitempty"`
	// Include, if set, limits indexing to files matching one of these gitignore-style patterns.
	Include []string `json:"include,omitempty"`
	// Exclude skips paths matching these gitignore-style patterns, on top of .chizuignore.
	Exclude []string `json:"exclude,omitempty"`
	// MaxSize skips files larger than this many bytes (DefaultMaxSize if zero).
	MaxSize int64 `json:"max_size,omitempty"`
	// ClearFilters ignores any saved filters (and saves the ones given, if any).
	ClearFilters bool `json:"-"`
	// Progress, if set, is called after each candidate file is processed.
	Progress func(Progress) `json:"-"`
	// SkipPaths are absolute file paths never indexed, such as the database itself.
	SkipPaths []string       `json:"-"`
	Log       zerolog.Logger `json:"-"`
}

func (o Options) hasFilters() bool {
	return len(o.Exts) > 0 || len(o.Include) > 0 || len(o.Exclude) > 0 || o.MaxSize > 0
}

// filters holds the compiled form of Options.
type filters struct {
	exts    map[string]bool
	include *ignore.Rules
	exclude *ignore.Rules
	maxSize int64
}

func compileFilters(o Options) (filters, error) {

	f := filters{exts: map[string]bool{}, maxSize: o.MaxSize}
	if f.maxSize <= 0 {
		f.maxSize = DefaultMaxSize
	}

	for _, e := range o.Exts {
		f.exts[normalizeExt(e)] = true
	}

	var err error
	if len(o.Exclude) > 0 {
		if f.exclude, err = ignore.Parse("", strings.NewReader(strings.Join(o.Exclude, "\n"))); err != nil {
			return f, err
		}
	}
	if len(o.Include) > 0 {
		if f.include, err = ignore.Parse("", strings.NewReader(strings.Join(o.Include, "\n"))); err != nil {
			return f, err
		}
	}

	return f, nil
}

// Progress reports how far an index run has got.
type Progress struct {
	Root string
	// Files is how many candidate files have been processed so far.
	Files int
	// Path is the file just processed, relative to Root.
	Path string
}

type Stats struct {
	Root      string `json:"root"`
	Added     int    `json:"added"`
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
	Removed   int    `json:"removed"`
	Skipped   int    `json:"skipped"`
}

// ResolveRoot turns dir into the absolute, symlink-free path stored as a root.
func ResolveRoot(dir string) (string, error) {

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}

	return real, nil
}

// Index brings the index for dir up to date. Unchanged files (same size and
// mtime, or same content hash) are not rewritten, and files that are gone or
// no longer pass the filters are removed.
func Index(ctx context.Context, db *database.DB, dir string, opts Options) (Stats, error) {

	root, err := ResolveRoot(dir)
	if err != nil {
		return Stats{}, err
	}

	indexMu.Lock()
	defer indexMu.Unlock()

	matcher, err := ignore.NewMatcher(root)
	if err != nil {
		return Stats{}, err
	}

	skip := map[string]bool{}
	for _, p := range opts.SkipPaths {
		if abs, err := filepath.Abs(p); err == nil {
			skip[abs] = true
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				skip[real] = true
			}
		}
	}

	stats := Stats{Root: root}

	err = db.WithTx(ctx, func(s *database.Store) error {

		r, err := s.UpsertRoot(ctx, root)
		if err != nil {
			return err
		}

		if !opts.hasFilters() && !opts.ClearFilters && r.Options != "" {
			if err := json.Unmarshal([]byte(r.Options), &opts); err != nil {
				return fmt.Errorf("saved options for %s: %w", root, err)
			}
		}

		saved, err := json.Marshal(opts)
		if err != nil {
			return err
		}
		if err := s.SetRootOptions(ctx, r.ID, string(saved)); err != nil {
			return err
		}

		f, err := compileFilters(opts)
		if err != nil {
			return err
		}

		existing, err := s.FileStates(ctx, r.ID)
		if err != nil {
			return err
		}

		seen := make(map[string]bool, len(existing))
		files := 0

		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {

			if err != nil {
				opts.Log.Warn().Err(err).Str("path", path).Msg("cannot read")
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if err := ctx.Err(); err != nil {
				return err
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			rel = filepath.ToSlash(rel)

			if d.IsDir() {
				if matcher.Ignored(rel, true) || (f.exclude != nil && f.exclude.Matches(rel, true)) {
					return filepath.SkipDir
				}
				if err := matcher.LoadDir(root, rel); err != nil {
					// Its rules are unknown, so skipping is safer than indexing it.
					opts.Log.Warn().Err(err).Str("dir", rel).Msg("cannot read ignore file, skipping directory")
					return filepath.SkipDir
				}
				return nil
			}

			if !d.Type().IsRegular() || skip[path] {
				return nil
			}
			if matcher.Ignored(rel, false) || (f.exclude != nil && f.exclude.Matches(rel, false)) {
				return nil
			}
			if f.include != nil && !f.include.Matches(rel, false) {
				return nil
			}
			ext := normalizeExt(filepath.Ext(rel))
			if len(f.exts) > 0 && !f.exts[ext] {
				return nil
			}

			if opts.Progress != nil {
				defer func() {
					files++
					opts.Progress(Progress{Root: root, Files: files, Path: rel})
				}()
			}

			info, err := d.Info()
			if err != nil {
				opts.Log.Warn().Err(err).Str("path", rel).Msg("cannot stat")
				return nil
			}

			old, known := existing[rel]
			mtime := info.ModTime().UnixNano()

			if known && old.Size == info.Size() && old.ModTime == mtime {
				seen[rel] = true
				stats.Unchanged++
				return nil
			}

			if info.Size() > f.maxSize {
				opts.Log.Debug().Str("path", rel).Int64("size", info.Size()).Msg("skipping large file")
				stats.Skipped++
				return nil
			}

			data, err := readText(path)
			if err != nil {
				opts.Log.Warn().Err(err).Str("path", rel).Msg("cannot read")
				stats.Skipped++
				return nil
			}

			data = bytes.TrimPrefix(data, utf8BOM)
			if data == nil || !IsText(data) {
				opts.Log.Debug().Str("path", rel).Msg("skipping non-text file")
				stats.Skipped++
				return nil
			}

			sum := sha256.Sum256(data)
			hash := hex.EncodeToString(sum[:])
			seen[rel] = true

			if known && old.Hash == hash {
				stats.Unchanged++
				return s.TouchFile(ctx, old.ID, mtime)
			}

			_, err = s.UpsertFile(ctx, database.File{
				RootID:  r.ID,
				Path:    rel,
				Ext:     ext,
				Size:    info.Size(),
				ModTime: mtime,
				Hash:    hash,
				Content: string(data),
			})
			if err != nil {
				return err
			}

			if known {
				stats.Updated++
				opts.Log.Debug().Str("path", rel).Msg("updated")
			} else {
				stats.Added++
				opts.Log.Debug().Str("path", rel).Msg("added")
			}

			return nil
		})
		if walkErr != nil {
			return walkErr
		}

		for path, st := range existing {
			if seen[path] {
				continue
			}
			if err := s.DeleteFile(ctx, st.ID); err != nil {
				return err
			}
			stats.Removed++
			opts.Log.Debug().Str("path", path).Msg("removed")
		}

		return s.SetRootIndexedAt(ctx, r.ID, time.Now().Unix())
	})

	return stats, err
}

// IndexAll re-indexes every root in the database. A root that fails (for
// example because its directory was deleted) does not stop the others; its
// error is included in the returned error.
func IndexAll(ctx context.Context, db *database.DB, opts Options) ([]Stats, error) {

	roots, err := db.ListRoots(ctx)
	if err != nil {
		return nil, err
	}

	var all []Stats
	var errs []error

	for _, r := range roots {
		stats, err := Index(ctx, db, r.Path, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Path, err))
			continue
		}
		all = append(all, stats)
	}

	return all, errors.Join(errs...)
}

// LookupRoot finds the root for dir, which may be its stored path, a
// relative path, or a path through a symlink.
func LookupRoot(ctx context.Context, db *database.DB, dir string) (database.Root, error) {

	r, err := db.GetRoot(ctx, dir)
	if !errors.Is(err, database.ErrNotFound) {
		return r, err
	}

	if abs, err := filepath.Abs(dir); err == nil {
		if r, err := db.GetRoot(ctx, abs); !errors.Is(err, database.ErrNotFound) {
			return r, err
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			return db.GetRoot(ctx, real)
		}
	}

	return database.Root{}, fmt.Errorf("%w: %s is not an indexed root", database.ErrNotFound, dir)
}

// StoredOptions returns the filters saved for an indexed root.
func StoredOptions(ctx context.Context, db *database.DB, dir string) (Options, error) {

	r, err := LookupRoot(ctx, db, dir)
	if err != nil {
		return Options{}, err
	}

	var opts Options
	err = json.Unmarshal([]byte(r.Options), &opts)
	return opts, err
}

// Remove drops a root and all its files from the index.
func Remove(ctx context.Context, db *database.DB, dir string) error {

	r, err := LookupRoot(ctx, db, dir)
	if err != nil {
		return err
	}

	return db.DeleteRoot(ctx, r.ID)
}

// readText reads a file, returning nil without reading the rest if its
// first sniffLen bytes contain a NUL (so binaries are cheap to skip).
func readText(path string) ([]byte, error) {

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	head := make([]byte, sniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]

	if bytes.IndexByte(head, 0) >= 0 {
		return nil, nil
	}

	rest, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	return append(head, rest...), nil
}

// IsText reports whether data looks like a UTF-8 text file.
func IsText(data []byte) bool {
	if bytes.IndexByte(data[:min(len(data), sniffLen)], 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

func normalizeExt(ext string) string {
	return strings.ToLower(strings.TrimPrefix(ext, "."))
}
