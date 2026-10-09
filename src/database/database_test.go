package database

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T, opts Options) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "test.sqlite"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// matchCount returns how many files the raw FTS5 query matches.
func matchCount(t *testing.T, db *DB, query string) int {
	t.Helper()
	var n int
	require.NoError(t, db.GetContext(t.Context(), &n, `SELECT count(*) FROM files_fts WHERE files_fts MATCH ?`, query))
	return n
}

func TestOpenDefaultsToUnicodeTokenizer(t *testing.T) {
	db := openTestDB(t, Options{})
	require.Equal(t, TokenizerUnicode, db.Tokenizer())
}

func TestInvalidTokenizerRejected(t *testing.T) {
	_, err := Open(t.Context(), filepath.Join(t.TempDir(), "x.sqlite"), Options{Tokenizer: "nope"})
	require.ErrorIs(t, err, ErrInvalidTokenizer)
}

func TestTokenizerPersistedAcrossOpens(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "x.sqlite")

	db, err := Open(ctx, path, Options{Tokenizer: TokenizerTrigram})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(ctx, path, Options{})
	require.NoError(t, err)
	require.Equal(t, TokenizerTrigram, db.Tokenizer())
	require.NoError(t, db.Close())

	_, err = Open(ctx, path, Options{Tokenizer: TokenizerUnicode})
	require.ErrorIs(t, err, ErrTokenizerMismatch)
}

func TestPragmasOnEveryConnection(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})

	for range 5 {
		conn, err := db.Connx(ctx)
		require.NoError(t, err)
		defer conn.Close()

		var fk int
		require.NoError(t, conn.GetContext(ctx, &fk, `PRAGMA foreign_keys`))
		require.Equal(t, 1, fk)
	}
}

func TestRootUpsertGetList(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})

	a, err := db.UpsertRoot(ctx, "/a")
	require.NoError(t, err)
	again, err := db.UpsertRoot(ctx, "/a")
	require.NoError(t, err)
	require.Equal(t, a.ID, again.ID)

	_, err = db.UpsertRoot(ctx, "/b")
	require.NoError(t, err)

	got, err := db.GetRoot(ctx, "/a")
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID)

	_, err = db.GetRoot(ctx, "/missing")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = db.UpsertFile(ctx, File{RootID: a.ID, Path: "x.md", Ext: "md", Content: "hello"})
	require.NoError(t, err)

	roots, err := db.ListRoots(ctx)
	require.NoError(t, err)
	require.Len(t, roots, 2)
	require.Equal(t, "/a", roots[0].Path)
	require.EqualValues(t, 1, roots[0].FileCount)
	require.Equal(t, "/b", roots[1].Path)
	require.EqualValues(t, 0, roots[1].FileCount)
}

func TestUpsertFileIsSearchable(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)

	id, err := db.UpsertFile(ctx, File{RootID: root.ID, Path: "notes/a.md", Ext: "md", Size: 5, ModTime: 1, Hash: "h1", Content: "the quick brown fox"})
	require.NoError(t, err)
	require.Equal(t, 1, matchCount(t, db, "quick"))

	// Updating the same path replaces the indexed content and keeps the id.
	id2, err := db.UpsertFile(ctx, File{RootID: root.ID, Path: "notes/a.md", Ext: "md", Size: 6, ModTime: 2, Hash: "h2", Content: "lazy dog"})
	require.NoError(t, err)
	require.Equal(t, id, id2)
	require.Equal(t, 0, matchCount(t, db, "quick"))
	require.Equal(t, 1, matchCount(t, db, "lazy"))

	f, err := db.GetFile(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "lazy dog", f.Content)
	require.Equal(t, "h2", f.Hash)
	require.Equal(t, "/r", f.RootPath)

	f2, err := db.GetFileByPath(ctx, root.ID, "notes/a.md")
	require.NoError(t, err)
	require.Equal(t, id, f2.ID)
}

func TestTrigramTokenizerMatchesSubstrings(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{Tokenizer: TokenizerTrigram})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)
	_, err = db.UpsertFile(ctx, File{RootID: root.ID, Path: "a.go", Ext: "go", Content: "func indexDirectory()"})
	require.NoError(t, err)
	require.Equal(t, 1, matchCount(t, db, `"ndexDir"`))
}

func TestFileStatesAndDelete(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)

	id, err := db.UpsertFile(ctx, File{RootID: root.ID, Path: "a.txt", Size: 3, ModTime: 10, Hash: "ha", Content: "alpha"})
	require.NoError(t, err)
	_, err = db.UpsertFile(ctx, File{RootID: root.ID, Path: "b.txt", Size: 4, ModTime: 20, Hash: "hb", Content: "beta"})
	require.NoError(t, err)

	states, err := db.FileStates(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, states, 2)
	require.Equal(t, FileState{ID: id, Path: "a.txt", Size: 3, ModTime: 10, Hash: "ha"}, states["a.txt"])

	require.NoError(t, db.DeleteFile(ctx, id))
	require.Equal(t, 0, matchCount(t, db, "alpha"))
	_, err = db.GetFile(ctx, id)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTouchFileUpdatesModTimeOnly(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)
	id, err := db.UpsertFile(ctx, File{RootID: root.ID, Path: "a.txt", Size: 3, ModTime: 10, Hash: "ha", Content: "alpha"})
	require.NoError(t, err)

	require.NoError(t, db.TouchFile(ctx, id, 99))
	f, err := db.GetFile(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, 99, f.ModTime)
	require.Equal(t, 1, matchCount(t, db, "alpha"))
}

func TestDeleteRootCascades(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)
	_, err = db.UpsertFile(ctx, File{RootID: root.ID, Path: "a.txt", Content: "alpha"})
	require.NoError(t, err)

	require.NoError(t, db.DeleteRoot(ctx, root.ID))
	require.Equal(t, 0, matchCount(t, db, "alpha"))
	roots, err := db.ListRoots(ctx)
	require.NoError(t, err)
	require.Empty(t, roots)
}

func TestWithTxRollsBackOnError(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)

	err = db.WithTx(ctx, func(s *Store) error {
		_, err := s.UpsertFile(ctx, File{RootID: root.ID, Path: "a.txt", Content: "alpha"})
		require.NoError(t, err)
		return ErrNotFound
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, 0, matchCount(t, db, "alpha"))

	require.NoError(t, db.WithTx(ctx, func(s *Store) error {
		_, err := s.UpsertFile(ctx, File{RootID: root.ID, Path: "a.txt", Content: "alpha"})
		return err
	}))
	require.Equal(t, 1, matchCount(t, db, "alpha"))
}

func TestUpsertFilesBatch(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)

	require.NoError(t, db.UpsertFiles(ctx, nil))

	require.NoError(t, db.UpsertFiles(ctx, []File{
		{RootID: root.ID, Path: "a.md", Ext: "md", Size: 1, ModTime: 1, Hash: "ha", Content: "alpha"},
		{RootID: root.ID, Path: "b.md", Ext: "md", Size: 2, ModTime: 2, Hash: "hb", Content: "beta"},
	}))
	require.Equal(t, 1, matchCount(t, db, "alpha"))
	require.Equal(t, 1, matchCount(t, db, "beta"))

	// A batch mixing an update and an insert.
	require.NoError(t, db.UpsertFiles(ctx, []File{
		{RootID: root.ID, Path: "a.md", Ext: "md", Size: 3, ModTime: 3, Hash: "ha2", Content: "gamma"},
		{RootID: root.ID, Path: "c.md", Ext: "md", Content: "delta"},
	}))
	require.Equal(t, 0, matchCount(t, db, "alpha"))
	require.Equal(t, 1, matchCount(t, db, "gamma"))
	require.Equal(t, 1, matchCount(t, db, "delta"))

	states, err := db.FileStates(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, states, 3)
	require.Equal(t, "ha2", states["a.md"].Hash)
	require.EqualValues(t, 3, states["a.md"].ModTime)
}

func TestUpsertFilesLargeBatchIsChunked(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)

	files := make([]File, 5000) // more rows than fit in one statement's variables
	for i := range files {
		files[i] = File{RootID: root.ID, Path: fmt.Sprintf("f%d.txt", i), Content: "common"}
	}
	require.NoError(t, db.UpsertFiles(ctx, files))
	require.Equal(t, 5000, matchCount(t, db, "common"))
}

func TestDeleteFiles(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)

	files := make([]File, 2000)
	for i := range files {
		files[i] = File{RootID: root.ID, Path: fmt.Sprintf("f%d", i), Content: "word"}
	}
	require.NoError(t, db.UpsertFiles(ctx, files))

	states, err := db.FileStates(ctx, root.ID)
	require.NoError(t, err)
	var ids []int64
	for _, st := range states {
		ids = append(ids, st.ID)
	}

	require.NoError(t, db.DeleteFiles(ctx, nil))
	require.NoError(t, db.DeleteFiles(ctx, ids[:1500]))
	require.Equal(t, 500, matchCount(t, db, "word"))
}

func TestFTSHashSizeIsRaised(t *testing.T) {
	db := openTestDB(t, Options{})
	var v int
	require.NoError(t, db.GetContext(t.Context(), &v, `SELECT v FROM files_fts_config WHERE k = 'hashsize'`))
	require.Equal(t, 8<<20, v)
}

func TestListFiles(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, Options{})
	a, err := db.UpsertRoot(ctx, "/a")
	require.NoError(t, err)
	b, err := db.UpsertRoot(ctx, "/b")
	require.NoError(t, err)
	require.NoError(t, db.UpsertFiles(ctx, []File{
		{RootID: a.ID, Path: "notes/z.md", Ext: "md", Size: 10, ModTime: 5, Content: "big content"},
		{RootID: a.ID, Path: "notes/a.txt", Ext: "txt", Size: 3, Content: "x"},
		{RootID: a.ID, Path: "100%_done.md", Ext: "md", Content: "x"},
		{RootID: b.ID, Path: "Main.go", Ext: "go", Content: "x"},
	}))

	all, total, err := db.ListFiles(ctx, ListParams{})
	require.NoError(t, err)
	require.EqualValues(t, 4, total)
	var got []string
	for _, f := range all {
		got = append(got, f.RootPath+"/"+f.Path)
	}
	require.Equal(t, []string{"/a/100%_done.md", "/a/notes/a.txt", "/a/notes/z.md", "/b/Main.go"}, got, "sorted by root then path")
	require.Equal(t, FileInfo{ID: all[2].ID, RootPath: "/a", Path: "notes/z.md", Ext: "md", Size: 10, ModTime: 5}, all[2])

	page, total, err := db.ListFiles(ctx, ListParams{Limit: 2, Offset: 1})
	require.NoError(t, err)
	require.EqualValues(t, 4, total, "total ignores paging")
	require.Len(t, page, 2)
	require.Equal(t, "notes/a.txt", page[0].Path)

	count := func(p ListParams) int64 {
		_, n, err := db.ListFiles(ctx, p)
		require.NoError(t, err)
		return n
	}
	require.EqualValues(t, 2, count(ListParams{Filter: Filter{Exts: []string{"md"}}}))
	require.EqualValues(t, 2, count(ListParams{Filter: Filter{PathPrefix: "notes/"}}))
	require.EqualValues(t, 1, count(ListParams{Filter: Filter{Scopes: []Scope{{RootID: b.ID}}}}))
	require.EqualValues(t, 1, count(ListParams{Filter: Filter{Contains: "main"}}), "contains is case-insensitive")
	require.EqualValues(t, 1, count(ListParams{Filter: Filter{Contains: "%_"}}), "wildcards are literal")
	require.EqualValues(t, 0, count(ListParams{Filter: Filter{Contains: "nope"}}))
}
