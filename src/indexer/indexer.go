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
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"chizu/src/database"
	"chizu/src/ignore"
	"chizu/src/symbols"

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
	// Workers is how many files are read and hashed in parallel (NumCPU if zero).
	Workers int `json:"-"`
	// Progress, if set, is called after each candidate file is processed.
	Progress func(Progress) `json:"-"`
	// SkipPaths are absolute file paths never indexed, such as the database itself.
	SkipPaths []string       `json:"-"`
	Log       zerolog.Logger `json:"-"`
}

func (o Options) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return runtime.NumCPU()
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

// SkipSet returns paths as they can appear under a resolved root: absolute,
// with symlinks in their directory resolved, and with the file resolved if
// it is a symlink itself. Resolving the directory matters for files that do
// not exist yet, such as a database's -wal file. On Windows it also fixes
// the case of each part, so a lowercase drive letter still matches.
func SkipSet(paths []string) map[string]bool {

	skip := map[string]bool{}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		skip[abs] = true
		if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
			skip[filepath.Join(dir, filepath.Base(abs))] = true
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			skip[real] = true
		}
	}
	return skip
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
// no longer pass the filters are removed. Source files are parsed for their
// definitions; see package symbols.
func Index(ctx context.Context, db *database.DB, dir string, opts Options) (Stats, error) {

	root, err := ResolveRoot(dir)
	if err != nil {
		return Stats{}, err
	}
	return index(ctx, db, root, []string{""}, opts)
}

// IndexPaths is Index limited to paths, absolute or relative to dir, which
// is how a watcher keeps a large root up to date without walking all of it.
// A directory is indexed with everything below it, a path that is gone or
// ignored is removed from the index with everything below it, and an ignore
// file stands for the directory it applies to. Paths outside dir are
// skipped. The result is the same as Index if nothing else changed.
func IndexPaths(ctx context.Context, db *database.DB, dir string, paths []string, opts Options) (Stats, error) {

	root, err := ResolveRoot(dir)
	if err != nil {
		return Stats{}, err
	}
	scopes := scopesOf(root, paths)
	if len(scopes) == 0 {
		return Stats{Root: root}, nil
	}
	return index(ctx, db, root, scopes, opts)
}

// scopesOf turns paths into slash-separated paths relative to root ("" is
// root itself), leaving out any path below another one.
func scopesOf(root string, paths []string) []string {

	set := map[string]bool{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		rel = filepath.ToSlash(rel)
		if path.Base(rel) == ignore.FileName {
			rel = path.Dir(rel)
		}
		if rel == "." {
			rel = ""
		}
		set[rel] = true
	}

	var scopes []string
	for rel := range set {
		if !hasAncestorIn(rel, set) {
			scopes = append(scopes, rel)
		}
	}
	sort.Strings(scopes)
	return scopes
}

func hasAncestorIn(rel string, set map[string]bool) bool {
	for rel != "" {
		rel = path.Dir(rel)
		if rel == "." {
			rel = ""
		}
		if set[rel] {
			return true
		}
	}
	return false
}

// index updates the files at and below each of scopes ("" for all of root).
func index(ctx context.Context, db *database.DB, root string, scopes []string, opts Options) (Stats, error) {

	indexMu.Lock()
	defer indexMu.Unlock()

	matcher, err := ignore.NewMatcher(root)
	if err != nil {
		return Stats{}, err
	}

	skip := SkipSet(opts.SkipPaths)

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

		existing := map[string]database.FileState{}
		for _, scope := range scopes {
			states, err := s.FileStatesUnder(ctx, r.ID, scope)
			if err != nil {
				return err
			}
			maps.Copy(existing, states)
		}

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Pipeline: one walker finds candidate files, workers read and hash
		// them in parallel, and this goroutine (the only one touching the
		// transaction) writes the results in batches.
		jobs := make(chan job, 256)
		results := make(chan result, 256)

		var walkErr error
		go func() {
			defer close(jobs)
			walkErr = walk(ctx, root, scopes, matcher, f, skip, existing, opts.Log, jobs, results)
		}()

		var wg sync.WaitGroup
		for range opts.workers() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					select {
					case results <- process(j, f.maxSize, opts.Log):
					case <-ctx.Done():
						return
					}
				}
			}()
		}
		go func() {
			wg.Wait()
			close(results)
		}()

		seen := make(map[string]bool, len(existing))
		w := writer{s: s, rootID: r.ID, stats: &stats, log: opts.Log}
		files := 0
		var writeErr error

		for res := range results {
			if writeErr != nil {
				continue // drain so the other goroutines can exit
			}
			if res.skipped {
				stats.Skipped++
			} else {
				seen[res.rel] = true
				writeErr = w.add(ctx, res)
			}
			if writeErr != nil {
				cancel()
			}
			if opts.Progress != nil {
				files++
				opts.Progress(Progress{Root: root, Files: files, Path: res.rel})
			}
		}

		if writeErr != nil {
			return writeErr
		}
		if walkErr != nil {
			return walkErr
		}
		if err := w.flush(ctx); err != nil {
			return err
		}

		var removed []int64
		for path, st := range existing {
			if !seen[path] {
				removed = append(removed, st.ID)
				opts.Log.Debug().Str("path", path).Msg("removed")
			}
		}
		if err := s.DeleteFiles(ctx, removed); err != nil {
			return err
		}
		stats.Removed = len(removed)

		return s.SetRootIndexedAt(ctx, r.ID, time.Now().Unix())
	})
	if err != nil {
		return stats, err
	}

	// An index is one big transaction, so the WAL can grow as large as the
	// changes. Shrink it now rather than when every process closes the DB.
	if err := db.Checkpoint(ctx); err != nil {
		opts.Log.Warn().Err(err).Msg("checkpoint failed")
	}

	return stats, nil
}

// job is a candidate file that needs reading.
type job struct {
	path, rel, ext string
	info           fs.FileInfo
	old            database.FileState
	known          bool
}

// result is a processed candidate file.
type result struct {
	job
	// unchanged files matched on size and mtime, so were never read.
	unchanged bool
	// skipped files are not text, too large, or unreadable.
	skipped bool
	data    []byte
	hash    string
	symbols []database.Symbol
}

// walk sends the files at and below each of scopes needing a read to jobs.
// Files that are unchanged or too large are decided without reading and
// sent straight to results.
func walk(ctx context.Context, root string, scopes []string, matcher *ignore.Matcher, f filters, skip map[string]bool,
	existing map[string]database.FileState, log zerolog.Logger, jobs chan<- job, results chan<- result) error {

	parents := newParentMatchers(root, matcher, f, log)
	for _, scope := range scopes {
		m, start, ok := parents.forScope(scope)
		if !ok {
			continue
		}
		if err := walkScope(ctx, root, start, m, f, skip, existing, log, jobs, results); err != nil {
			return err
		}
	}
	return nil
}

func walkScope(ctx context.Context, root, start string, matcher *ignore.Matcher, f filters, skip map[string]bool,
	existing map[string]database.FileState, log zerolog.Logger, jobs chan<- job, results chan<- result) error {

	send := func(r result) error {
		select {
		case results <- r:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {

		if err != nil {
			// A changed path that is gone is removed, not an error.
			if path == start && start != root && isNotExist(err) {
				return nil
			}
			log.Warn().Err(err).Str("path", path).Msg("cannot read")
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
			return enterDir(root, rel, matcher, f, log)
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

		info, err := d.Info()
		if err != nil {
			log.Warn().Err(err).Str("path", rel).Msg("cannot stat")
			return send(result{job: job{rel: rel}, skipped: true})
		}

		old, known := existing[rel]
		j := job{path: path, rel: rel, ext: ext, info: info, old: old, known: known}

		if known && old.Size == info.Size() && old.ModTime == info.ModTime().UnixNano() && old.SymbolsVersion == symbols.Version {
			return send(result{job: j, unchanged: true})
		}

		if info.Size() > f.maxSize {
			log.Debug().Str("path", rel).Int64("size", info.Size()).Msg("skipping large file")
			return send(result{job: j, skipped: true})
		}

		select {
		case jobs <- j:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// parentMatchers builds the ignore rules that apply inside a directory,
// remembering them for directories that share parents.
type parentMatchers struct {
	root  string
	base  *ignore.Matcher
	f     filters
	log   zerolog.Logger
	cache map[string]*ignore.Matcher
}

func newParentMatchers(root string, base *ignore.Matcher, f filters, log zerolog.Logger) *parentMatchers {
	return &parentMatchers{root: root, base: base, f: f, log: log, cache: map[string]*ignore.Matcher{}}
}

// forScope returns the rules to walk the slash-separated path scope with,
// and its absolute path. It reports false if a directory above scope is
// ignored, so nothing at scope is indexed. The rules are a fresh copy, as
// walking adds to them.
func (p *parentMatchers) forScope(scope string) (*ignore.Matcher, string, bool) {
	if scope == "" {
		return p.base, p.root, true
	}
	parent := path.Dir(scope)
	if parent == "." {
		parent = ""
	}
	m, ok := p.dir(parent)
	if !ok {
		return nil, "", false
	}
	return m.Clone(), filepath.Join(p.root, filepath.FromSlash(scope)), true
}

// dir returns the rules that apply inside the directory rel, or false if it
// or a directory above it is ignored.
func (p *parentMatchers) dir(rel string) (*ignore.Matcher, bool) {
	if rel == "" {
		return p.base, true
	}
	if m, ok := p.cache[rel]; ok {
		return m, m != nil
	}
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	m, ok := p.dir(parent)
	if ok {
		m = m.Clone()
		ok = enterDir(p.root, rel, m, p.f, p.log) == nil
	}
	if !ok {
		m = nil
	}
	p.cache[rel] = m
	return m, ok
}

// isNotExist reports whether err means a path is gone, including when a
// directory above it was replaced by a file.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// enterDir returns nil if the walk should descend into the directory rel,
// loading its ignore file, or filepath.SkipDir if it is ignored.
func enterDir(root, rel string, matcher *ignore.Matcher, f filters, log zerolog.Logger) error {
	if matcher.Ignored(rel, true) || (f.exclude != nil && f.exclude.Matches(rel, true)) {
		return filepath.SkipDir
	}
	if err := matcher.LoadDir(root, rel); err != nil {
		// Its rules are unknown, so skipping is safer than indexing it.
		log.Warn().Err(err).Str("dir", rel).Msg("cannot read ignore file, skipping directory")
		return filepath.SkipDir
	}
	return nil
}

// Dirs returns the absolute paths of every directory an index of the
// indexed root dir looks in, using the filters saved for it.
func Dirs(ctx context.Context, db *database.DB, dir string, log zerolog.Logger) ([]string, error) {
	return DirsUnder(ctx, db, dir, "", log)
}

// DirsUnder is Dirs limited to sub, an absolute path in the root, and the
// directories below it. It returns none if sub is not a directory or is
// ignored.
func DirsUnder(ctx context.Context, db *database.DB, dir, sub string, log zerolog.Logger) ([]string, error) {

	r, err := LookupRoot(ctx, db, dir)
	if err != nil {
		return nil, err
	}

	var opts Options
	if err := json.Unmarshal([]byte(r.Options), &opts); err != nil {
		return nil, fmt.Errorf("saved options for %s: %w", r.Path, err)
	}
	f, err := compileFilters(opts)
	if err != nil {
		return nil, err
	}

	matcher, err := ignore.NewMatcher(r.Path)
	if err != nil {
		return nil, err
	}

	scope := ""
	if sub != "" {
		scopes := scopesOf(r.Path, []string{sub})
		if len(scopes) == 0 {
			return nil, nil
		}
		scope = scopes[0]
	}
	matcher, start, ok := newParentMatchers(r.Path, matcher, f, log).forScope(scope)
	if !ok {
		return nil, nil
	}

	var dirs []string
	err = filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == start && path != r.Path && isNotExist(err) {
				return nil
			}
			if d != nil && d.IsDir() && path != r.Path {
				return filepath.SkipDir
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != r.Path {
			rel, err := filepath.Rel(r.Path, path)
			if err != nil {
				return err
			}
			if err := enterDir(r.Path, filepath.ToSlash(rel), matcher, f, log); err != nil {
				return err
			}
		}
		dirs = append(dirs, path)
		return nil
	})

	return dirs, err
}

// process reads, sniffs and hashes one file, and finds its symbols unless
// they are already stored for the same content.
func process(j job, maxSize int64, log zerolog.Logger) result {

	data, err := readText(j.path)
	if err != nil {
		log.Warn().Err(err).Str("path", j.rel).Msg("cannot read")
		return result{job: j, skipped: true}
	}

	data = bytes.TrimPrefix(data, utf8BOM)
	// The size can change between stat and read.
	if data == nil || int64(len(data)) > maxSize || !IsText(data) {
		log.Debug().Str("path", j.rel).Msg("skipping non-text file")
		return result{job: j, skipped: true}
	}

	sum := sha256.Sum256(data)
	r := result{job: j, data: data, hash: hex.EncodeToString(sum[:])}

	if j.known && j.old.Hash == r.hash && j.old.SymbolsVersion == symbols.Version {
		return r
	}

	syms, err := symbols.Extract(j.path, data)
	if err != nil {
		log.Debug().Err(err).Str("path", j.rel).Msg("cannot parse")
	}
	r.symbols = syms

	return r
}

// Batches are written when they reach this many files or bytes.
const (
	batchFiles = 500
	batchBytes = 32 << 20
)

// writer applies results to the database, batching inserts because FTS5
// flushes its pending data at the start of every write statement.
type writer struct {
	s       *database.Store
	rootID  int64
	stats   *Stats
	log     zerolog.Logger
	batch   []database.File
	batchSz int
}

func (w *writer) add(ctx context.Context, r result) error {

	if r.unchanged {
		w.stats.Unchanged++
		return nil
	}

	mtime := r.info.ModTime().UnixNano()

	if r.known && r.old.Hash == r.hash {
		w.stats.Unchanged++
		if r.old.SymbolsVersion != symbols.Version {
			return w.s.SetFileSymbols(ctx, r.old.ID, mtime, symbols.Version, r.symbols)
		}
		return w.s.TouchFile(ctx, r.old.ID, mtime)
	}

	if r.known {
		w.stats.Updated++
		w.log.Debug().Str("path", r.rel).Msg("updated")
	} else {
		w.stats.Added++
		w.log.Debug().Str("path", r.rel).Msg("added")
	}

	w.batch = append(w.batch, database.File{
		RootID:  w.rootID,
		Path:    r.rel,
		Ext:     r.ext,
		Size:    r.info.Size(),
		ModTime: mtime,
		Hash:    r.hash,
		Content: string(r.data),

		SymbolsVersion: symbols.Version,
		Symbols:        r.symbols,
	})
	w.batchSz += len(r.data)

	if len(w.batch) >= batchFiles || w.batchSz >= batchBytes {
		return w.flush(ctx)
	}
	return nil
}

func (w *writer) flush(ctx context.Context) error {
	_, err := w.s.UpsertFiles(ctx, w.batch)
	w.batch, w.batchSz = w.batch[:0], 0
	return err
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
