package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"chizu/src/database"
	"chizu/src/symbols"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// writeTree creates files under dir from a map of slash paths to contents.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

func openDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "idx.sqlite"), database.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// indexedPaths returns the sorted relative paths stored for root.
func indexedPaths(t *testing.T, db *database.DB, root string) []string {
	t.Helper()
	r, err := db.GetRoot(t.Context(), root)
	require.NoError(t, err)
	states, err := db.FileStates(t.Context(), r.ID)
	require.NoError(t, err)
	paths := make([]string, 0, len(states))
	for p := range states {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// bumpMtime moves a file's mtime forward so the indexer sees it as changed.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))
}

func TestIndexNewFolder(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()

	writeTree(t, dir, map[string]string{
		"readme.md":            "# Hello\nworld",
		"notes/todo.txt":       "buy milk",
		"notes/deep/x.org":     "* heading",
		"Makefile":             "all:\n\tgo build",
		"image.png":            "\x89PNG\r\n\x1a\n\x00\x00\x00binary",
		"latin1.txt":           "caf\xe9",
		".git/config":          "[core]",
		"node_modules/a.js":    "module.exports = 1",
		"big.txt":              strings.Repeat("a", 2000),
		"private/.chizuignore": "secret.md\n",
		"private/secret.md":    "password",
		"private/public.md":    "fine",
	})

	stats, err := Index(ctx, db, dir, Options{MaxSize: 1000})
	require.NoError(t, err)

	require.Equal(t, []string{
		"Makefile",
		"notes/deep/x.org",
		"notes/todo.txt",
		"private/.chizuignore",
		"private/public.md",
		"readme.md",
	}, indexedPaths(t, db, stats.Root))

	require.Equal(t, 6, stats.Added)
	require.Equal(t, 3, stats.Skipped, "png, latin1 and big.txt")
	require.Zero(t, stats.Updated)
	require.Zero(t, stats.Removed)

	r, err := db.GetRoot(ctx, stats.Root)
	require.NoError(t, err)
	require.NotZero(t, r.IndexedAt)

	f, err := db.GetFileByPath(ctx, r.ID, "readme.md")
	require.NoError(t, err)
	require.Equal(t, "md", f.Ext)
	require.Equal(t, "# Hello\nworld", f.Content)
	require.NotEmpty(t, f.Hash)
}

func TestRootStoredAsAbsolutePath(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a"})

	wd, err := os.Getwd()
	require.NoError(t, err)
	rel, err := filepath.Rel(wd, dir)
	require.NoError(t, err)

	stats, err := Index(t.Context(), db, rel, Options{})
	require.NoError(t, err)

	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, real, stats.Root)
}

func TestIndexMissingDirFails(t *testing.T) {
	db := openDB(t)
	_, err := Index(t.Context(), db, filepath.Join(t.TempDir(), "nope"), Options{})
	require.Error(t, err)

	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	_, err = Index(t.Context(), db, file, Options{})
	require.Error(t, err)
}

func TestReindexIsIncremental(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"keep.md":   "keep",
		"change.md": "before",
		"touch.md":  "same",
		"delete.md": "bye",
	})

	_, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)

	stats, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, Stats{Root: stats.Root, Unchanged: 4}, stats)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "change.md"), []byte("after edit"), 0o644))
	bumpMtime(t, filepath.Join(dir, "change.md"))
	bumpMtime(t, filepath.Join(dir, "touch.md"))
	require.NoError(t, os.Remove(filepath.Join(dir, "delete.md")))
	writeTree(t, dir, map[string]string{"new.md": "fresh"})

	stats, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, Stats{Root: stats.Root, Added: 1, Updated: 1, Unchanged: 2, Removed: 1}, stats)

	r, err := db.GetRoot(ctx, stats.Root)
	require.NoError(t, err)

	changed, err := db.GetFileByPath(ctx, r.ID, "change.md")
	require.NoError(t, err)
	require.Equal(t, "after edit", changed.Content)

	// touch.md had the same content, so only its mtime was recorded.
	touched, err := db.GetFileByPath(ctx, r.ID, "touch.md")
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(dir, "touch.md"))
	require.NoError(t, err)
	require.Equal(t, info.ModTime().UnixNano(), touched.ModTime)
}

func TestFileNoLongerTextIsRemoved(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.txt": "text", "b.txt": "text"})

	_, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("now\x00binary"), 0o644))
	bumpMtime(t, filepath.Join(dir, "a.txt"))

	stats, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Removed)
	require.Equal(t, 1, stats.Skipped)
	require.Equal(t, []string{"b.txt"}, indexedPaths(t, db, stats.Root))
}

func TestExtFilter(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a", "b.TXT": "b", "c.go": "c", "noext": "d"})

	stats, err := Index(t.Context(), db, dir, Options{Exts: []string{"md", ".txt"}})
	require.NoError(t, err)
	require.Equal(t, []string{"a.md", "b.TXT"}, indexedPaths(t, db, stats.Root))
}

func TestIncludeExcludeGlobs(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.md":        "a",
		"b.txt":       "b",
		"drafts/c.md": "c",
		"docs/d.md":   "d",
		"docs/e.rst":  "e",
	})

	stats, err := Index(t.Context(), db, dir, Options{
		Include: []string{"*.md", "*.rst"},
		Exclude: []string{"drafts/"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a.md", "docs/d.md", "docs/e.rst"}, indexedPaths(t, db, stats.Root))
}

func TestRemovingDefaultFromIgnoreFileIndexesThoseFiles(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".git/description": "repo description",
		".chizuignore":     "node_modules/\n",
		"a.md":             "a",
	})

	stats, err := Index(t.Context(), db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, []string{".chizuignore", ".git/description", "a.md"}, indexedPaths(t, db, stats.Root))
}

func TestUTF8BOMStripped(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"bom.txt": "\xef\xbb\xbfhello"})

	stats, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	r, err := db.GetRoot(ctx, stats.Root)
	require.NoError(t, err)
	f, err := db.GetFileByPath(ctx, r.ID, "bom.txt")
	require.NoError(t, err)
	require.Equal(t, "hello", f.Content)
}

func TestIsText(t *testing.T) {
	require.True(t, IsText([]byte("plain ascii\n")))
	require.True(t, IsText([]byte("ünïcödé ✓")))
	require.True(t, IsText([]byte{}))
	require.False(t, IsText([]byte("nul\x00byte")))
	require.False(t, IsText([]byte{0xff, 0xfe, 0x41}))
}

func TestFiltersAreRememberedPerRoot(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a", "b.txt": "b"})

	stats, err := Index(ctx, db, dir, Options{Exts: []string{"md"}, MaxSize: 500})
	require.NoError(t, err)
	require.Equal(t, []string{"a.md"}, indexedPaths(t, db, stats.Root))

	stored, err := StoredOptions(ctx, db, stats.Root)
	require.NoError(t, err)
	require.Equal(t, []string{"md"}, stored.Exts)
	require.EqualValues(t, 500, stored.MaxSize)

	// No filters given: the stored ones are reused.
	_, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, []string{"a.md"}, indexedPaths(t, db, stats.Root))

	// New filters replace the stored ones.
	_, err = Index(ctx, db, dir, Options{Exts: []string{"txt"}})
	require.NoError(t, err)
	require.Equal(t, []string{"b.txt"}, indexedPaths(t, db, stats.Root))

	// ClearFilters drops them.
	_, err = Index(ctx, db, dir, Options{ClearFilters: true})
	require.NoError(t, err)
	require.Equal(t, []string{"a.md", "b.txt"}, indexedPaths(t, db, stats.Root))

	stored, err = StoredOptions(ctx, db, stats.Root)
	require.NoError(t, err)
	require.Empty(t, stored.Exts)
}

func TestIndexAllReindexesEveryRoot(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]string{"a.md": "a"})
	writeTree(t, b, map[string]string{"b.md": "b"})

	_, err := Index(ctx, db, a, Options{})
	require.NoError(t, err)
	_, err = Index(ctx, db, b, Options{})
	require.NoError(t, err)

	writeTree(t, a, map[string]string{"new.md": "new"})

	all, err := IndexAll(ctx, db, Options{})
	require.NoError(t, err)
	require.Len(t, all, 2)

	added := 0
	for _, s := range all {
		added += s.Added
	}
	require.Equal(t, 1, added)
}

func TestIndexAllReportsMissingRoots(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	a := filepath.Join(t.TempDir(), "gone")
	b := t.TempDir()
	writeTree(t, a, map[string]string{"a.md": "a"})
	writeTree(t, b, map[string]string{"b.md": "b"})

	_, err := Index(ctx, db, a, Options{})
	require.NoError(t, err)
	_, err = Index(ctx, db, b, Options{})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(a))

	all, err := IndexAll(ctx, db, Options{})
	require.Error(t, err)
	require.Len(t, all, 1, "the remaining root is still indexed")
}

func TestRemove(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a"})

	stats, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)

	require.NoError(t, Remove(ctx, db, dir))
	_, err = db.GetRoot(ctx, stats.Root)
	require.ErrorIs(t, err, database.ErrNotFound)

	require.ErrorIs(t, Remove(ctx, db, dir), database.ErrNotFound)

	// A root whose directory was deleted can still be removed by its stored path.
	gone := filepath.Join(t.TempDir(), "gone")
	writeTree(t, gone, map[string]string{"a.md": "a"})
	stats, err = Index(ctx, db, gone, Options{})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(gone))
	require.NoError(t, Remove(ctx, db, stats.Root))
}

func TestSkipPaths(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a", "my.db": "pretend database"})

	stats, err := Index(t.Context(), db, dir, Options{SkipPaths: []string{filepath.Join(dir, "my.db")}})
	require.NoError(t, err)
	require.Equal(t, []string{"a.md"}, indexedPaths(t, db, stats.Root))
	require.Zero(t, stats.Skipped)
}

func TestBinaryAfterSniffWindowIsSkipped(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	// Like git, only the first sniffLen bytes are checked for NUL.
	writeTree(t, dir, map[string]string{
		"late-nul.txt":  strings.Repeat("a", sniffLen+10) + "\x00",
		"early-nul.bin": "\x00" + strings.Repeat("a", sniffLen*2),
		"exact.txt":     strings.Repeat("b", sniffLen),
	})

	stats, err := Index(t.Context(), db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, []string{"exact.txt", "late-nul.txt"}, indexedPaths(t, db, stats.Root))
	require.Equal(t, 1, stats.Skipped)
}

func TestProgressReported(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "a", "b.md": "b", "c.md": "c", "d.bin": "\x00"})

	for range 2 { // first index and re-index both report
		var events []Progress
		stats, err := Index(t.Context(), db, dir, Options{Progress: func(p Progress) { events = append(events, p) }})
		require.NoError(t, err)
		require.Len(t, events, 4, "one event per candidate file, skipped ones included")
		for i, e := range events {
			require.Equal(t, stats.Root, e.Root)
			require.Equal(t, i+1, e.Files)
			require.NotEmpty(t, e.Path)
		}
	}
}

func TestUnreadableIgnoreFileSkipsThatDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.md":                "a",
		"locked/.chizuignore": "x",
		"locked/b.md":         "b",
	})
	require.NoError(t, os.Chmod(filepath.Join(dir, "locked", ".chizuignore"), 0))
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "locked", ".chizuignore"), 0o644) })

	stats, err := Index(t.Context(), db, dir, Options{})
	require.NoError(t, err, "one unreadable ignore file must not abort the run")
	require.Equal(t, []string{"a.md"}, indexedPaths(t, db, stats.Root), "its rules are unknown, so the directory is skipped")
}

func TestWorkerCountDoesNotChangeResults(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	files := map[string]string{}
	for i := range 1200 {
		switch i % 4 {
		case 0:
			files[fmt.Sprintf("bin/%d.dat", i)] = "\x00binary"
		default:
			files[fmt.Sprintf("d%d/f%d.md", i%7, i)] = fmt.Sprintf("note %d word%d", i, i%13)
		}
	}
	writeTree(t, dir, files)

	var want []string
	for _, workers := range []int{1, 3, 16} {
		db := openDB(t)
		var progress int
		stats, err := Index(ctx, db, dir, Options{Workers: workers, Progress: func(Progress) { progress++ }})
		require.NoError(t, err)
		require.Equal(t, 900, stats.Added)
		require.Equal(t, 300, stats.Skipped)
		require.Equal(t, 1200, progress)

		got := indexedPaths(t, db, stats.Root)
		if want == nil {
			want = got
		}
		require.Equal(t, want, got, "workers=%d", workers)

		// Re-indexing with changes still works with many workers.
		require.NoError(t, os.Remove(filepath.Join(dir, "d1/f1.md")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "d2/f2.md"), []byte("changed"), 0o644))
		bumpMtime(t, filepath.Join(dir, "d2/f2.md"))
		stats, err = Index(ctx, db, dir, Options{Workers: workers})
		require.NoError(t, err)
		require.Equal(t, Stats{Root: stats.Root, Updated: 1, Removed: 1, Unchanged: 898, Skipped: 300}, stats)

		// Put the tree back for the next worker count.
		writeTree(t, dir, map[string]string{"d1/f1.md": files["d1/f1.md"], "d2/f2.md": files["d2/f2.md"]})
	}
}

func TestIndexTruncatesWALWhileAnotherProcessHasItOpen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "idx.sqlite")
	db, err := database.Open(ctx, path, database.Options{})
	require.NoError(t, err)
	defer db.Close()

	other, err := database.Open(ctx, path, database.Options{})
	require.NoError(t, err)
	defer other.Close()

	dir := t.TempDir()
	files := map[string]string{}
	for i := range 200 {
		files[fmt.Sprintf("f%d.txt", i)] = strings.Repeat(fmt.Sprintf("word%d ", i), 2000)
	}
	writeTree(t, dir, files)

	_, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)

	st, err := os.Stat(path + "-wal")
	if err == nil {
		require.Zero(t, st.Size(), "WAL is truncated after indexing")
	}
}

// symbolNames returns "kind name" for each symbol stored for a file.
func symbolNames(t *testing.T, db *database.DB, root, path string) []string {
	t.Helper()
	r, err := db.GetRoot(t.Context(), root)
	require.NoError(t, err)
	f, err := db.GetFileByPath(t.Context(), r.ID, path)
	require.NoError(t, err)
	syms, err := db.FileSymbols(t.Context(), f.ID)
	require.NoError(t, err)
	names := []string{}
	for _, s := range syms {
		names = append(names, s.Kind+" "+s.Name)
	}
	return names
}

func symbolCount(t *testing.T, db *database.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.GetContext(t.Context(), &n, `SELECT count(*) FROM symbols`))
	return n
}

func TestIndexSymbols(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()

	writeTree(t, dir, map[string]string{
		"main.go":   "package main\n\nfunc main() {}\n\nfunc helper() {}\n",
		"lib.py":    "class Thing:\n    def run(self):\n        pass\n",
		"readme.md": "# func notCode()",
	})

	stats, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, []string{"function main", "function helper"}, symbolNames(t, db, stats.Root, "main.go"))
	require.Equal(t, []string{"class Thing", "method run"}, symbolNames(t, db, stats.Root, "lib.py"))
	require.Empty(t, symbolNames(t, db, stats.Root, "readme.md"))

	// A changed file's symbols are replaced.
	writeTree(t, dir, map[string]string{"main.go": "package main\n\nfunc main() {}\n\ntype Config struct{}\n"})
	bumpMtime(t, filepath.Join(dir, "main.go"))
	_, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, []string{"function main", "struct Config"}, symbolNames(t, db, stats.Root, "main.go"))

	// Touching a file without changing it keeps its symbols.
	bumpMtime(t, filepath.Join(dir, "lib.py"))
	stats, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, 3, stats.Unchanged)
	require.Equal(t, []string{"class Thing", "method run"}, symbolNames(t, db, stats.Root, "lib.py"))

	// A removed file's symbols go with it.
	require.NoError(t, os.Remove(filepath.Join(dir, "lib.py")))
	_, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, 2, symbolCount(t, db))

	require.NoError(t, Remove(ctx, db, dir))
	require.Zero(t, symbolCount(t, db))
}

func TestIndexFillsInSymbolsForOldFiles(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"main.go": "package main\n\nfunc main() {}\n", "a.txt": "text"})

	stats, err := Index(ctx, db, dir, Options{})
	require.NoError(t, err)

	// As if indexed by a chizu that did not parse symbols.
	_, err = db.ExecContext(ctx, `DELETE FROM symbols`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE files SET symbols_version = 0`)
	require.NoError(t, err)

	stats, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, 2, stats.Unchanged, "content is not rewritten")
	require.Zero(t, stats.Updated)
	require.Equal(t, []string{"function main"}, symbolNames(t, db, stats.Root, "main.go"))

	r, err := db.GetRoot(ctx, stats.Root)
	require.NoError(t, err)
	states, err := db.FileStates(ctx, r.ID)
	require.NoError(t, err)
	for _, st := range states {
		require.Equal(t, symbols.Version, st.SymbolsVersion, st.Path)
	}
}

func TestDirs(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	writeTree(t, dir, map[string]string{
		"a/b/c.md":          "x",
		"skip/d.md":         "x",
		"node_modules/e.js": "x",
		"keep/.chizuignore": "inner/\n",
		"keep/inner/f.md":   "x",
	})

	_, err = Dirs(ctx, db, dir, zerolog.Nop())
	require.Error(t, err, "not indexed")

	_, err = Index(ctx, db, dir, Options{Exclude: []string{"skip/"}})
	require.NoError(t, err)

	dirs, err := Dirs(ctx, db, dir, zerolog.Nop())
	require.NoError(t, err)
	for i := range dirs {
		dirs[i], err = filepath.Rel(dir, dirs[i])
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []string{".", "a", filepath.Join("a", "b"), "keep"}, dirs)
}

func TestDirsUnder(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	writeTree(t, dir, map[string]string{
		"a/b/c.md":          "x",
		"a/node_modules/e":  "x",
		"keep/.chizuignore": "inner/\n",
		"keep/inner/x/f.md": "x",
		"file.md":           "x",
	})
	_, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)

	rel := func(sub string) []string {
		t.Helper()
		dirs, err := DirsUnder(ctx, db, dir, filepath.Join(dir, filepath.FromSlash(sub)), zerolog.Nop())
		require.NoError(t, err)
		out := []string{}
		for _, d := range dirs {
			r, err := filepath.Rel(dir, d)
			require.NoError(t, err)
			out = append(out, filepath.ToSlash(r))
		}
		return out
	}

	require.ElementsMatch(t, []string{"a", "a/b"}, rel("a"))
	require.Empty(t, rel("a/node_modules"))
	require.Empty(t, rel("keep/inner/x"), "below an ignored directory")
	require.Empty(t, rel("file.md"))
	require.Empty(t, rel("gone"))
}

func TestIndexPaths(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	p := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }

	writeTree(t, dir, map[string]string{
		"a.md":          "alpha",
		"b.md":          "beta",
		"sub/c.md":      "gamma",
		"sub/d/e.md":    "epsilon",
		"sub-x/f.md":    "phi",
		"sub2/g.md":     "gee",
		"other/h.md":    "aitch",
		"other/i.md":    "eye",
		"vendor/j.go":   "package j",
		"keep/k.md":     "kay",
		"keep/l.md":     "ell",
		"keep/sub/m.md": "em",
	})
	_, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)

	content := func(rel string) string {
		t.Helper()
		r, err := db.GetRoot(ctx, dir)
		require.NoError(t, err)
		f, err := db.GetFileByPath(ctx, r.ID, rel)
		if err != nil {
			return ""
		}
		return f.Content
	}

	// Only the named paths are looked at: b.md changed too, but is not named.
	writeTree(t, dir, map[string]string{"a.md": "alpha two", "b.md": "beta two", "new.md": "new"})
	bumpMtime(t, p("a.md"))
	bumpMtime(t, p("b.md"))
	stats, err := IndexPaths(ctx, db, dir, []string{p("a.md"), "new.md", p("missing.md")}, Options{})
	require.NoError(t, err)
	require.Equal(t, Stats{Root: dir, Added: 1, Updated: 1}, stats)
	require.Equal(t, "alpha two", content("a.md"))
	require.Equal(t, "beta", content("b.md"))
	require.Equal(t, "new", content("new.md"))

	// A deleted directory takes everything below it, but not siblings
	// whose names start the same.
	require.NoError(t, os.RemoveAll(p("sub")))
	stats, err = IndexPaths(ctx, db, dir, []string{p("sub"), p("sub/c.md")}, Options{})
	require.NoError(t, err)
	require.Equal(t, 2, stats.Removed)
	require.Contains(t, indexedPaths(t, db, dir), "sub-x/f.md")
	require.Contains(t, indexedPaths(t, db, dir), "sub2/g.md")

	// A new directory is indexed with everything in it.
	writeTree(t, dir, map[string]string{"made/n.md": "en", "made/deep/o.md": "oh"})
	stats, err = IndexPaths(ctx, db, dir, []string{p("made")}, Options{})
	require.NoError(t, err)
	require.Equal(t, 2, stats.Added)

	// Files under ignored directories stay out.
	writeTree(t, dir, map[string]string{"vendor/q.go": "package q", "node_modules/r.js": "r"})
	stats, err = IndexPaths(ctx, db, dir, []string{p("vendor/q.go"), p("node_modules")}, Options{})
	require.NoError(t, err)
	require.Zero(t, stats.Added)

	// An ignore file stands for its directory.
	writeTree(t, dir, map[string]string{"keep/.chizuignore": "l.md\nsub/\n"})
	stats, err = IndexPaths(ctx, db, dir, []string{p("keep/.chizuignore")}, Options{})
	require.NoError(t, err)
	require.Equal(t, 2, stats.Removed)
	require.Equal(t, 1, stats.Unchanged)

	// Paths outside the root do nothing.
	stats, err = IndexPaths(ctx, db, dir, []string{filepath.Dir(dir)}, Options{})
	require.NoError(t, err)
	require.Equal(t, Stats{Root: dir}, stats)

	// The result is what a full index gives, apart from b.md, which was
	// never named.
	stats, err = Index(ctx, db, dir, Options{})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Updated)
	require.Zero(t, stats.Added+stats.Removed)
}
