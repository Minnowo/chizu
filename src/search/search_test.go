package search

import (
	"path/filepath"
	"strings"
	"testing"

	"chizu/src/database"

	"github.com/stretchr/testify/require"
)

func TestBuildMatch(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello", `"hello"`},
		{"hello world", `"hello" "world"`},
		{"  spaced   out  ", `"spaced" "out"`},
		{"foo-bar", `"foo-bar"`},
		{"c++", `"c++"`},
		{"AND OR NOT", `"AND" "OR" "NOT"`},
		{`say "exact phrase" here`, `"say" "exact phrase" "here"`},
		{`unterminated "quote here`, `"unterminated" "quote here"`},
		{`it"s`, `"it""s"`},
		{"run*", `"run"*`},
		{"*", ``},
		{"col:umn", `"col:umn"`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := BuildMatch(c.in, false)
			if c.want == "" {
				require.ErrorIs(t, err, ErrEmptyQuery)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

func TestBuildMatchRaw(t *testing.T) {
	got, err := BuildMatch(`  foo AND (bar OR baz*)  `, true)
	require.NoError(t, err)
	require.Equal(t, `foo AND (bar OR baz*)`, got)

	_, err = BuildMatch("   ", true)
	require.ErrorIs(t, err, ErrEmptyQuery)
}

func TestParseMarked(t *testing.T) {
	require.Equal(t, []Segment{
		{Text: "a "},
		{Text: "hit", Match: true},
		{Text: " b"},
	}, parseMarked("a \x02hit\x03 b"))

	require.Equal(t, []Segment{{Text: "x", Match: true}}, parseMarked("\x02x\x03"))
	require.Empty(t, parseMarked(""))
}

// fixture builds a DB with two roots and a few files.
type fixture struct {
	db    *database.DB
	rootA database.Root
	rootB database.Root
}

func newFixture(t *testing.T, tokenizer string) *fixture {
	t.Helper()
	ctx := t.Context()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "s.sqlite"), database.Options{Tokenizer: tokenizer})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	a, err := db.UpsertRoot(ctx, "/notes")
	require.NoError(t, err)
	b, err := db.UpsertRoot(ctx, "/code")
	require.NoError(t, err)

	add := func(root database.Root, path, ext, content string) {
		_, err := db.UpsertFile(ctx, database.File{RootID: root.ID, Path: path, Ext: ext, Content: content})
		require.NoError(t, err)
	}

	add(a, "journal/monday.md", "md", "# Monday\nWent running in the park.\nSaw a heron.\nRunning again tomorrow.")
	add(a, "journal/tuesday.md", "md", "# Tuesday\nRained all day.")
	add(a, "recipes/bread.txt", "txt", "flour water salt yeast\nknead the dough")
	add(b, "main.go", "go", "package main\n\nfunc main() {\n\trun()\n}")
	add(b, "docs/readme.md", "md", "how to run the program")

	return &fixture{db: db, rootA: a, rootB: b}
}

func paths(results []Result) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Root + "/" + r.Path
	}
	return out
}

func TestSearchStemmingAcrossRoots(t *testing.T) {
	f := newFixture(t, "")
	results, err := Search(t.Context(), f.db, Query{Text: "run"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"/notes/journal/monday.md",
		"/code/main.go",
		"/code/docs/readme.md",
	}, paths(results))
}

func TestSearchResultDetails(t *testing.T) {
	f := newFixture(t, "")
	results, err := Search(t.Context(), f.db, Query{Text: "heron"})
	require.NoError(t, err)
	require.Len(t, results, 1)

	r := results[0]
	require.Equal(t, "/notes", r.Root)
	require.Equal(t, "journal/monday.md", r.Path)
	require.Equal(t, "/notes/journal/monday.md", r.AbsPath)
	require.NotZero(t, r.FileID)
	require.Greater(t, r.Score, 0.0)

	require.Contains(t, r.Snippet, Segment{Text: "heron", Match: true})

	require.Equal(t, 1, r.LineCount)
	require.Equal(t, []Line{{
		Number:   3,
		Segments: []Segment{{Text: "Saw a "}, {Text: "heron", Match: true}, {Text: "."}},
	}}, r.Lines)
}

func TestSearchMultipleMatchingLines(t *testing.T) {
	f := newFixture(t, "")
	results, err := Search(t.Context(), f.db, Query{Text: "running", Dir: "/notes"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, 2, results[0].LineCount)
	require.Equal(t, 2, results[0].Lines[0].Number)
	require.Equal(t, 4, results[0].Lines[1].Number)

	results, err = Search(t.Context(), f.db, Query{Text: "running", Dir: "/notes", MaxLines: 1})
	require.NoError(t, err)
	require.Len(t, results[0].Lines, 1)
	require.Equal(t, 2, results[0].LineCount)
}

func TestSearchFilters(t *testing.T) {
	f := newFixture(t, "")
	ctx := t.Context()

	results, err := Search(ctx, f.db, Query{Text: "run", Dir: "/code"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"/code/main.go", "/code/docs/readme.md"}, paths(results))

	// A subdirectory of a root works too.
	results, err = Search(ctx, f.db, Query{Text: "run", Dir: "/code/docs"})
	require.NoError(t, err)
	require.Equal(t, []string{"/code/docs/readme.md"}, paths(results))

	results, err = Search(ctx, f.db, Query{Text: "run", Exts: []string{".MD"}})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"/notes/journal/monday.md", "/code/docs/readme.md"}, paths(results))

	results, err = Search(ctx, f.db, Query{Text: "rained OR heron", Raw: true, PathPrefix: "journal/t"})
	require.NoError(t, err)
	require.Equal(t, []string{"/notes/journal/tuesday.md"}, paths(results))

	_, err = Search(ctx, f.db, Query{Text: "run", Dir: "/elsewhere"})
	require.ErrorIs(t, err, ErrNotIndexed)
}

func TestSearchPathPrefixEscapesLike(t *testing.T) {
	f := newFixture(t, "")
	results, err := Search(t.Context(), f.db, Query{Text: "run", PathPrefix: "%"})
	require.NoError(t, err)
	require.Empty(t, results)
}

func TestSearchLimitOffset(t *testing.T) {
	f := newFixture(t, "")
	ctx := t.Context()

	all, err := Search(ctx, f.db, Query{Text: "run"})
	require.NoError(t, err)
	require.Len(t, all, 3)

	page, err := Search(ctx, f.db, Query{Text: "run", Limit: 2})
	require.NoError(t, err)
	require.Equal(t, paths(all[:2]), paths(page))

	page, err = Search(ctx, f.db, Query{Text: "run", Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, paths(all[2:]), paths(page))
}

func TestSearchPunctuationIsSafeInSimpleMode(t *testing.T) {
	f := newFixture(t, "")
	_, err := Search(t.Context(), f.db, Query{Text: `main() { "quote`})
	require.NoError(t, err)
}

func TestSearchBadRawQuery(t *testing.T) {
	f := newFixture(t, "")
	_, err := Search(t.Context(), f.db, Query{Text: `foo AND (`, Raw: true})
	require.ErrorIs(t, err, ErrBadQuery)
}

func TestSearchTrigramSubstring(t *testing.T) {
	f := newFixture(t, database.TokenizerTrigram)
	results, err := Search(t.Context(), f.db, Query{Text: "nead"})
	require.NoError(t, err)
	require.Equal(t, []string{"/notes/recipes/bread.txt"}, paths(results))
	require.Equal(t, 2, results[0].Lines[0].Number)
}

func TestLongLinesAreCropped(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, "")
	long := strings.Repeat("filler ", 200) + "needle" + strings.Repeat(" filler", 200)
	_, err := f.db.UpsertFile(ctx, database.File{RootID: f.rootA.ID, Path: "long.txt", Content: long})
	require.NoError(t, err)

	results, err := Search(ctx, f.db, Query{Text: "needle"})
	require.NoError(t, err)
	require.Len(t, results, 1)

	var text strings.Builder
	for _, s := range results[0].Lines[0].Segments {
		text.WriteString(s.Text)
	}
	require.Less(t, len(text.String()), 400)
	require.Contains(t, results[0].Lines[0].Segments, Segment{Text: "needle", Match: true})
}

func TestGetFile(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, "")

	file, err := GetFile(ctx, f.db, "/code/docs/readme.md")
	require.NoError(t, err)
	require.Equal(t, "how to run the program", file.Content)
	require.Equal(t, "/code", file.RootPath)

	byID, err := GetFileByID(ctx, f.db, file.ID)
	require.NoError(t, err)
	require.Equal(t, file.Path, byID.Path)

	_, err = GetFile(ctx, f.db, "/code/nope.md")
	require.ErrorIs(t, err, database.ErrNotFound)
	_, err = GetFile(ctx, f.db, "/elsewhere/x.md")
	require.ErrorIs(t, err, database.ErrNotFound)
}

func TestListFiles(t *testing.T) {
	f := newFixture(t, "")
	ctx := t.Context()

	list, err := ListFiles(ctx, f.db, FileQuery{})
	require.NoError(t, err)
	require.Equal(t, 5, list.Total)
	require.Len(t, list.Files, 5)
	require.Equal(t, "/code", list.Files[0].Root)
	require.Equal(t, "docs/readme.md", list.Files[0].Path)
	require.Equal(t, "/code/docs/readme.md", list.Files[0].AbsPath)
	require.NotZero(t, list.Files[0].FileID)

	list, err = ListFiles(ctx, f.db, FileQuery{Dir: "/notes/journal"})
	require.NoError(t, err)
	require.Equal(t, 2, list.Total)

	list, err = ListFiles(ctx, f.db, FileQuery{Exts: []string{".TXT"}})
	require.NoError(t, err)
	require.Equal(t, 1, list.Total)
	require.Equal(t, "recipes/bread.txt", list.Files[0].Path)

	list, err = ListFiles(ctx, f.db, FileQuery{Contains: "day", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, 2, list.Total)
	require.Len(t, list.Files, 1)

	list, err = ListFiles(ctx, f.db, FileQuery{PathPrefix: "nothing/"})
	require.NoError(t, err)
	require.Equal(t, 0, list.Total)
	require.NotNil(t, list.Files)

	_, err = ListFiles(ctx, f.db, FileQuery{Dir: "/elsewhere"})
	require.ErrorIs(t, err, ErrNotIndexed)
}

func TestFileMatches(t *testing.T) {
	f := newFixture(t, "")
	ctx := t.Context()
	results, err := Search(ctx, f.db, Query{Text: "heron"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	id := results[0].FileID // journal/monday.md

	lines, err := FileMatches(ctx, f.db, id, "running", false)
	require.NoError(t, err)
	require.Equal(t, []Line{
		{Number: 2, Segments: []Segment{{Text: "Went "}, {Text: "running", Match: true}, {Text: " in the park."}}},
		{Number: 4, Segments: []Segment{{Text: "Running", Match: true}, {Text: " again tomorrow."}}},
	}, lines)

	lines, err = FileMatches(ctx, f.db, id, "heron OR park", true)
	require.NoError(t, err)
	require.Len(t, lines, 2)

	// A file the query does not match has no matching lines, not an error.
	lines, err = FileMatches(ctx, f.db, id, "flour", false)
	require.NoError(t, err)
	require.Empty(t, lines)

	_, err = FileMatches(ctx, f.db, id, "", false)
	require.ErrorIs(t, err, ErrEmptyQuery)
	_, err = FileMatches(ctx, f.db, id, "foo AND (", true)
	require.ErrorIs(t, err, ErrBadQuery)
}

func TestFileMatchesKeepsLongLinesWhole(t *testing.T) {
	ctx := t.Context()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "s.sqlite"), database.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	root, err := db.UpsertRoot(ctx, "/r")
	require.NoError(t, err)
	long := strings.Repeat("a ", 500) + "needle" + strings.Repeat(" b", 500)
	id, err := db.UpsertFile(ctx, database.File{RootID: root.ID, Path: "x.txt", Content: long})
	require.NoError(t, err)

	lines, err := FileMatches(ctx, db, id, "needle", false)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	var sb strings.Builder
	for _, s := range lines[0].Segments {
		sb.WriteString(s.Text)
	}
	require.Equal(t, long, sb.String())
}
