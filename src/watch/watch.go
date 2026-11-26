// Package watch keeps indexed directories up to date as their files change.
//
// Changes are indexed with indexer.IndexPaths, limited to the paths file
// system events named, so a change costs the same in a large root as in a
// small one and the index is what a manual re-index would have produced.
// When events were dropped, the whole root is indexed again.
package watch

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"chizu/src/database"
	"chizu/src/ignore"
	"chizu/src/indexer"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
)

const (
	// DefaultDebounce is how long to wait for changes to stop before re-indexing.
	DefaultDebounce = 500 * time.Millisecond
	// maxDelayFactor caps the wait at this many debounces after the first
	// change, so a file that is written constantly still gets indexed.
	maxDelayFactor = 10
)

var (
	// bufferSize is the buffer each watched directory gets on Windows.
	// Reduce the size for less memory usage.
	bufferSize = 16 * 1024
)

func init() {

	// let the user override this but it's not public api
	envOverride := os.Getenv("WINDOWS_FSNOTIFY_BUF_SIZE")

	if envOverride != "" {
		if s, err := strconv.Atoi(envOverride); err == nil {
			bufferSize = s
		}
	}
}

// rootsInterval is how often the database is checked for roots added or
// removed by another command, when watching every root.
var rootsInterval = 5 * time.Second

type Options struct {
	// Index is used for every index run. Its filters are saved with each root
	// on the first run, as with chizu index, and reused after that.
	Index indexer.Options
	// Debounce is how long to wait after the last change before
	// re-indexing (DefaultDebounce if zero).
	Debounce time.Duration
	// Indexed, if set, is called after every index run with whether it was
	// the first run for that root.
	Indexed func(stats indexer.Stats, took time.Duration, initial bool, err error)
	Log     zerolog.Logger
}

type watcher struct {
	db   *database.DB
	opts Options
	fs   *fsnotify.Watcher
	skip map[string]bool
	// roots are the watched roots; dirs maps each watched directory to its root.
	roots map[string]bool
	dirs  map[string]string
	// limitWarned is set once the OS watch limit has been reported.
	limitWarned bool
}

// Run indexes dirs and then re-indexes them whenever files in them change,
// until ctx is done. With no dirs it watches every root in the database,
// including roots added or removed while it runs.
func Run(ctx context.Context, db *database.DB, dirs []string, opts Options) error {

	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	w := &watcher{
		db:    db,
		opts:  opts,
		fs:    fsw,
		skip:  indexer.SkipSet(opts.Index.SkipPaths),
		roots: map[string]bool{},
		dirs:  map[string]string{},
	}

	all := len(dirs) == 0
	if all {
		roots, err := db.ListRoots(ctx)
		if err != nil {
			return err
		}
		for _, r := range roots {
			dirs = append(dirs, r.Path)
		}
	}

	// changed holds the paths changed in each root since the last run;
	// a nil set means the whole root.
	changed := map[string]map[string]bool{}

	var errs []error
	for _, dir := range dirs {
		root, err := indexer.ResolveRoot(dir)
		if err == nil {
			err = w.index(ctx, root, true)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
			continue
		}
		w.roots[root] = true
		w.sync(ctx, root, "")
		// Files changed while it was indexed, before it was watched, are
		// picked up by one more (usually empty) run.
		changed[root] = nil
	}
	if !all && len(w.roots) == 0 {
		return errors.Join(errs...)
	}

	// Later runs reuse the filters the first run saved.
	w.opts.Index.ClearFilters = false

	var rootsTick <-chan time.Time
	if all {
		t := time.NewTicker(rootsInterval)
		defer t.Stop()
		rootsTick = t.C
	}

	var timer *time.Timer
	var fire <-chan time.Time
	var first time.Time
	if len(changed) > 0 {
		timer = time.NewTimer(0)
		fire = timer.C
	}

	// schedule waits for changes to settle before the next run, but not forever.
	schedule := func() {
		now := time.Now()
		if timer == nil {
			first = now
			timer = time.NewTimer(opts.Debounce)
			fire = timer.C
		} else if now.Sub(first) < opts.Debounce*maxDelayFactor {
			timer.Reset(opts.Debounce)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			root := w.rootOf(ev)
			if root == "" {
				continue
			}
			if paths, ok := changed[root]; !ok {
				changed[root] = map[string]bool{ev.Name: true}
			} else if paths != nil {
				paths[ev.Name] = true
			}
			w.watchChange(ctx, root, ev)
			schedule()

		case <-fire:
			timer, fire = nil, nil
			for root, paths := range changed {
				delete(changed, root)
				if !w.roots[root] {
					continue
				}
				if paths != nil {
					w.indexPaths(ctx, root, paths)
					continue
				}
				if err := w.index(ctx, root, false); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					continue
				}
				w.sync(ctx, root, "")
			}
			if ctx.Err() != nil {
				return nil
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			// Events were dropped, so any root may have changed.
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.opts.Log.Debug().Err(err).Msg("re-indexing every root")
				for root := range w.roots {
					changed[root] = nil
				}
				schedule()
				continue
			}
			w.opts.Log.Warn().Err(err).Msg("file watcher error")

		case <-rootsTick:
			w.syncRoots(ctx)
		}
	}
}

// index runs the indexer on all of one root and reports the result.
func (w *watcher) index(ctx context.Context, root string, initial bool) error {
	start := time.Now()
	stats, err := indexer.Index(ctx, w.db, root, w.opts.Index)
	return w.report(ctx, root, stats, start, initial, err)
}

// indexPaths runs the indexer on the changed paths in one root and reports
// the result.
func (w *watcher) indexPaths(ctx context.Context, root string, paths map[string]bool) {
	start := time.Now()
	stats, err := indexer.IndexPaths(ctx, w.db, root, slices.Collect(maps.Keys(paths)), w.opts.Index)
	w.report(ctx, root, stats, start, false, err)
}

func (w *watcher) report(ctx context.Context, root string, stats indexer.Stats, start time.Time, initial bool, err error) error {
	if err != nil && ctx.Err() == nil {
		w.opts.Log.Warn().Err(err).Str("root", root).Msg("index failed")
	}
	if w.opts.Indexed != nil && ctx.Err() == nil {
		w.opts.Indexed(stats, time.Since(start), initial, err)
	}
	return err
}

// rootOf returns the watched root an event belongs to, or "" if the event
// does not need a re-index.
func (w *watcher) rootOf(ev fsnotify.Event) string {

	// Only metadata changed, or it is the database itself being written.
	if ev.Op == fsnotify.Chmod || w.skip[ev.Name] {
		return ""
	}

	// Windows reports a directory as written when its entries change,
	// even for ignored directories. Those entries get their own events.
	if ev.Op == fsnotify.Write {
		if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
			return ""
		}
	}

	if root, ok := w.dirs[ev.Name]; ok {
		return root
	}
	return w.dirs[filepath.Dir(ev.Name)]
}

// watchChange keeps the watched directories in step with a change: a new
// directory is watched now, before the run that indexes it, so files
// written into it in the meantime are not missed; a directory moved or
// deleted is forgotten; and a changed ignore file can include or exclude
// directories below it.
func (w *watcher) watchChange(ctx context.Context, root string, ev fsnotify.Event) {

	switch {
	case filepath.Base(ev.Name) == ignore.FileName:
		w.sync(ctx, root, filepath.Dir(ev.Name))
	case ev.Has(fsnotify.Create):
		if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
			w.sync(ctx, root, ev.Name)
		}
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		if _, ok := w.dirs[ev.Name]; ok {
			w.sync(ctx, root, ev.Name)
		}
	}
}

// sync watches every directory the indexer looks in at and below sub in
// root ("" for all of it), and stops watching the rest.
func (w *watcher) sync(ctx context.Context, root, sub string) {

	dirs, err := indexer.DirsUnder(ctx, w.db, root, sub, w.opts.Log)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.opts.Log.Warn().Err(err).Str("root", root).Msg("cannot list directories to watch")
		return
	}

	want := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		want[d] = true
		if _, ok := w.dirs[d]; ok {
			continue
		}
		if err := w.fs.AddWith(d, fsnotify.WithBufferSize(bufferSize)); err != nil {
			w.addFailed(d, err)
			continue
		}
		w.dirs[d] = root
	}

	// Forget directories that were deleted, moved or are now ignored.
	for d, r := range w.dirs {
		if r == root && !want[d] && within(d, sub) {
			w.fs.Remove(d)
			delete(w.dirs, d)
		}
	}
}

// within reports whether path is dir or below it, or dir is "".
func within(path, dir string) bool {
	if dir == "" || path == dir {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *watcher) addFailed(dir string, err error) {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EMFILE) {
		if !w.limitWarned {
			w.limitWarned = true
			w.opts.Log.Warn().Err(err).Str("dir", dir).
				Msg("hit the OS limit on watched directories; changes in some directories will not be noticed (on Linux raise fs.inotify.max_user_watches)")
		}
		return
	}
	w.opts.Log.Debug().Err(err).Str("dir", dir).Msg("cannot watch directory")
}

// syncRoots starts watching roots added to the database since the last
// check and stops watching removed ones.
func (w *watcher) syncRoots(ctx context.Context) {

	roots, err := w.db.ListRoots(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.opts.Log.Warn().Err(err).Msg("cannot list roots")
		return
	}

	current := map[string]bool{}
	for _, r := range roots {
		current[r.Path] = true
		if !w.roots[r.Path] {
			w.roots[r.Path] = true
			w.sync(ctx, r.Path, "")
		}
	}

	for root := range w.roots {
		if current[root] {
			continue
		}
		delete(w.roots, root)
		for d, r := range w.dirs {
			if r == root {
				w.fs.Remove(d)
				delete(w.dirs, d)
			}
		}
	}
}
