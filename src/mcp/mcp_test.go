package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chizu/src/database"
	"chizu/src/indexer"
	"chizu/src/search"

	"github.com/mark3labs/mcp-go/client"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	dir    string
	db     *database.DB
	client *client.Client
}

func newFixture(t *testing.T, readOnly bool) *fixture {
	t.Helper()
	ctx := t.Context()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for p, c := range map[string]string{
		"monday.md":    "# Monday\nSaw a heron.\nline three\nline four",
		"code/main.go": "package main // heron\n\ntype Store struct{}\n\nfunc (s *Store) Open(path string) error {\n\treturn nil\n}\n\nfunc main() {}\n",
	} {
		full := filepath.Join(dir, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(c), 0o644))
	}

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "mcp.sqlite"), database.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	_, err = indexer.Index(ctx, db, dir, indexer.Options{})
	require.NoError(t, err)

	c, err := client.NewInProcessClient(New(Config{DB: db, ReadOnly: readOnly}))
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })

	require.NoError(t, c.Start(ctx))
	init := gomcp.InitializeRequest{}
	init.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = gomcp.Implementation{Name: "test", Version: "1"}
	_, err = c.Initialize(ctx, init)
	require.NoError(t, err)

	return &fixture{dir: dir, db: db, client: c}
}

func (f *fixture) call(t *testing.T, name string, args map[string]any) *gomcp.CallToolResult {
	t.Helper()
	req := gomcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := f.client.CallTool(t.Context(), req)
	require.NoError(t, err)
	return res
}

func text(t *testing.T, res *gomcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(gomcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func toolNames(t *testing.T, f *fixture) []string {
	t.Helper()
	res, err := f.client.ListTools(t.Context(), gomcp.ListToolsRequest{})
	require.NoError(t, err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestTools(t *testing.T) {
	readOnly := []string{"search", "find_definition", "file_outline", "list_roots", "list_files", "read_file"}
	require.ElementsMatch(t, append(readOnly, "index", "remove_root"), toolNames(t, newFixture(t, false)))
	require.ElementsMatch(t, readOnly, toolNames(t, newFixture(t, true)))
}

func TestSearchTool(t *testing.T) {
	f := newFixture(t, false)

	res := f.call(t, "search", map[string]any{"query": "heron"})
	require.False(t, res.IsError, text(t, res))
	out := text(t, res)
	require.Contains(t, out, filepath.Join(f.dir, "monday.md"))
	require.Contains(t, out, "2: Saw a heron.")
	require.Contains(t, out, filepath.Join(f.dir, "code", "main.go"))

	res = f.call(t, "search", map[string]any{"query": "heron", "exts": []string{"go"}, "limit": 5})
	out = text(t, res)
	require.Contains(t, out, "main.go")
	require.NotContains(t, out, "monday.md")

	res = f.call(t, "search", map[string]any{"query": "heron", "dir": filepath.Join(f.dir, "code")})
	require.NotContains(t, text(t, res), "monday.md")

	res = f.call(t, "search", map[string]any{"query": "heron", "path_prefix": "mon"})
	require.NotContains(t, text(t, res), "main.go")

	res = f.call(t, "search", map[string]any{"query": "three OR four", "raw": true})
	require.Contains(t, text(t, res), "monday.md")

	res = f.call(t, "search", map[string]any{"query": "zebra"})
	require.False(t, res.IsError)
	require.Contains(t, text(t, res), "No results")
}

func TestSearchToolErrors(t *testing.T) {
	f := newFixture(t, false)

	res := f.call(t, "search", map[string]any{})
	require.True(t, res.IsError)

	res = f.call(t, "search", map[string]any{"query": "foo AND (", "raw": true})
	require.True(t, res.IsError)
	require.Contains(t, text(t, res), "invalid query")
}

func TestListRootsTool(t *testing.T) {
	f := newFixture(t, false)
	res := f.call(t, "list_roots", nil)
	require.False(t, res.IsError)
	require.Contains(t, text(t, res), f.dir)
	require.Contains(t, text(t, res), "2 files")
}

func TestReadFileTool(t *testing.T) {
	f := newFixture(t, false)
	path := filepath.Join(f.dir, "monday.md")

	res := f.call(t, "read_file", map[string]any{"path": path})
	require.False(t, res.IsError, text(t, res))
	require.Equal(t, "# Monday\nSaw a heron.\nline three\nline four", text(t, res))

	file, err := f.db.GetFileByPath(t.Context(), 1, "monday.md")
	require.NoError(t, err)
	res = f.call(t, "read_file", map[string]any{"file_id": file.ID, "start_line": 2, "end_line": 3})
	require.False(t, res.IsError, text(t, res))
	require.Equal(t, "Saw a heron.\nline three", text(t, res))

	res = f.call(t, "read_file", map[string]any{"path": path, "start_line": 3})
	require.Equal(t, "line three\nline four", text(t, res))

	res = f.call(t, "read_file", map[string]any{"path": filepath.Join(f.dir, "nope.md")})
	require.True(t, res.IsError)

	res = f.call(t, "read_file", map[string]any{})
	require.True(t, res.IsError)
}

func TestIndexAndRemoveTools(t *testing.T) {
	f := newFixture(t, false)

	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "new.md"), []byte("brand new"), 0o644))
	res := f.call(t, "index", nil)
	require.False(t, res.IsError, text(t, res))
	require.Contains(t, text(t, res), "1 added")

	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "x.txt"), []byte("other"), 0o644))
	res = f.call(t, "index", map[string]any{"dirs": []string{other}, "exts": []string{"txt"}})
	require.False(t, res.IsError, text(t, res))

	stored, err := indexer.StoredOptions(t.Context(), f.db, other)
	require.NoError(t, err)
	require.Equal(t, []string{"txt"}, stored.Exts)

	res = f.call(t, "remove_root", map[string]any{"path": other})
	require.False(t, res.IsError, text(t, res))

	res = f.call(t, "remove_root", map[string]any{"path": other})
	require.True(t, res.IsError)
}

func TestHTTPHandler(t *testing.T) {
	f := newFixture(t, true)
	srv := httptest.NewServer(HTTPHandler(New(Config{DB: f.db, ReadOnly: true})))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": gomcp.LATEST_PROTOCOL_VERSION,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "t", "version": "1"},
		},
	})
	req, err := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(data))
	require.Contains(t, string(data), `"name":"chizu"`)
}

func TestListFilesTool(t *testing.T) {
	f := newFixture(t, true)

	res := f.call(t, "list_files", nil)
	require.False(t, res.IsError, text(t, res))
	out := text(t, res)
	require.Contains(t, out, filepath.Join(f.dir, "code", "main.go"))
	require.Contains(t, out, filepath.Join(f.dir, "monday.md"))
	require.Contains(t, out, "2 files")

	res = f.call(t, "list_files", map[string]any{"exts": []string{"go"}})
	out = text(t, res)
	require.Contains(t, out, "main.go")
	require.NotContains(t, out, "monday.md")

	res = f.call(t, "list_files", map[string]any{"contains": "mon", "limit": 1})
	require.Contains(t, text(t, res), "monday.md")

	res = f.call(t, "list_files", map[string]any{"limit": 1})
	require.Contains(t, text(t, res), "showing 1 of 2")

	res = f.call(t, "list_files", map[string]any{"dir": "/not/indexed"})
	require.True(t, res.IsError)
}

func TestReadFileToolWithQuery(t *testing.T) {
	f := newFixture(t, false)
	path := filepath.Join(f.dir, "monday.md")

	res := f.call(t, "read_file", map[string]any{"path": path, "query": "line"})
	require.False(t, res.IsError, text(t, res))
	require.Equal(t, "3: line three\n4: line four\n2 matching lines", text(t, res))

	// The line range narrows which matches are returned.
	res = f.call(t, "read_file", map[string]any{"path": path, "query": "line", "start_line": 4})
	require.Equal(t, "4: line four\n1 matching line", text(t, res))

	res = f.call(t, "read_file", map[string]any{"path": path, "query": "heron OR four", "raw": true})
	require.Equal(t, "2: Saw a heron.\n4: line four\n2 matching lines", text(t, res))

	res = f.call(t, "read_file", map[string]any{"path": path, "query": "zebra"})
	require.False(t, res.IsError)
	require.Equal(t, "No matching lines.", text(t, res))

	res = f.call(t, "read_file", map[string]any{"path": path, "query": "foo AND (", "raw": true})
	require.True(t, res.IsError)
}

func TestSearchToolMaxTokens(t *testing.T) {
	f := newFixture(t, false)

	full := text(t, f.call(t, "search", map[string]any{"query": "heron"}))
	require.NotContains(t, full, "token budget")

	res := f.call(t, "search", map[string]any{"query": "heron", "max_tokens": 1})
	require.False(t, res.IsError, text(t, res))
	require.Equal(t, "\n[token budget of 1 reached: 2 dropped; narrow the query or raise max_tokens]", text(t, res))

	res = f.call(t, "search", map[string]any{"query": "heron", "max_tokens": search.EstimateTokens(full)})
	require.Equal(t, full, text(t, res), "a budget the results fit in changes nothing")
}

func TestFindDefinitionTool(t *testing.T) {
	f := newFixture(t, false)
	mainGo := filepath.Join(f.dir, "code", "main.go")

	res := f.call(t, "find_definition", map[string]any{"name": "open"})
	require.False(t, res.IsError, text(t, res))
	out := text(t, res)
	require.Contains(t, out, mainGo+":5 (file_id")
	require.Contains(t, out, "method Store.Open (lines 5-7)")
	require.Contains(t, out, "func (s *Store) Open(path string) error {")
	require.NotContains(t, out, "return nil", "no body unless asked")

	out = text(t, f.call(t, "find_definition", map[string]any{"name": "Store.Open", "body": true}))
	require.Contains(t, out, "6: \treturn nil")
	require.Contains(t, out, "7: }")

	require.Contains(t, text(t, f.call(t, "find_definition", map[string]any{"name": "Other.Open"})), "No definitions named Other.Open")

	out = text(t, f.call(t, "find_definition", map[string]any{"name": "ope"}))
	require.Contains(t, out, "No exact match for ope; names containing it:")
	require.Contains(t, out, "Store.Open")

	out = text(t, f.call(t, "find_definition", map[string]any{"name": "Store", "kinds": []string{"function"}}))
	require.Contains(t, out, "No definitions named Store")

	res = f.call(t, "find_definition", map[string]any{"name": "ope", "max_tokens": 1})
	require.False(t, res.IsError)
	require.Equal(t, "No exact match for ope; names containing it:\n\n[token budget of 1 reached: 1 dropped; narrow the query or raise max_tokens]", text(t, res))
}

func TestFileOutlineTool(t *testing.T) {
	f := newFixture(t, false)

	res := f.call(t, "file_outline", map[string]any{"path": filepath.Join(f.dir, "code", "main.go")})
	require.False(t, res.IsError, text(t, res))
	out := text(t, res)
	require.Contains(t, out, "3-3 struct Store\n")
	require.Contains(t, out, "5-7 method Open\n")
	require.Contains(t, out, "9-9 function main\n")

	res = f.call(t, "file_outline", map[string]any{"path": filepath.Join(f.dir, "monday.md")})
	require.False(t, res.IsError)
	require.Contains(t, text(t, res), "No definitions found (4 lines)")

	require.True(t, f.call(t, "file_outline", map[string]any{}).IsError)
}

func TestReadFileToolMaxTokens(t *testing.T) {
	f := newFixture(t, false)
	path := filepath.Join(f.dir, "monday.md")

	out := text(t, f.call(t, "read_file", map[string]any{"path": path, "max_tokens": 1000}))
	require.Equal(t, "# Monday\nSaw a heron.\nline three\nline four", out)

	out = text(t, f.call(t, "read_file", map[string]any{"path": path, "max_tokens": 7}))
	require.Equal(t, "# Monday\nSaw a heron.\n[token budget of 7 reached at line 2 of 4; continue with start_line=3]", out)

	out = text(t, f.call(t, "read_file", map[string]any{"path": path, "start_line": 3, "max_tokens": 1}))
	require.Equal(t, "line three\n[token budget of 1 reached at line 3 of 4; continue with start_line=4]", out, "at least one line")
}
