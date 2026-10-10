// Package mcp exposes chizu as a Model Context Protocol server, over stdio
// or streamable HTTP.
package mcp

import (
	"chizu/src/version"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"chizu/src/database"
	"chizu/src/indexer"
	"chizu/src/search"

	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
)

const instructions = `chizu is a full text index of local directories.
Use search to find files by content (results include matching line numbers),
then read_file to read a file or a range of its lines. list_roots shows which
directories are indexed and list_files lists the files in them.`

// defaultListLimit keeps list_files from flooding the model's context.
const defaultListLimit = 200

type Config struct {
	DB *database.DB
	// DBPath is never indexed.
	DBPath string
	// ReadOnly leaves out the index and remove_root tools.
	ReadOnly bool
	Log      zerolog.Logger
}

type handlers struct {
	Config
}

// New builds the MCP server and its tools.
func New(cfg Config) *server.MCPServer {

	h := &handlers{Config: cfg}

	s := server.NewMCPServer("chizu", version.Version,
		server.WithToolCapabilities(false),
		server.WithInstructions(instructions),
	)

	s.AddTool(gomcp.NewTool("search",
		gomcp.WithDescription(`Full text search over the indexed files, best matches first.
Each word must appear in the file (stemmed, so "running" also finds "run"). Use "double quotes" for phrases and a trailing * for prefixes.
Set raw=true for SQLite FTS5 syntax: AND, OR, NOT, NEAR(a b, 5), path:term.`),
		gomcp.WithReadOnlyHintAnnotation(true),
		gomcp.WithString("query", gomcp.Required(), gomcp.Description("search terms")),
		gomcp.WithBoolean("raw", gomcp.Description("treat query as FTS5 syntax")),
		gomcp.WithString("dir", gomcp.Description("only search inside this absolute directory")),
		gomcp.WithString("path_prefix", gomcp.Description("only files whose path relative to its indexed root starts with this")),
		gomcp.WithArray("exts", gomcp.WithStringItems(), gomcp.Description(`only these file extensions, e.g. ["md","txt"]`)),
		gomcp.WithNumber("limit", gomcp.Description(fmt.Sprintf("max files (default %d)", search.DefaultLimit))),
		gomcp.WithNumber("offset", gomcp.Description("skip this many files, for paging")),
		gomcp.WithNumber("max_lines", gomcp.Description(fmt.Sprintf("max matching lines per file (default %d)", search.DefaultMaxLines))),
	), h.search)

	s.AddTool(gomcp.NewTool("list_roots",
		gomcp.WithDescription("List the indexed directories."),
		gomcp.WithReadOnlyHintAnnotation(true),
	), h.listRoots)

	s.AddTool(gomcp.NewTool("list_files",
		gomcp.WithDescription("List indexed files by path, sorted by directory and path. Filter by name to find files without searching their content."),
		gomcp.WithReadOnlyHintAnnotation(true),
		gomcp.WithString("contains", gomcp.Description("only files whose path contains this, ignoring case")),
		gomcp.WithString("dir", gomcp.Description("only files inside this absolute directory")),
		gomcp.WithString("path_prefix", gomcp.Description("only files whose path relative to its indexed root starts with this")),
		gomcp.WithArray("exts", gomcp.WithStringItems(), gomcp.Description(`only these file extensions, e.g. ["md","txt"]`)),
		gomcp.WithNumber("limit", gomcp.Description(fmt.Sprintf("max files (default %d)", defaultListLimit))),
		gomcp.WithNumber("offset", gomcp.Description("skip this many files, for paging")),
	), h.listFiles)

	s.AddTool(gomcp.NewTool("read_file",
		gomcp.WithDescription(`Read an indexed file by absolute path or file_id, optionally only a range of lines.
With query, return only the lines that match it (all of them, uncropped, prefixed with line numbers) instead of the content.`),
		gomcp.WithReadOnlyHintAnnotation(true),
		gomcp.WithString("path", gomcp.Description("absolute path of the file")),
		gomcp.WithNumber("file_id", gomcp.Description("file id from search results")),
		gomcp.WithNumber("start_line", gomcp.Description("first line to return (1-based)")),
		gomcp.WithNumber("end_line", gomcp.Description("last line to return (inclusive)")),
		gomcp.WithString("query", gomcp.Description("only return lines matching this search, same syntax as the search tool")),
		gomcp.WithBoolean("raw", gomcp.Description("treat query as FTS5 syntax")),
	), h.readFile)

	if !cfg.ReadOnly {
		s.AddTool(gomcp.NewTool("index",
			gomcp.WithDescription(`Index directories, or re-index every indexed directory when dirs is empty.
Re-indexing is incremental. Filters are saved per directory and reused when none are given.`),
			gomcp.WithDestructiveHintAnnotation(false),
			gomcp.WithArray("dirs", gomcp.WithStringItems(), gomcp.Description("absolute directories to index")),
			gomcp.WithArray("exts", gomcp.WithStringItems(), gomcp.Description("only index these extensions")),
			gomcp.WithArray("include", gomcp.WithStringItems(), gomcp.Description("only index files matching these gitignore-style patterns")),
			gomcp.WithArray("exclude", gomcp.WithStringItems(), gomcp.Description("skip paths matching these gitignore-style patterns")),
			gomcp.WithNumber("max_size", gomcp.Description("skip files larger than this many bytes")),
			gomcp.WithBoolean("clear_filters", gomcp.Description("forget saved filters")),
		), h.index)

		s.AddTool(gomcp.NewTool("remove_root",
			gomcp.WithDescription("Remove an indexed directory and its files from the index (files on disk are not touched)."),
			gomcp.WithDestructiveHintAnnotation(true),
			gomcp.WithString("path", gomcp.Required(), gomcp.Description("the indexed directory")),
		), h.removeRoot)
	}

	return s
}

// ServeStdio serves MCP over the given streams until ctx is done or in closes.
func ServeStdio(ctx context.Context, s *server.MCPServer, in io.Reader, out io.Writer) error {
	return server.NewStdioServer(s).Listen(ctx, in, out)
}

// HTTPHandler serves MCP over streamable HTTP.
func HTTPHandler(s *server.MCPServer) http.Handler {
	return server.NewStreamableHTTPServer(s)
}

func (h *handlers) search(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {

	query, err := req.RequireString("query")
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	results, err := search.Search(ctx, h.DB, search.Query{
		Text:       query,
		Raw:        req.GetBool("raw", false),
		Dir:        req.GetString("dir", ""),
		PathPrefix: req.GetString("path_prefix", ""),
		Exts:       req.GetStringSlice("exts", nil),
		Limit:      req.GetInt("limit", 0),
		Offset:     req.GetInt("offset", 0),
		MaxLines:   req.GetInt("max_lines", 0),
	})
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	if len(results) == 0 {
		return gomcp.NewToolResultStructured(map[string]any{"results": results}, "No results."), nil
	}

	var sb strings.Builder
	for i, r := range results {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "%s (file_id %d)\n", r.AbsPath, r.FileID)
		for _, l := range r.Lines {
			fmt.Fprintf(&sb, "%d: %s\n", l.Number, plain(l.Segments))
		}
		if len(r.Lines) == 0 {
			fmt.Fprintf(&sb, "%s\n", plain(r.Snippet))
		}
		if more := r.LineCount - len(r.Lines); more > 0 {
			fmt.Fprintf(&sb, "… %d more matching lines\n", more)
		}
	}

	return gomcp.NewToolResultStructured(map[string]any{"results": results}, sb.String()), nil
}

func plain(segs []search.Segment) string {
	var sb strings.Builder
	for _, s := range segs {
		sb.WriteString(s.Text)
	}
	return sb.String()
}

func (h *handlers) listRoots(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {

	roots, err := h.DB.ListRoots(ctx)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	for _, r := range roots {
		fmt.Fprintf(&sb, "%s (%d files)\n", r.Path, r.FileCount)
	}
	if len(roots) == 0 {
		sb.WriteString("Nothing is indexed.")
	}

	return gomcp.NewToolResultStructured(map[string]any{"roots": roots}, sb.String()), nil
}

func (h *handlers) listFiles(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {

	q := search.FileQuery{
		Contains:   req.GetString("contains", ""),
		Dir:        req.GetString("dir", ""),
		PathPrefix: req.GetString("path_prefix", ""),
		Exts:       req.GetStringSlice("exts", nil),
		Limit:      req.GetInt("limit", defaultListLimit),
		Offset:     req.GetInt("offset", 0),
	}
	if q.Limit <= 0 {
		q.Limit = defaultListLimit
	}

	list, err := search.ListFiles(ctx, h.DB, q)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	for _, f := range list.Files {
		fmt.Fprintf(&sb, "%s (file_id %d, %d bytes)\n", f.AbsPath, f.FileID, f.Size)
	}
	switch {
	case list.Total == 0:
		sb.WriteString("No files.")
	case len(list.Files) < list.Total:
		fmt.Fprintf(&sb, "showing %d of %d files from offset %d\n", len(list.Files), list.Total, max(q.Offset, 0))
	default:
		fmt.Fprintf(&sb, "%d files\n", list.Total)
	}

	return gomcp.NewToolResultStructured(list, sb.String()), nil
}

func (h *handlers) readFile(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {

	var f database.File
	var err error

	args := req.GetArguments()
	switch {
	case args["file_id"] != nil:
		f, err = search.GetFileByID(ctx, h.DB, int64(req.GetInt("file_id", 0)))
	case req.GetString("path", "") != "":
		f, err = search.GetFile(ctx, h.DB, req.GetString("path", ""))
	default:
		return gomcp.NewToolResultError("path or file_id is required"), nil
	}

	if errors.Is(err, database.ErrNotFound) {
		return gomcp.NewToolResultError("file is not in the index: " + err.Error()), nil
	}
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	start, end := req.GetInt("start_line", 0), req.GetInt("end_line", 0)

	if query := req.GetString("query", ""); query != "" {
		return h.matchingLines(ctx, f, query, req.GetBool("raw", false), start, end)
	}

	content := f.Content

	if start > 0 || end > 0 {
		lines := strings.Split(content, "\n")
		start = max(start, 1)
		if end <= 0 || end > len(lines) {
			end = len(lines)
		}
		if start > end {
			return gomcp.NewToolResultError("start_line is past the end of the file (" + strconv.Itoa(len(lines)) + " lines)"), nil
		}
		content = strings.Join(lines[start-1:end], "\n")
	}

	return gomcp.NewToolResultText(content), nil
}

// matchingLines answers read_file with a query: the matching lines within start..end (0 means unbounded).
func (h *handlers) matchingLines(ctx context.Context, f database.File, query string, raw bool, start, end int) (*gomcp.CallToolResult, error) {

	all, err := search.FileMatches(ctx, h.DB, f.ID, query, raw)
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	lines := []search.Line{}
	for _, l := range all {
		if (start <= 0 || l.Number >= start) && (end <= 0 || l.Number <= end) {
			lines = append(lines, l)
		}
	}

	if len(lines) == 0 {
		return gomcp.NewToolResultStructured(map[string]any{"matches": lines}, "No matching lines."), nil
	}

	var sb strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&sb, "%d: %s\n", l.Number, plain(l.Segments))
	}
	if len(lines) == 1 {
		sb.WriteString("1 matching line")
	} else {
		fmt.Fprintf(&sb, "%d matching lines", len(lines))
	}

	return gomcp.NewToolResultStructured(map[string]any{"matches": lines}, sb.String()), nil
}

func (h *handlers) index(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {

	opts := indexer.Options{
		Exts:         req.GetStringSlice("exts", nil),
		Include:      req.GetStringSlice("include", nil),
		Exclude:      req.GetStringSlice("exclude", nil),
		MaxSize:      int64(req.GetInt("max_size", 0)),
		ClearFilters: req.GetBool("clear_filters", false),
		Log:          h.Log,
	}
	if h.DBPath != "" {
		opts.SkipPaths = []string{h.DBPath, h.DBPath + "-wal", h.DBPath + "-shm", h.DBPath + "-journal"}
	}

	var all []indexer.Stats
	var errs []error

	dirs := req.GetStringSlice("dirs", nil)
	if len(dirs) == 0 {
		stats, err := indexer.IndexAll(ctx, h.DB, opts)
		all = stats
		if err != nil {
			errs = append(errs, err)
		}
	} else {
		for _, dir := range dirs {
			st, err := indexer.Index(ctx, h.DB, dir, opts)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", dir, err))
				continue
			}
			all = append(all, st)
		}
	}

	var sb strings.Builder
	for _, s := range all {
		fmt.Fprintf(&sb, "%s: %d added, %d updated, %d unchanged, %d removed, %d skipped\n",
			s.Root, s.Added, s.Updated, s.Unchanged, s.Removed, s.Skipped)
	}

	if err := errors.Join(errs...); err != nil {
		sb.WriteString("errors:\n" + err.Error())
		res := gomcp.NewToolResultText(sb.String())
		res.IsError = len(all) == 0
		return res, nil
	}

	return gomcp.NewToolResultText(sb.String()), nil
}

func (h *handlers) removeRoot(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {

	path, err := req.RequireString("path")
	if err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	if err := indexer.Remove(ctx, h.DB, path); err != nil {
		return gomcp.NewToolResultError(err.Error()), nil
	}

	return gomcp.NewToolResultText("removed " + path), nil
}
