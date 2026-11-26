package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"chizu/src/database"
	"chizu/src/indexer"

	"github.com/stretchr/testify/require"
)

const wait = 5 * time.Second

type run struct {
	stats   indexer.Stats
	initial bool
	err     error
}

type fixture struct {
	db     *database.DB
	dbPath string
	mu     sync.Mutex
	runs   []run
	done   chan error
}

// start runs Run in the background until the test ends.
func start(t *testing.T, dbPath string, dirs ...string) *fixture {
	t.Helper()

	db, err := database.Open(t.Context(), dbPath, database.Options{})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{db: db, dbPath: dbPath, done: make(chan error, 1)}

	go func() {
		f.done <- Run(ctx, db, dirs, Options{
			Index:    indexer.Options{SkipPaths: []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"}},
			Debounce: 20 * time.Millisecond,
			Indexed: func(stats indexer.Stats, _ time.Duration, initial bool, err error) {
				f.mu.Lock()
				f.runs = append(f.runs, run{stats, initial, err})
				f.mu.Unlock()
			},
		})
	}()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-f.done)
		db.Close()
	})
	return f
}

// settle waits until no index run has happened for a while and returns
// how many there were. It fails if runs keep happening.
func (f *fixture) settle(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		n := f.runCount()
		time.Sleep(200 * time.Millisecond)
		if f.runCount() == n {
			return n
		}
	}
	t.Fatal("index runs did not stop")
	return 0
}

func (f *fixture) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// paths returns the indexed relative paths under root.
func (f *fixture) paths(t *testing.T, root string) map[string]bool {
	r, err := f.db.GetRoot(t.Context(), root)
	if err != nil {
		return nil
	}
	states, err := f.db.FileStates(t.Context(), r.ID)
	require.NoError(t, err)
	out := map[string]bool{}
	for p := range states {
		out[p] = true
	}
	return out
}

func (f *fixture) content(t *testing.T, root, path string) string {
	r, err := f.db.GetRoot(t.Context(), root)
	require.NoError(t, err)
	file, err := f.db.GetFileByPath(t.Context(), r.ID, path)
	if err != nil {
		return ""
	}
	return file.Content
}

func tempDir(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestWatch(t *testing.T) {
	dir := tempDir(t)
	write(t, filepath.Join(dir, "a.md"), "alpha")
	write(t, filepath.Join(dir, "node_modules", "x.js"), "ignored")

	f := start(t, filepath.Join(t.TempDir(), "w.sqlite"), dir)
	require.Eventually(t, func() bool { return f.runCount() >= 1 }, wait, 10*time.Millisecond)
	f.mu.Lock()
	first := f.runs[0]
	f.mu.Unlock()
	require.True(t, first.initial)
	require.NoError(t, first.err)
	require.Equal(t, 1, first.stats.Added)

	// Added, changed and deleted files.
	write(t, filepath.Join(dir, "b.md"), "beta")
	require.Eventually(t, func() bool { return f.paths(t, dir)["b.md"] }, wait, 10*time.Millisecond)

	write(t, filepath.Join(dir, "a.md"), "alpha two")
	require.Eventually(t, func() bool { return f.content(t, dir, "a.md") == "alpha two" }, wait, 10*time.Millisecond)

	require.NoError(t, os.Remove(filepath.Join(dir, "b.md")))
	require.Eventually(t, func() bool { return !f.paths(t, dir)["b.md"] }, wait, 10*time.Millisecond)

	// A new directory is indexed, and then watched for later changes.
	write(t, filepath.Join(dir, "sub", "c.md"), "gamma")
	require.Eventually(t, func() bool { return f.paths(t, dir)["sub/c.md"] }, wait, 10*time.Millisecond)
	write(t, filepath.Join(dir, "sub", "deeper", "d.md"), "delta")
	require.Eventually(t, func() bool { return f.paths(t, dir)["sub/deeper/d.md"] }, wait, 10*time.Millisecond)
	write(t, filepath.Join(dir, "sub", "deeper", "e.md"), "epsilon")
	require.Eventually(t, func() bool { return f.paths(t, dir)["sub/deeper/e.md"] }, wait, 10*time.Millisecond)

	// Ignored directories are not watched.
	runs := f.settle(t)
	write(t, filepath.Join(dir, "node_modules", "y.js"), "ignored")
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, runs, f.runCount())
	require.False(t, f.paths(t, dir)["node_modules/y.js"])
}

func TestWatchDatabaseInsideRoot(t *testing.T) {
	dir := tempDir(t)
	write(t, filepath.Join(dir, "a.md"), "alpha")

	// Writing the database must not set off another index run, which would
	// write the database again, forever.
	f := start(t, filepath.Join(dir, "custom-name.sqlite"), dir)
	require.Eventually(t, func() bool { return f.runCount() >= 1 }, wait, 10*time.Millisecond)
	f.settle(t)

	write(t, filepath.Join(dir, "b.md"), "beta")
	require.Eventually(t, func() bool { return f.paths(t, dir)["b.md"] }, wait, 10*time.Millisecond)
	f.settle(t)
	require.Equal(t, map[string]bool{"a.md": true, "b.md": true}, f.paths(t, dir))
}

// The database path can name the root through a symlink (a working
// directory under a symlink, or a Windows drive letter in another case),
// while events come for the resolved root.
func TestWatchDatabaseInsideRootThroughSymlink(t *testing.T) {
	dir := tempDir(t)
	write(t, filepath.Join(dir, "a.md"), "alpha")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("cannot create symlinks:", err) // Windows, without developer mode
	}

	f := start(t, filepath.Join(link, ".chizu.sqlite"), link)
	require.Eventually(t, func() bool { return f.runCount() >= 1 }, wait, 10*time.Millisecond)
	f.settle(t)
	require.Equal(t, map[string]bool{"a.md": true}, f.paths(t, dir))
}

func TestWatchEveryRoot(t *testing.T) {
	old := rootsInterval
	rootsInterval = 20 * time.Millisecond
	t.Cleanup(func() { rootsInterval = old })

	a, b := tempDir(t), tempDir(t)
	write(t, filepath.Join(a, "a.md"), "alpha")
	write(t, filepath.Join(b, "b.md"), "beta")

	dbPath := filepath.Join(t.TempDir(), "w.sqlite")
	db, err := database.Open(t.Context(), dbPath, database.Options{})
	require.NoError(t, err)
	_, err = indexer.Index(t.Context(), db, a, indexer.Options{})
	require.NoError(t, err)

	f := start(t, dbPath)
	require.Eventually(t, func() bool { return f.runCount() >= 1 }, wait, 10*time.Millisecond)

	write(t, filepath.Join(a, "a2.md"), "alpha two")
	require.Eventually(t, func() bool { return f.paths(t, a)["a2.md"] }, wait, 10*time.Millisecond)

	// A root indexed by another command is picked up.
	_, err = indexer.Index(t.Context(), db, b, indexer.Options{})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	write(t, filepath.Join(b, "b2.md"), "beta two")
	require.Eventually(t, func() bool { return f.paths(t, b)["b2.md"] }, wait, 10*time.Millisecond)

	// A removed root is no longer watched.
	require.NoError(t, indexer.Remove(t.Context(), db, b))
	time.Sleep(100 * time.Millisecond)
	write(t, filepath.Join(b, "b3.md"), "beta three")
	time.Sleep(200 * time.Millisecond)
	require.Nil(t, f.paths(t, b))
	db.Close()
}

func TestWatchMissingDir(t *testing.T) {
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "w.sqlite"), database.Options{})
	require.NoError(t, err)
	defer db.Close()

	err = Run(t.Context(), db, []string{filepath.Join(t.TempDir(), "nope")}, Options{})
	require.Error(t, err)
}

// lastRun returns the latest index run.
func (f *fixture) lastRun() run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[len(f.runs)-1]
}

func TestWatchIndexesOnlyChangedPaths(t *testing.T) {
	dir := tempDir(t)
	for i := range 20 {
		write(t, filepath.Join(dir, "many", fmt.Sprintf("%d.md", i)), "same")
	}
	write(t, filepath.Join(dir, "a.md"), "alpha")

	f := start(t, filepath.Join(t.TempDir(), "w.sqlite"), dir)
	require.Eventually(t, func() bool { return f.runCount() >= 1 }, wait, 10*time.Millisecond)
	f.settle(t)

	// The other 20 files are not looked at.
	write(t, filepath.Join(dir, "a.md"), "alpha two")
	require.Eventually(t, func() bool { return f.content(t, dir, "a.md") == "alpha two" }, wait, 10*time.Millisecond)
	f.settle(t)
	last := f.lastRun()
	require.NoError(t, last.err)
	require.Equal(t, 1, last.stats.Updated)
	require.Zero(t, last.stats.Unchanged)

	// A moved directory is removed and indexed under its new name, and
	// the new name is watched.
	require.NoError(t, os.Rename(filepath.Join(dir, "many"), filepath.Join(dir, "moved")))
	require.Eventually(t, func() bool {
		p := f.paths(t, dir)
		return !p["many/0.md"] && p["moved/0.md"]
	}, wait, 10*time.Millisecond)
	write(t, filepath.Join(dir, "moved", "new.md"), "new")
	require.Eventually(t, func() bool { return f.paths(t, dir)["moved/new.md"] }, wait, 10*time.Millisecond)

	// A changed ignore file applies to its directory.
	write(t, filepath.Join(dir, "moved", ".chizuignore"), "1*.md\n")
	require.Eventually(t, func() bool {
		p := f.paths(t, dir)
		return !p["moved/1.md"] && !p["moved/15.md"] && p["moved/2.md"]
	}, wait, 10*time.Millisecond)

	// An ignore file that ignores a directory stops it being watched.
	write(t, filepath.Join(dir, "sub", "x.md"), "x")
	require.Eventually(t, func() bool { return f.paths(t, dir)["sub/x.md"] }, wait, 10*time.Millisecond)
	write(t, filepath.Join(dir, ".chizuignore"), "sub/\n")
	require.Eventually(t, func() bool { return !f.paths(t, dir)["sub/x.md"] }, wait, 10*time.Millisecond)
	runs := f.settle(t)
	write(t, filepath.Join(dir, "sub", "y.md"), "y")
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, runs, f.runCount())
}
