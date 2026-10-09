package database

import (
	"context"
	"strings"

	"github.com/vinovest/sqlx"
)

// Highlight markers wrapped around matched text by snippet() and highlight().
const (
	MarkStart = "\x02"
	MarkEnd   = "\x03"
)

// Scope limits a search to one root, optionally under a path prefix.
type Scope struct {
	RootID int64
	Prefix string
}

// Filter narrows which files a search or listing covers.
type Filter struct {
	// Scopes, if any, limit results to files inside at least one of them.
	Scopes []Scope
	// PathPrefix limits results to files whose relative path starts with it.
	PathPrefix string
	// Contains limits results to files whose relative path contains it (ASCII case-insensitive).
	Contains string
	// Exts limits results to these extensions (lowercase, no dot).
	Exts []string
}

type SearchParams struct {
	// Match is an FTS5 query.
	Match string
	Filter
	Limit  int
	Offset int
}

type SearchHit struct {
	FileID   int64   `db:"id"`
	RootPath string  `db:"root_path"`
	Path     string  `db:"path"`
	Rank     float64 `db:"rank"`
	Snippet  string  `db:"snippet"`
}

// escapeLike escapes LIKE wildcards so s matches literally (with ESCAPE '\').
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// where appends " AND ..." conditions on files f to sb and returns args with their values.
func (p Filter) where(sb *strings.Builder, args []any) []any {

	if len(p.Scopes) > 0 {
		sb.WriteString(" AND (")
		for i, sc := range p.Scopes {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			if sc.Prefix == "" {
				sb.WriteString("f.root_id = ?")
				args = append(args, sc.RootID)
			} else {
				sb.WriteString(`(f.root_id = ? AND f.path LIKE ? ESCAPE '\')`)
				args = append(args, sc.RootID, escapeLike(sc.Prefix)+"%")
			}
		}
		sb.WriteString(")")
	}

	if p.PathPrefix != "" {
		sb.WriteString(` AND f.path LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(p.PathPrefix)+"%")
	}

	if p.Contains != "" {
		sb.WriteString(` AND f.path LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(p.Contains)+"%")
	}

	if len(p.Exts) > 0 {
		sb.WriteString(" AND f.ext IN (?" + strings.Repeat(", ?", len(p.Exts)-1) + ")")
		for _, e := range p.Exts {
			args = append(args, e)
		}
	}

	return args
}

// SearchFiles runs an FTS5 query, best matches first. Matches in a file's
// path are weighted above matches in its content.
func (s *Store) SearchFiles(ctx context.Context, p SearchParams) ([]SearchHit, error) {

	var sb strings.Builder
	args := []any{p.Match}

	sb.WriteString(`
		SELECT f.id, r.path AS root_path, f.path,
			bm25(files_fts, 5.0, 1.0) AS rank,
			snippet(files_fts, 1, char(2), char(3), '…', 24) AS snippet
		FROM files_fts
		JOIN files f ON f.id = files_fts.rowid
		JOIN roots r ON r.id = f.root_id
		WHERE files_fts MATCH ?`)

	args = p.Filter.where(&sb, args)

	sb.WriteString(" ORDER BY rank LIMIT ? OFFSET ?")
	args = append(args, p.Limit, p.Offset)

	hits := []SearchHit{}
	err := sqlx.SelectContext(ctx, s.q, &hits, sb.String(), args...)
	return hits, err
}

// Highlight returns a file's full content with matches of the FTS5 query
// wrapped in MarkStart/MarkEnd.
func (s *Store) Highlight(ctx context.Context, match string, fileID int64) (string, error) {
	var out string
	err := sqlx.GetContext(ctx, s.q, &out, `
		SELECT highlight(files_fts, 1, char(2), char(3))
		FROM files_fts WHERE files_fts MATCH ? AND rowid = ?`, match, fileID)
	return out, notFound(err)
}

type ListParams struct {
	Filter
	// Limit <= 0 means no limit.
	Limit  int
	Offset int
}

// FileInfo is a file without its content.
type FileInfo struct {
	ID       int64  `db:"id"`
	RootPath string `db:"root_path"`
	Path     string `db:"path"`
	Ext      string `db:"ext"`
	Size     int64  `db:"size"`
	ModTime  int64  `db:"mtime"`
}

// ListFiles returns the files matching p sorted by root and path, and how
// many match in total regardless of paging.
func (s *Store) ListFiles(ctx context.Context, p ListParams) ([]FileInfo, int64, error) {

	var where strings.Builder
	args := p.Filter.where(&where, nil)

	var total int64
	err := sqlx.GetContext(ctx, s.q, &total, `SELECT count(*) FROM files f WHERE 1`+where.String(), args...)
	if err != nil {
		return nil, 0, err
	}

	limit := p.Limit
	if limit <= 0 {
		limit = -1
	}

	files := []FileInfo{}
	err = sqlx.SelectContext(ctx, s.q, &files, `
		SELECT f.id, r.path AS root_path, f.path, f.ext, f.size, f.mtime
		FROM files f JOIN roots r ON r.id = f.root_id
		WHERE 1`+where.String()+`
		ORDER BY r.path, f.path LIMIT ? OFFSET ?`,
		append(args, limit, max(p.Offset, 0))...)

	return files, total, err
}
