// Package watch keeps indexed directories up to date as their files change.
//
// There is one way to update the index: indexer.Index. File system events
// only decide when to run it and for which root, so a watched index is
// always what a manual re-index would have produced.
package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"chizu/src/database"
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

	dirty := map[string]bool{}

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
		w.sync(ctx, root)
		// Files changed while it was indexed, before it was watched, are
		// picked up by one more (usually empty) run.
		dirty[root] = true
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
	if len(dirty) > 0 {
		timer = time.NewTimer(0)
		fire = timer.C
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
			dirty[root] = true

			// Watch a new directory now, before the run that indexes it, so
			// files written into it in the meantime are not missed.
			if ev.Has(fsnotify.Create) {
				if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
					w.sync(ctx, root)
				}
			}

			// Wait for changes to settle, but not forever.
			now := time.Now()
			if timer == nil {
				first = now
				timer = time.NewTimer(opts.Debounce)
				fire = timer.C
			} else if now.Sub(first) < opts.Debounce*maxDelayFactor {
				timer.Reset(opts.Debounce)
			}

		case <-fire:
			timer, fire = nil, nil
			for root := range dirty {
				delete(dirty, root)
				if !w.roots[root] {
					continue
				}
				if err := w.index(ctx, root, false); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					continue
				}
				w.sync(ctx, root)
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.opts.Log.Warn().Err(err).Msg("file watcher error")

		case <-rootsTick:
			w.syncRoots(ctx)
		}
	}
}

// index runs the indexer on one root and reports the result.
func (w *watcher) index(ctx context.Context, root string, initial bool) error {

	start := time.Now()
	stats, err := indexer.Index(ctx, w.db, root, w.opts.Index)
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

	if root, ok := w.dirs[ev.Name]; ok {
		return root
	}
	return w.dirs[filepath.Dir(ev.Name)]
}

// sync watches every directory the indexer looks in under root, so new
// directories are picked up after the index run that found them. Deleted
// directories drop out of the OS watch by themselves.
func (w *watcher) sync(ctx context.Context, root string) {

	dirs, err := indexer.Dirs(ctx, w.db, root, w.opts.Log)
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
		if err := w.fs.Add(d); err != nil {
			w.addFailed(d, err)
			continue
		}
		w.dirs[d] = root
	}

	// Forget directories that were deleted or are now ignored.
	for d, r := range w.dirs {
		if r == root && !want[d] {
			w.fs.Remove(d)
			delete(w.dirs, d)
		}
	}
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
			w.sync(ctx, r.Path)
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
