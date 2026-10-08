package database

import (
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
