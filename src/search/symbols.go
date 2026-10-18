package search

import (
	"context"
	"path/filepath"
	"strings"

	"chizu/src/database"
)

const (
	DefaultDefLimit = 10
	// maxBodyLines is how many lines of a definition's body are returned.
	maxBodyLines = 300
)

type DefQuery struct {
	// Name is a symbol name, optionally qualified by its parent:
	// Store.UpsertRoot, Store::UpsertRoot or Store#UpsertRoot.
	Name string `json:"name"`
	// Kinds limits results to these kinds (function, method, class, ...).
	Kinds []string `json:"kinds,omitempty"`
	// Dir limits results to an indexed directory or any directory inside one.
	Dir string `json:"dir,omitempty"`
	// PathPrefix limits results to files whose root-relative path starts with it.
	PathPrefix string   `json:"path_prefix,omitempty"`
	Exts       []string `json:"exts,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Offset     int      `json:"offset,omitempty"`
	// Body includes each definition's source lines.
	Body bool `json:"body,omitempty"`
}

type Definition struct {
	FileID    int64  `json:"file_id"`
	Root      string `json:"root"`
	Path      string `json:"path"`
	AbsPath   string `json:"abs_path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Parent    string `json:"parent,omitempty"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`
	Signature string `json:"signature"`
	// Exact is false when no name matched exactly and this name only contains the query.
	Exact bool `json:"exact"`
	// Body is the source of lines Line..EndLine (at most maxBodyLines), if asked for.
	Body []string `json:"body,omitempty"`
}

// QualifiedName is the name with its parent, if it has one.
func (d Definition) QualifiedName() string {
	if d.Parent == "" {
		return d.Name
	}
	return d.Parent + "." + d.Name
}

// splitQualified splits "Parent.Name" (or ::, #, ->) into parent and name.
func splitQualified(s string) (parent, name string) {
	best, sepLen := -1, 0
	for _, sep := range []string{"::", ".", "#", "->"} {
		if i := strings.LastIndex(s, sep); i > best {
			best, sepLen = i, len(sep)
		}
	}
	if best <= 0 || best+sepLen >= len(s) {
		return "", s
	}
	return s[:best], s[best+sepLen:]
}

// Definitions finds where symbols named q.Name are defined. Exact (case
// insensitive) matches are returned if there are any; otherwise names
// containing q.Name.
func Definitions(ctx context.Context, db *database.DB, q DefQuery) ([]Definition, error) {

	text := strings.TrimSpace(q.Name)
	if text == "" {
		return nil, ErrEmptyQuery
	}

	if q.Limit <= 0 {
		q.Limit = DefaultDefLimit
	}

	filter, err := buildFilter(ctx, db, q.Dir, q.PathPrefix, "", q.Exts)
	if err != nil {
		return nil, err
	}

	parent, name := splitQualified(text)

	params := database.SymbolParams{
		Name:   name,
		Parent: parent,
		Kinds:  q.Kinds,
		Filter: filter,
		Limit:  q.Limit,
		Offset: max(q.Offset, 0),
	}

	hits, err := db.FindSymbols(ctx, params)
	if err != nil {
		return nil, err
	}

	exact := true
	if len(hits) == 0 && q.Offset <= 0 {
		exact = false
		params.Substring = true
		if hits, err = db.FindSymbols(ctx, params); err != nil {
			return nil, err
		}
	}

	defs := make([]Definition, len(hits))
	contents := map[int64][]string{}

	for i, h := range hits {
		defs[i] = Definition{
			FileID:    h.FileID,
			Root:      h.RootPath,
			Path:      h.Path,
			AbsPath:   filepath.Join(h.RootPath, filepath.FromSlash(h.Path)),
			Name:      h.Name,
			Kind:      h.Kind,
			Parent:    h.Parent,
			Line:      h.Line,
			EndLine:   h.EndLine,
			Signature: h.Signature,
			Exact:     exact,
		}

		if !q.Body {
			continue
		}

		lines, ok := contents[h.FileID]
		if !ok {
			f, err := db.GetFile(ctx, h.FileID)
			if err != nil {
				return nil, err
			}
			lines = strings.Split(f.Content, "\n")
			contents[h.FileID] = lines
		}
		defs[i].Body = sourceLines(lines, h.Line, min(h.EndLine, h.Line+maxBodyLines-1))
	}

	return defs, nil
}

// sourceLines returns lines start..end (1-based, inclusive) without trailing CRs.
func sourceLines(lines []string, start, end int) []string {
	start, end = max(start, 1), min(end, len(lines))
	if start > end {
		return nil
	}
	out := make([]string, 0, end-start+1)
	for _, l := range lines[start-1 : end] {
		out = append(out, strings.TrimSuffix(l, "\r"))
	}
	return out
}

// OutlineEntry is a symbol and how deeply it is nested in other symbols.
type OutlineEntry struct {
	database.Symbol
	Depth int `json:"depth"`
}

// Outline lists the definitions in a file in source order.
func Outline(ctx context.Context, db *database.DB, fileID int64) ([]OutlineEntry, error) {

	syms, err := db.FileSymbols(ctx, fileID)
	if err != nil {
		return nil, err
	}

	out := make([]OutlineEntry, len(syms))
	var open []database.Symbol // enclosing symbols of the current one

	for i, s := range syms {
		for len(open) > 0 && !contains(open[len(open)-1], s) {
			open = open[:len(open)-1]
		}
		out[i] = OutlineEntry{Symbol: s, Depth: len(open)}
		open = append(open, s)
	}

	return out, nil
}

// contains reports whether outer's lines strictly contain inner's.
func contains(outer, inner database.Symbol) bool {
	same := outer.Line == inner.Line && outer.EndLine == inner.EndLine
	return !same && outer.Line <= inner.Line && inner.EndLine <= outer.EndLine
}
