package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"chizu/src/database"
	"chizu/src/ignore"
	"chizu/src/search"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type output struct {
	stdout string
	stderr string
}

func run(t *testing.T, args ...string) (output, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := New(&out, &errOut)
	err := app.Run(t.Context(), append([]string{"chizu"}, args...))
	return output{stdout: out.String(), stderr: errOut.String()}, err
}

func mustRun(t *testing.T, args ...string) output {
	t.Helper()
	out, err := run(t, args...)
	require.NoError(t, err, "stderr: %s", out.stderr)
	return out
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// notesDir returns a resolved temp dir with a few notes in it.
func notesDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	writeTree(t, dir, map[string]string{
		"monday.md":      "# Monday\nWent running.\nSaw a heron.",
		"tuesday.md":     "# Tuesday\nRained all day.",
		"code/main.go":   "package main\n// heron detector",
		"notes/todo.txt": "buy milk",
	})
	return dir
}

func TestInit(t *testing.T) {
	dir := t.TempDir()

	out := mustRun(t, "init", dir)
	require.Contains(t, out.stdout, filepath.Join(dir, ignore.FileName))

	data, err := os.ReadFile(filepath.Join(dir, ignore.FileName))
	require.NoError(t, err)
	require.Equal(t, ignore.DefaultFile(), string(data))

	_, err = run(t, "init", dir)
	require.Error(t, err)

	mustRun(t, "init", "--force", dir)
}

func TestIndexThenSearch(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")

	out := mustRun(t, "index", "--db", db, dir)
	require.Contains(t, out.stdout, dir)
	require.Contains(t, out.stdout, "4 added")

	out = mustRun(t, "search", "--db", db, "heron")
	require.Contains(t, out.stdout, filepath.Join(dir, "monday.md"))
	require.Contains(t, out.stdout, "3: Saw a heron.")
	require.Contains(t, out.stdout, filepath.Join(dir, "code", "main.go"))

	// Multiple words are joined into one query.
	out = mustRun(t, "search", "--db", db, "saw", "heron")
	require.Contains(t, out.stdout, "monday.md")
	require.NotContains(t, out.stdout, "main.go")
}

func TestSearchJSON(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	out := mustRun(t, "search", "--db", db, "--json", "rained")
	var results []search.Result
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &results))
	require.Len(t, results, 1)
	require.Equal(t, "tuesday.md", results[0].Path)
	require.Equal(t, 2, results[0].Lines[0].Number)
}

func TestSearchFlags(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	out := mustRun(t, "search", "--db", db, "--ext", "go", "heron")
	require.Contains(t, out.stdout, "main.go")
	require.NotContains(t, out.stdout, "monday.md")

	out = mustRun(t, "search", "--db", db, "--dir", filepath.Join(dir, "code"), "heron")
	require.Contains(t, out.stdout, "main.go")
	require.NotContains(t, out.stdout, "monday.md")

	out = mustRun(t, "search", "--db", db, "--path", "mon", "heron")
	require.Contains(t, out.stdout, "monday.md")
	require.NotContains(t, out.stdout, "main.go")

	out = mustRun(t, "search", "--db", db, "--raw", "rained OR milk")
	require.Contains(t, out.stdout, "tuesday.md")
	require.Contains(t, out.stdout, "todo.txt")

	out = mustRun(t, "search", "--db", db, "--json", "--limit", "1", "heron")
	var results []search.Result
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &results))
	require.Len(t, results, 1)
}

func TestSearchNoResults(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	_, err := run(t, "search", "--db", db, "zebra")
	require.ErrorIs(t, err, ErrNoResults)

	// JSON output prints an empty array instead.
	out, err := run(t, "search", "--db", db, "--json", "zebra")
	require.ErrorIs(t, err, ErrNoResults)
	require.Equal(t, "[]\n", out.stdout)
}

func TestSearchRequiresQuery(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	_, err := run(t, "search", "--db", db)
	require.Error(t, err)
}

func TestReadCommandsNeedAnExistingDB(t *testing.T) {
	db := filepath.Join(t.TempDir(), "missing.sqlite")
	for _, args := range [][]string{
		{"search", "--db", db, "x"},
		{"roots", "--db", db},
		{"cat", "--db", db, "1"},
	} {
		_, err := run(t, args...)
		require.ErrorContains(t, err, "chizu index", args[0])
		require.NoFileExists(t, db)
	}
}

func TestIndexMultipleDirs(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]string{"a.md": "alpha"})
	writeTree(t, b, map[string]string{"b.md": "beta"})
	writeTree(t, c, map[string]string{"c.md": "gamma"})
	db := filepath.Join(t.TempDir(), "x.sqlite")

	mustRun(t, "index", "--db", db, "--workers", "2", "-i", a, "-i", b, c)

	out := mustRun(t, "roots", "--db", db)
	require.Equal(t, 3, strings.Count(out.stdout, "1 files"))
}

func TestIndexWithoutArgsReindexesAllRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]string{"a.md": "alpha"})
	writeTree(t, b, map[string]string{"b.md": "beta"})
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, a, b)

	writeTree(t, b, map[string]string{"new.md": "delta"})
	out := mustRun(t, "index", "--db", db)
	require.Contains(t, out.stdout, "1 added")
	require.Equal(t, 2, strings.Count(out.stdout, "unchanged"))

	mustRun(t, "search", "--db", db, "delta")
}

func TestIndexWithoutArgsOnEmptyDBFails(t *testing.T) {
	db := filepath.Join(t.TempDir(), "x.sqlite")
	_, err := run(t, "index", "--db", db)
	require.ErrorContains(t, err, "chizu index <dir>")
}

func TestIndexFilterFlags(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.md": "alpha", "b.txt": "alpha", "drafts/c.md": "alpha", "big.md": strings.Repeat("alpha ", 100)})
	db := filepath.Join(t.TempDir(), "x.sqlite")

	mustRun(t, "index", "--db", db, "--ext", "md", "--exclude", "drafts/", "--max-size", "100", dir)
	out := mustRun(t, "search", "--db", db, "--json", "alpha")
	var results []search.Result
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &results))
	require.Len(t, results, 1)
	require.Equal(t, "a.md", results[0].Path)

	// Filters are remembered, and --clear-filters drops them.
	mustRun(t, "index", "--db", db)
	out = mustRun(t, "search", "--db", db, "--json", "alpha")
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &results))
	require.Len(t, results, 1)

	mustRun(t, "index", "--db", db, "--clear-filters", dir)
	out = mustRun(t, "search", "--db", db, "--json", "alpha")
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &results))
	require.Len(t, results, 4)
}

func TestIndexTokenizerFlag(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": "func indexDirectory() {}"})
	db := filepath.Join(t.TempDir(), "x.sqlite")

	mustRun(t, "index", "--db", db, "--tokenizer", "trigram", dir)
	out := mustRun(t, "search", "--db", db, "ndexDir")
	require.Contains(t, out.stdout, "a.go")

	_, err := run(t, "index", "--db", db, "--tokenizer", "unicode61", dir)
	require.ErrorIs(t, err, database.ErrTokenizerMismatch)
}

func TestRootsJSONAndRemove(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	out := mustRun(t, "roots", "--db", db, "--json")
	var roots []database.Root
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &roots))
	require.Len(t, roots, 1)
	require.Equal(t, dir, roots[0].Path)
	require.EqualValues(t, 4, roots[0].FileCount)

	mustRun(t, "remove", "--db", db, dir)
	out = mustRun(t, "roots", "--db", db, "--json")
	require.Equal(t, "[]\n", out.stdout)

	_, err := run(t, "remove", "--db", db, dir)
	require.ErrorIs(t, err, database.ErrNotFound)
}

func TestFiles(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	out := mustRun(t, "files", "--db", db)
	require.Equal(t, strings.Join([]string{
		filepath.Join(dir, "code/main.go"),
		filepath.Join(dir, "monday.md"),
		filepath.Join(dir, "notes/todo.txt"),
		filepath.Join(dir, "tuesday.md"),
	}, "\n")+"\n", out.stdout)

	out = mustRun(t, "files", "--db", db, "--ext", "md", "day")
	require.Equal(t, filepath.Join(dir, "monday.md")+"\n"+filepath.Join(dir, "tuesday.md")+"\n", out.stdout)

	out = mustRun(t, "files", "--db", db, "--dir", filepath.Join(dir, "notes"))
	require.Equal(t, filepath.Join(dir, "notes/todo.txt")+"\n", out.stdout)

	out = mustRun(t, "files", "--db", db, "--limit", "1", "--offset", "1", "--json")
	var list search.FileList
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &list))
	require.Equal(t, 4, list.Total)
	require.Len(t, list.Files, 1)
	require.Equal(t, "monday.md", list.Files[0].Path)

	_, err := run(t, "files", "--db", db, "zebra")
	require.ErrorIs(t, err, ErrNoResults)
}

func TestVacuum(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	out := mustRun(t, "vacuum", "--db", db)
	require.Regexp(t, `^vacuumed .*x\.sqlite: \S+ -> \S+\n$`, out.stdout)

	_, err := run(t, "vacuum", "--db", filepath.Join(t.TempDir(), "missing.sqlite"))
	require.Error(t, err)
}

func TestCat(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	out := mustRun(t, "cat", "--db", db, filepath.Join(dir, "tuesday.md"))
	require.Equal(t, "# Tuesday\nRained all day.", out.stdout)

	jsonOut := mustRun(t, "search", "--db", db, "--json", "rained")
	var results []search.Result
	require.NoError(t, json.Unmarshal([]byte(jsonOut.stdout), &results))

	out = mustRun(t, "cat", "--db", db, strconv.FormatInt(results[0].FileID, 10))
	require.Equal(t, "# Tuesday\nRained all day.", out.stdout)

	_, err := run(t, "cat", "--db", db, filepath.Join(dir, "nope.md"))
	require.ErrorIs(t, err, database.ErrNotFound)
}

func TestDefaultDBIsFoundInCwdOrParents(t *testing.T) {
	dir := notesDir(t)
	t.Chdir(dir)

	mustRun(t, "index", ".")
	require.FileExists(t, filepath.Join(dir, ".chizu.sqlite"))

	// The database itself is not indexed.
	out := mustRun(t, "roots", "--json")
	var roots []database.Root
	require.NoError(t, json.Unmarshal([]byte(out.stdout), &roots))
	require.EqualValues(t, 4, roots[0].FileCount)

	// Searching from a subdirectory finds the parent's database, and paths
	// are shown relative to the working directory.
	t.Chdir(filepath.Join(dir, "code"))
	out = mustRun(t, "search", "heron")
	require.Contains(t, out.stdout, "main.go\n")
	require.Contains(t, out.stdout, filepath.Join("..", "monday.md")+"\n")
}

func TestDBFromEnv(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "env.sqlite")
	t.Setenv("CHIZU_DB", db)

	mustRun(t, "index", dir)
	require.FileExists(t, db)
}

func TestMCPStdio(t *testing.T) {
	dir := notesDir(t)
	db := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", db, dir)

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"rained"}}}`,
	}, "\n") + "\n"

	// Like a real client, keep stdin open until the tool call is answered.
	pr, pw := io.Pipe()
	out := &notifyWriter{want: `"id":2`, done: make(chan struct{})}
	go func() {
		io.WriteString(pw, in)
		select {
		case <-out.done:
		case <-time.After(10 * time.Second):
		}
		pw.Close()
	}()

	var errOut bytes.Buffer
	app := New(out, &errOut)
	app.Reader = pr
	require.NoError(t, app.Run(t.Context(), []string{"chizu", "mcp", "--db", db}), errOut.String())

	// Every stdout line is a JSON-RPC message; logs must not leak into it.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2, out.String())
	for _, l := range lines {
		require.True(t, json.Valid([]byte(l)), l)
	}
	require.Contains(t, lines[1], "tuesday.md")
}

func TestServeHandlerMountsAPIAndMCP(t *testing.T) {
	dir := notesDir(t)
	dbFile := filepath.Join(t.TempDir(), "x.sqlite")
	mustRun(t, "index", "--db", dbFile, dir)

	db, err := database.Open(t.Context(), dbFile, database.Options{})
	require.NoError(t, err)
	defer db.Close()

	srv := httptest.NewServer(serveHandler(db, dbFile, true, zerolog.Nop()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/search?q=heron")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, err = http.Get(srv.URL + "/")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// notifyWriter buffers output and closes done once want has been written.
type notifyWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	done chan struct{}
	once sync.Once
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.done) })
	}
	return n, err
}

func (w *notifyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestIndexReportsEachDirectoryClearly(t *testing.T) {
	dir := notesDir(t)
	missing := filepath.Join(t.TempDir(), "missing")
	db := filepath.Join(t.TempDir(), "x.sqlite")

	out, err := run(t, "index", "--db", db, dir, missing)
	require.Error(t, err)
	require.Contains(t, out.stdout, "indexed "+dir+" in ")
	require.Contains(t, out.stdout, "4 added")
	require.Contains(t, out.stderr, "failed to index "+missing+":")
	require.Equal(t, "1 of 2 directories failed", err.Error())

	// Re-indexing everything reports each root the same way.
	out = mustRun(t, "index", "--db", db)
	require.Contains(t, out.stdout, "indexed "+dir+" in ")
}
