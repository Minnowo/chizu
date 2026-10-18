package search

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"chizu/src/database"
	"chizu/src/indexer"

	"github.com/stretchr/testify/require"
)

// codeDB indexes a small multi-language tree and returns the db and its root.
func codeDB(t *testing.T) (*database.DB, string) {
	t.Helper()
	ctx := t.Context()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for p, c := range map[string]string{
		"store/store.go": "package store\n\ntype Store struct{}\n\nfunc (s *Store) Open() error {\n\treturn nil\n}\n\nfunc Open() *Store {\n\treturn &Store{}\n}\n",
		"app/app.py":     "class Store:\n    def open(self):\n        pass\n\n    class Inner:\n        def deep(self):\n            pass\n\ndef reopen():\n    pass\n",
		"notes.md":       "Open the Store",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(c), 0o644))
	}

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "code.sqlite"), database.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	_, err = indexer.Index(ctx, db, dir, indexer.Options{})
	require.NoError(t, err)
	return db, dir
}

// where renders definitions as "path:line kind qualified-name".
func where(defs []Definition) []string {
	out := []string{}
	for _, d := range defs {
		out = append(out, fmt.Sprintf("%s:%d %s %s", d.Path, d.Line, d.Kind, d.QualifiedName()))
	}
	return out
}

func TestDefinitions(t *testing.T) {
	ctx := t.Context()
	db, dir := codeDB(t)

	defs, err := Definitions(ctx, db, DefQuery{Name: "open"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"store/store.go:5 method Store.Open",
		"store/store.go:9 function Open",
		"app/app.py:2 method Store.open",
	}, where(defs))
	require.True(t, defs[0].Exact)
	require.Equal(t, "open", defs[0].Name, "exact case first")
	require.Empty(t, defs[0].Body)

	for _, name := range []string{"Store.Open", "store::open", "Store#open", "Store->Open"} {
		defs, err = Definitions(ctx, db, DefQuery{Name: name})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"store/store.go:5 method Store.Open", "app/app.py:2 method Store.open"}, where(defs), name)
	}

	defs, err = Definitions(ctx, db, DefQuery{Name: "open", Exts: []string{"go"}, Kinds: []string{"function"}})
	require.NoError(t, err)
	require.Equal(t, []string{"store/store.go:9 function Open"}, where(defs))
	require.Equal(t, filepath.Join(dir, "store", "store.go"), defs[0].AbsPath)
	require.Equal(t, "func Open() *Store {", defs[0].Signature)

	defs, err = Definitions(ctx, db, DefQuery{Name: "open", Dir: filepath.Join(dir, "app")})
	require.NoError(t, err)
	require.Equal(t, []string{"app/app.py:2 method Store.open"}, where(defs))

	defs, err = Definitions(ctx, db, DefQuery{Name: "Open", Exts: []string{"go"}, Kinds: []string{"function"}, Body: true})
	require.NoError(t, err)
	require.Equal(t, []string{"func Open() *Store {", "\treturn &Store{}", "}"}, defs[0].Body)

	// No exact match: names containing it, those starting with it first.
	defs, err = Definitions(ctx, db, DefQuery{Name: "ope"})
	require.NoError(t, err)
	require.Len(t, defs, 4)
	require.False(t, defs[0].Exact)
	require.Equal(t, "reopen", defs[3].Name)

	defs, err = Definitions(ctx, db, DefQuery{Name: "nothing"})
	require.NoError(t, err)
	require.Empty(t, defs)

	defs, err = Definitions(ctx, db, DefQuery{Name: "open", Limit: 1, Offset: 5})
	require.NoError(t, err)
	require.Empty(t, defs, "paging past the exact matches does not fall back to substrings")

	_, err = Definitions(ctx, db, DefQuery{Name: " "})
	require.ErrorIs(t, err, ErrEmptyQuery)
}

func TestSplitQualified(t *testing.T) {
	for in, want := range map[string][2]string{
		"Open":         {"", "Open"},
		"Store.Open":   {"Store", "Open"},
		"a.b.Open":     {"a.b", "Open"},
		"mod::Type::f": {"mod::Type", "f"},
		".Open":        {"", ".Open"},
		"Store.":       {"", "Store."},
		"Store#open":   {"Store", "open"},
	} {
		parent, name := splitQualified(in)
		require.Equal(t, want, [2]string{parent, name}, in)
	}
}

func TestOutline(t *testing.T) {
	ctx := t.Context()
	db, dir := codeDB(t)

	f, err := GetFile(ctx, db, filepath.Join(dir, "app", "app.py"))
	require.NoError(t, err)

	outline, err := Outline(ctx, db, f.ID)
	require.NoError(t, err)

	var got []string
	for _, e := range outline {
		got = append(got, fmt.Sprintf("%d %s %s", e.Depth, e.Kind, e.Name))
	}
	require.Equal(t, []string{
		"0 class Store",
		"1 method open",
		"1 class Inner",
		"2 method deep",
		"0 function reopen",
	}, got)

	f, err = GetFile(ctx, db, filepath.Join(dir, "notes.md"))
	require.NoError(t, err)
	outline, err = Outline(ctx, db, f.ID)
	require.NoError(t, err)
	require.Empty(t, outline)
}
