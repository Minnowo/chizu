package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"chizu/src/database"

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
