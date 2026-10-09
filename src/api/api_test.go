package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chizu/src/database"
	"chizu/src/indexer"
	"chizu/src/search"

	"github.com/stretchr/testify/require"
)

type testServer struct {
	db  *database.DB
	dir string
	h   http.Handler
}

func newServer(t *testing.T, readOnly bool) *testServer {
	t.Helper()
	ctx := t.Context()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for p, c := range map[string]string{
		"monday.md":    "# Monday\nSaw a heron.",
		"tuesday.md":   "# Tuesday\nRained all day.",
		"code/main.go": "package main // heron",
	} {
		full := filepath.Join(dir, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(c), 0o644))
	}

	dbPath := filepath.Join(t.TempDir(), "api.sqlite")
	db, err := database.Open(ctx, dbPath, database.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	_, err = indexer.Index(ctx, db, dir, indexer.Options{})
	require.NoError(t, err)

	return &testServer{db: db, dir: dir, h: New(Config{DB: db, DBPath: dbPath, ReadOnly: readOnly})}
}

func (s *testServer) do(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, target, &buf)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}

type searchResponse struct {
	Results []search.Result `json:"results"`
}

func TestSearch(t *testing.T) {
	s := newServer(t, false)

	rec := s.do(t, "GET", "/api/search?q=heron", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	resp := decode[searchResponse](t, rec)
	require.Len(t, resp.Results, 2)

	rec = s.do(t, "GET", "/api/search?q=heron&ext=go", nil)
	resp = decode[searchResponse](t, rec)
	require.Len(t, resp.Results, 1)
	require.Equal(t, "code/main.go", resp.Results[0].Path)

	rec = s.do(t, "GET", "/api/search?q=heron&ext=md,go&limit=1", nil)
	resp = decode[searchResponse](t, rec)
	require.Len(t, resp.Results, 1)

	rec = s.do(t, "GET", "/api/search?q=heron&dir="+url.QueryEscape(filepath.Join(s.dir, "code")), nil)
	resp = decode[searchResponse](t, rec)
	require.Len(t, resp.Results, 1)

	rec = s.do(t, "GET", "/api/search?q=heron&path=mon&lines=1", nil)
	resp = decode[searchResponse](t, rec)
	require.Len(t, resp.Results, 1)
	require.Equal(t, 2, resp.Results[0].Lines[0].Number)

	rec = s.do(t, "GET", "/api/search?q="+url.QueryEscape("rained OR heron")+"&raw=true", nil)
	resp = decode[searchResponse](t, rec)
	require.Len(t, resp.Results, 3)

	rec = s.do(t, "GET", "/api/search?q=zebra", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, `{"results":[]}`, strings.TrimSpace(rec.Body.String()))
}

func TestSearchErrors(t *testing.T) {
	s := newServer(t, false)

	rec := s.do(t, "GET", "/api/search", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, decode[map[string]string](t, rec)["error"], "empty query")

	rec = s.do(t, "GET", "/api/search?raw=1&q="+url.QueryEscape("foo AND ("), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = s.do(t, "GET", "/api/search?q=x&limit=abc", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = s.do(t, "GET", "/api/search?q=x&dir=/not/indexed", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRoots(t *testing.T) {
	s := newServer(t, false)

	rec := s.do(t, "GET", "/api/roots", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	resp := decode[struct {
		Roots []database.Root `json:"roots"`
	}](t, rec)
	require.Len(t, resp.Roots, 1)
	require.Equal(t, s.dir, resp.Roots[0].Path)
	require.EqualValues(t, 3, resp.Roots[0].FileCount)
}

func TestFile(t *testing.T) {
	s := newServer(t, false)
	abs := filepath.Join(s.dir, "tuesday.md")

	rec := s.do(t, "GET", "/api/file?path="+url.QueryEscape(abs), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	f := decode[database.File](t, rec)
	require.Equal(t, "# Tuesday\nRained all day.", f.Content)

	rec = s.do(t, "GET", "/api/file?id="+jsonNumber(f.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, f.Path, decode[database.File](t, rec).Path)

	rec = s.do(t, "GET", "/api/file?raw=1&path="+url.QueryEscape(abs), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain"))
	require.Equal(t, "# Tuesday\nRained all day.", rec.Body.String())

	rec = s.do(t, "GET", "/api/file?path="+url.QueryEscape(filepath.Join(s.dir, "nope.md")), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = s.do(t, "GET", "/api/file", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestIndexAndRemove(t *testing.T) {
	s := newServer(t, false)

	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "wednesday.md"), []byte("sunny"), 0o644))

	// No dirs re-indexes every root.
	rec := s.do(t, "POST", "/api/index", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decode[struct {
		Stats  []indexer.Stats `json:"stats"`
		Errors []string        `json:"errors"`
	}](t, rec)
	require.Len(t, resp.Stats, 1)
	require.Equal(t, 1, resp.Stats[0].Added)
	require.Empty(t, resp.Errors)

	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "x.txt"), []byte("other words"), 0o644))
	rec = s.do(t, "POST", "/api/index", map[string]any{"dirs": []string{other, "/does/not/exist"}, "exts": []string{"txt"}})
	require.Equal(t, http.StatusOK, rec.Code)
	resp = decode[struct {
		Stats  []indexer.Stats `json:"stats"`
		Errors []string        `json:"errors"`
	}](t, rec)
	require.Len(t, resp.Stats, 1)
	require.Len(t, resp.Errors, 1)

	stored, err := indexer.StoredOptions(t.Context(), s.db, other)
	require.NoError(t, err)
	require.Equal(t, []string{"txt"}, stored.Exts)

	rec = s.do(t, "DELETE", "/api/roots?path="+url.QueryEscape(other), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	rec = s.do(t, "DELETE", "/api/roots?path="+url.QueryEscape(other), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestReadOnlyRejectsWrites(t *testing.T) {
	s := newServer(t, true)
	require.Equal(t, http.StatusForbidden, s.do(t, "POST", "/api/vacuum", nil).Code)

	rec := s.do(t, "POST", "/api/index", map[string]any{})
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = s.do(t, "DELETE", "/api/roots?path="+url.QueryEscape(s.dir), nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = s.do(t, "GET", "/api/search?q=heron", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestInfo(t *testing.T) {
	s := newServer(t, true)
	rec := s.do(t, "GET", "/api/info", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	info := decode[map[string]any](t, rec)
	require.Equal(t, "unicode61", info["tokenizer"])
	require.Equal(t, true, info["read_only"])
	require.Equal(t, database.Driver, info["driver"])
}

func TestUI(t *testing.T) {
	s := newServer(t, false)
	rec := s.do(t, "GET", "/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html"))
	require.Contains(t, rec.Body.String(), "<title>chizu</title>")
}

func TestFiles(t *testing.T) {
	s := newServer(t, false)

	rec := s.do(t, "GET", "/api/files", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	resp := decode[search.FileList](t, rec)
	require.Equal(t, 3, resp.Total)
	require.Equal(t, []string{"code/main.go", "monday.md", "tuesday.md"},
		[]string{resp.Files[0].Path, resp.Files[1].Path, resp.Files[2].Path})

	resp = decode[search.FileList](t, s.do(t, "GET", "/api/files?ext=md&limit=1&offset=1", nil))
	require.Equal(t, 2, resp.Total)
	require.Len(t, resp.Files, 1)
	require.Equal(t, "tuesday.md", resp.Files[0].Path)

	resp = decode[search.FileList](t, s.do(t, "GET", "/api/files?q=MON", nil))
	require.Equal(t, 1, resp.Total)

	resp = decode[search.FileList](t, s.do(t, "GET", "/api/files?dir="+url.QueryEscape(filepath.Join(s.dir, "code")), nil))
	require.Equal(t, 1, resp.Total)

	require.Equal(t, http.StatusBadRequest, s.do(t, "GET", "/api/files?limit=x", nil).Code)
	require.Equal(t, http.StatusNotFound, s.do(t, "GET", "/api/files?dir=/not/indexed", nil).Code)
}

func TestVacuum(t *testing.T) {
	s := newServer(t, false)
	rec := s.do(t, "POST", "/api/vacuum", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decode[map[string]int64](t, rec)
	require.Positive(t, resp["before"])
	require.Positive(t, resp["after"])
}
