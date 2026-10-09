// Package search runs queries against the index and shapes the results.
package search

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"chizu/src/database"
)

const (
	DefaultLimit    = 20
	DefaultMaxLines = 5
	// maxLineLen is how many bytes of a matching line are returned.
	maxLineLen = 300
)

var (
	ErrEmptyQuery = errors.New("empty query")
	ErrBadQuery   = errors.New("invalid query")
	ErrNotIndexed = errors.New("directory is not in the index")
)

type Query struct {
	Text string `json:"query"`
	// Raw passes Text to FTS5 as-is instead of quoting each term.
	Raw bool `json:"raw,omitempty"`
	// Dir limits results to an indexed directory or any directory inside one.
	Dir string `json:"dir,omitempty"`
	// PathPrefix limits results to files whose root-relative path starts with it.
	PathPrefix string   `json:"path_prefix,omitempty"`
	Exts       []string `json:"exts,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Offset     int      `json:"offset,omitempty"`
	// MaxLines is how many matching lines to return per file.
	MaxLines int `json:"max_lines,omitempty"`
}

// Segment is a piece of text that either matched the query or did not.
type Segment struct {
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
}

type Line struct {
	Number   int       `json:"line"`
	Segments []Segment `json:"segments"`
}

type Result struct {
	FileID  int64     `json:"file_id"`
	Root    string    `json:"root"`
	Path    string    `json:"path"`
	AbsPath string    `json:"abs_path"`
	Score   float64   `json:"score"`
	Snippet []Segment `json:"snippet"`
	// LineCount is the total number of matching lines; Lines holds the first MaxLines.
	LineCount int    `json:"line_count"`
	Lines     []Line `json:"lines"`
}

// Search runs q and returns the best matching files.
func Search(ctx context.Context, db *database.DB, q Query) ([]Result, error) {

	match, err := BuildMatch(q.Text, q.Raw)
	if err != nil {
		return nil, err
	}

	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.MaxLines <= 0 {
		q.MaxLines = DefaultMaxLines
	}

	filter, err := buildFilter(ctx, db, q.Dir, q.PathPrefix, "", q.Exts)
	if err != nil {
		return nil, err
	}

	params := database.SearchParams{
		Match:  match,
		Filter: filter,
		Limit:  q.Limit,
		Offset: max(q.Offset, 0),
	}

	hits, err := db.SearchFiles(ctx, params)
	if err != nil {
		return nil, queryError(err)
	}

	results := make([]Result, 0, len(hits))

	for _, h := range hits {

		marked, err := db.Highlight(ctx, match, h.FileID)
		if err != nil {
			return nil, queryError(err)
		}

		lines, count := matchingLines(marked, q.MaxLines)

		results = append(results, Result{
			FileID:    h.FileID,
			Root:      h.RootPath,
			Path:      h.Path,
			AbsPath:   filepath.Join(h.RootPath, filepath.FromSlash(h.Path)),
			Score:     -h.Rank,
			Snippet:   parseMarked(h.Snippet),
			LineCount: count,
			Lines:     lines,
		})
	}

	return results, nil
}

// buildFilter normalizes the filters shared by Search and ListFiles.
func buildFilter(ctx context.Context, db *database.DB, dir, pathPrefix, contains string, exts []string) (database.Filter, error) {

	f := database.Filter{PathPrefix: pathPrefix, Contains: contains}

	for _, e := range exts {
		if e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), ".")); e != "" {
			f.Exts = append(f.Exts, e)
		}
	}

	if dir != "" {
		var err error
		if f.Scopes, err = scopesFor(ctx, db, dir); err != nil {
			return f, err
		}
	}

	return f, nil
}

type FileQuery struct {
	// Dir limits the list to an indexed directory or any directory inside one.
	Dir string `json:"dir,omitempty"`
	// PathPrefix limits the list to files whose root-relative path starts with it.
	PathPrefix string `json:"path_prefix,omitempty"`
	// Contains limits the list to files whose root-relative path contains it, ignoring case.
	Contains string   `json:"contains,omitempty"`
	Exts     []string `json:"exts,omitempty"`
	// Limit <= 0 lists every file.
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

type FileEntry struct {
	FileID  int64  `json:"file_id"`
	Root    string `json:"root"`
	Path    string `json:"path"`
	AbsPath string `json:"abs_path"`
	Ext     string `json:"ext"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

type FileList struct {
	// Total is how many files match, ignoring Limit and Offset.
	Total int         `json:"total"`
	Files []FileEntry `json:"files"`
}

// ListFiles lists indexed files sorted by root and path.
func ListFiles(ctx context.Context, db *database.DB, q FileQuery) (FileList, error) {

	filter, err := buildFilter(ctx, db, q.Dir, q.PathPrefix, q.Contains, q.Exts)
	if err != nil {
		return FileList{}, err
	}

	infos, total, err := db.ListFiles(ctx, database.ListParams{Filter: filter, Limit: q.Limit, Offset: q.Offset})
	if err != nil {
		return FileList{}, err
	}

	list := FileList{Total: int(total), Files: make([]FileEntry, len(infos))}
	for i, f := range infos {
		list.Files[i] = FileEntry{
			FileID:  f.ID,
			Root:    f.RootPath,
			Path:    f.Path,
			AbsPath: filepath.Join(f.RootPath, filepath.FromSlash(f.Path)),
			Ext:     f.Ext,
			Size:    f.Size,
			ModTime: f.ModTime,
		}
	}

	return list, nil
}

// queryError marks FTS5 syntax problems as ErrBadQuery.
func queryError(err error) error {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"fts5", "syntax error", "no such column", "unterminated", "unknown special query"} {
		if strings.Contains(msg, s) {
			return fmt.Errorf("%w: %v", ErrBadQuery, err)
		}
	}
	return err
}

// scopesFor maps a directory onto the roots that contain it or are inside it.
func scopesFor(ctx context.Context, db *database.DB, dir string) ([]database.Scope, error) {

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}

	roots, err := db.ListRoots(ctx)
	if err != nil {
		return nil, err
	}

	var scopes []database.Scope

	for _, r := range roots {
		switch {
		case abs == r.Path || isUnder(r.Path, abs):
			scopes = append(scopes, database.Scope{RootID: r.ID})
		case isUnder(abs, r.Path):
			rel, err := filepath.Rel(r.Path, abs)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, database.Scope{RootID: r.ID, Prefix: filepath.ToSlash(rel) + "/"})
		}
	}

	if len(scopes) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotIndexed, abs)
	}

	return scopes, nil
}

// isUnder reports whether path is strictly inside dir.
func isUnder(path, dir string) bool {
	return strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// BuildMatch turns user input into an FTS5 query. In simple mode every term
// is quoted so punctuation and keywords are searched literally; "quoted
// phrases" are kept together and a trailing * makes a term a prefix search.
// Raw mode passes the input through unchanged.
func BuildMatch(text string, raw bool) (string, error) {

	text = strings.TrimSpace(text)

	if raw {
		if text == "" {
			return "", ErrEmptyQuery
		}
		return text, nil
	}

	var terms []string

	quote := func(term string) {
		prefix := false
		if strings.HasSuffix(term, "*") {
			term = strings.TrimRight(term, "*")
			prefix = true
		}
		if strings.TrimSpace(term) == "" {
			return
		}
		q := `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
		if prefix {
			q += "*"
		}
		terms = append(terms, q)
	}

	for text != "" {
		if text[0] == '"' {
			end := strings.IndexByte(text[1:], '"')
			if end < 0 {
				quote(text[1:])
				break
			}
			quote(text[1 : 1+end])
			text = strings.TrimSpace(text[end+2:])
			continue
		}

		end := strings.IndexAny(text, " \t\r\n")
		if end < 0 {
			end = len(text)
		}
		quote(text[:end])
		text = strings.TrimSpace(text[end:])
	}

	if len(terms) == 0 {
		return "", ErrEmptyQuery
	}

	return strings.Join(terms, " "), nil
}

// parseMarked splits text with MarkStart/MarkEnd markers into segments.
func parseMarked(s string) []Segment {

	var segs []Segment
	match := false

	for s != "" {
		marker := database.MarkStart
		if match {
			marker = database.MarkEnd
		}

		i := strings.Index(s, marker)
		if i < 0 {
			i = len(s)
		}
		if i > 0 {
			segs = append(segs, Segment{Text: s[:i], Match: match})
		}
		if i == len(s) {
			break
		}
		s = s[i+len(marker):]
		match = !match
	}

	return segs
}

// matchingLines returns up to maxLines lines containing a match, and how
// many lines matched in total. Line numbers start at 1.
func matchingLines(marked string, maxLines int) ([]Line, int) {

	var lines []Line
	count := 0

	for i, segs := range splitLines(parseMarked(marked)) {

		hasMatch := false
		for _, s := range segs {
			if s.Match {
				hasMatch = true
				break
			}
		}
		if !hasMatch {
			continue
		}

		count++
		if len(lines) < maxLines {
			lines = append(lines, Line{Number: i + 1, Segments: cropLine(segs)})
		}
	}

	return lines, count
}

// splitLines splits segments at newlines, keeping each piece's match flag.
func splitLines(segs []Segment) [][]Segment {

	lines := [][]Segment{nil}

	for _, s := range segs {
		parts := strings.Split(s.Text, "\n")
		for j, p := range parts {
			if j > 0 {
				lines = append(lines, nil)
			}
			p = strings.TrimSuffix(p, "\r")
			if p != "" {
				lines[len(lines)-1] = append(lines[len(lines)-1], Segment{Text: p, Match: s.Match})
			}
		}
	}

	return lines
}

// cropLine shortens a long line to a window around its first match.
func cropLine(segs []Segment) []Segment {

	total, firstMatch := 0, -1
	for _, s := range segs {
		if s.Match && firstMatch < 0 {
			firstMatch = total
		}
		total += len(s.Text)
	}

	if total <= maxLineLen {
		return segs
	}

	start := max(0, firstMatch-maxLineLen/3)
	end := min(total, start+maxLineLen)

	var out []Segment
	if start > 0 {
		out = append(out, Segment{Text: "…"})
	}

	pos := 0
	for _, s := range segs {
		segStart, segEnd := pos, pos+len(s.Text)
		pos = segEnd

		lo, hi := max(segStart, start), min(segEnd, end)
		if lo >= hi {
			continue
		}

		text := s.Text[lo-segStart : hi-segStart]
		text = strings.ToValidUTF8(text, "")
		if text != "" && utf8.ValidString(text) {
			out = append(out, Segment{Text: text, Match: s.Match})
		}
	}

	if end < total {
		out = append(out, Segment{Text: "…"})
	}

	return out
}

// GetFile returns an indexed file by its absolute path.
func GetFile(ctx context.Context, db *database.DB, path string) (database.File, error) {

	abs, err := filepath.Abs(path)
	if err != nil {
		return database.File{}, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}

	roots, err := db.ListRoots(ctx)
	if err != nil {
		return database.File{}, err
	}

	// Nested roots: prefer the deepest root containing the file.
	var best *database.Root
	for i, r := range roots {
		if isUnder(abs, r.Path) && (best == nil || len(r.Path) > len(best.Path)) {
			best = &roots[i]
		}
	}
	if best == nil {
		return database.File{}, fmt.Errorf("%w: %s is not inside an indexed directory", database.ErrNotFound, abs)
	}

	rel, err := filepath.Rel(best.Path, abs)
	if err != nil {
		return database.File{}, err
	}

	return db.GetFileByPath(ctx, best.ID, filepath.ToSlash(rel))
}

// GetFileByID returns an indexed file by its id.
func GetFileByID(ctx context.Context, db *database.DB, id int64) (database.File, error) {
	return db.GetFile(ctx, id)
}
