package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/vinovest/sqlx"
)

// Store holds the queries; it runs against either the DB or a transaction.
type Store struct {
	q sqlx.ExtContext
}

type Root struct {
	ID        int64  `db:"id" json:"id"`
	Path      string `db:"path" json:"path"`
	IndexedAt int64  `db:"indexed_at" json:"indexed_at"`
	FileCount int64  `db:"file_count" json:"file_count"`
	// Options is the JSON-encoded indexer filters used for this root.
	Options string `db:"options" json:"-"`
}

type File struct {
	ID       int64  `db:"id" json:"id"`
	RootID   int64  `db:"root_id" json:"root_id"`
	RootPath string `db:"root_path" json:"root"`
	Path     string `db:"path" json:"path"`
	Ext      string `db:"ext" json:"ext"`
	Size     int64  `db:"size" json:"size"`
	ModTime  int64  `db:"mtime" json:"mtime"`
	Hash     string `db:"hash" json:"hash"`
	Content  string `db:"content" json:"content"`
	// SymbolsVersion is the symbols.Version that produced Symbols.
	SymbolsVersion int `db:"symbols_version" json:"-"`
	// Symbols are written with the file by UpsertFiles; they are not loaded by GetFile.
	Symbols []Symbol `db:"-" json:"-"`
}

// Symbol is a definition (function, type, method, ...) found in a file.
type Symbol struct {
	FileID int64  `db:"file_id" json:"file_id"`
	Name   string `db:"name" json:"name"`
	Kind   string `db:"kind" json:"kind"`
	// Parent is the enclosing definition, or the receiver type of a method.
	Parent string `db:"parent" json:"parent,omitempty"`
	// Line and EndLine are the 1-based lines the definition spans.
	Line    int `db:"line" json:"line"`
	EndLine int `db:"end_line" json:"end_line"`
	// Signature is the line the definition's name is on, trimmed.
	Signature string `db:"signature" json:"signature"`
}

// FileState is what the indexer needs to decide whether a file changed.
type FileState struct {
	ID             int64  `db:"id"`
	Path           string `db:"path"`
	Size           int64  `db:"size"`
	ModTime        int64  `db:"mtime"`
	Hash           string `db:"hash"`
	SymbolsVersion int    `db:"symbols_version"`
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) UpsertRoot(ctx context.Context, path string) (Root, error) {
	var r Root
	err := sqlx.GetContext(ctx, s.q, &r, `
		INSERT INTO roots (path) VALUES (?)
		ON CONFLICT (path) DO UPDATE SET path = excluded.path
		RETURNING id, path, indexed_at, options`, path)
	return r, err
}

func (s *Store) GetRoot(ctx context.Context, path string) (Root, error) {
	var r Root
	err := sqlx.GetContext(ctx, s.q, &r, `
		SELECT r.id, r.path, r.indexed_at, r.options, (SELECT count(*) FROM files f WHERE f.root_id = r.id) AS file_count
		FROM roots r WHERE r.path = ?`, path)
	return r, notFound(err)
}

func (s *Store) ListRoots(ctx context.Context) ([]Root, error) {
	roots := []Root{}
	err := sqlx.SelectContext(ctx, s.q, &roots, `
		SELECT r.id, r.path, r.indexed_at, r.options, (SELECT count(*) FROM files f WHERE f.root_id = r.id) AS file_count
		FROM roots r ORDER BY r.path`)
	return roots, err
}

func (s *Store) SetRootIndexedAt(ctx context.Context, id, unix int64) error {
	_, err := s.q.ExecContext(ctx, `UPDATE roots SET indexed_at = ? WHERE id = ?`, unix, id)
	return err
}

func (s *Store) SetRootOptions(ctx context.Context, id int64, options string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE roots SET options = ? WHERE id = ?`, options, id)
	return err
}

func (s *Store) DeleteRoot(ctx context.Context, id int64) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM roots WHERE id = ?`, id)
	return err
}

// UpsertFile inserts or replaces the file at (RootID, Path), and its symbols, and returns its id.
func (s *Store) UpsertFile(ctx context.Context, f File) (int64, error) {
	ids, err := s.UpsertFiles(ctx, []File{f})
	if err != nil {
		return 0, err
	}
	return ids[f.Path], nil
}

// batchRows is how many rows go in one statement. FTS5 flushes its pending
// index data at the start of every write statement, so one statement per
// file is very slow; this keeps well under SQLite's variable limit.
const batchRows = 500

// UpsertFiles inserts or replaces many files, and their symbols, using as
// few statements as possible. It returns the files' ids by path.
func (s *Store) UpsertFiles(ctx context.Context, files []File) (map[string]int64, error) {

	ids := make(map[string]int64, len(files))

	for len(files) > 0 {
		n := min(len(files), batchRows)
		chunk := files[:n]
		files = files[n:]

		var sb strings.Builder
		args := make([]any, 0, len(chunk)*8)

		sb.WriteString(`INSERT INTO files (root_id, path, ext, size, mtime, hash, content, symbols_version) VALUES `)
		for i, f := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, f.RootID, f.Path, f.Ext, f.Size, f.ModTime, f.Hash, f.Content, f.SymbolsVersion)
		}
		sb.WriteString(`
			ON CONFLICT (root_id, path) DO UPDATE SET
				ext = excluded.ext, size = excluded.size, mtime = excluded.mtime,
				hash = excluded.hash, content = excluded.content, symbols_version = excluded.symbols_version
			RETURNING id, path`)

		var rows []struct {
			ID   int64  `db:"id"`
			Path string `db:"path"`
		}
		if err := sqlx.SelectContext(ctx, s.q, &rows, sb.String(), args...); err != nil {
			return nil, fmt.Errorf("upsert %d files: %w", len(chunk), err)
		}

		chunkIDs := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids[r.Path] = r.ID
			chunkIDs = append(chunkIDs, r.ID)
		}

		var syms []Symbol
		for _, f := range chunk {
			for _, sym := range f.Symbols {
				sym.FileID = ids[f.Path]
				syms = append(syms, sym)
			}
		}
		if err := s.replaceSymbols(ctx, chunkIDs, syms); err != nil {
			return nil, err
		}
	}

	return ids, nil
}

// SetFileSymbols replaces the symbols of a file whose content did not
// change, recording its new mtime and the symbols version.
func (s *Store) SetFileSymbols(ctx context.Context, id, mtime int64, version int, syms []Symbol) error {

	if _, err := s.q.ExecContext(ctx, `UPDATE files SET mtime = ?, symbols_version = ? WHERE id = ?`, mtime, version, id); err != nil {
		return err
	}

	for i := range syms {
		syms[i].FileID = id
	}
	return s.replaceSymbols(ctx, []int64{id}, syms)
}

// replaceSymbols deletes the symbols of the files in ids and inserts syms.
func (s *Store) replaceSymbols(ctx context.Context, ids []int64, syms []Symbol) error {

	if len(ids) > 0 {
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		q := `DELETE FROM symbols WHERE file_id IN (?` + strings.Repeat(", ?", len(ids)-1) + `)`
		if _, err := s.q.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("delete symbols: %w", err)
		}
	}

	for len(syms) > 0 {
		n := min(len(syms), batchRows)
		chunk := syms[:n]
		syms = syms[n:]

		var sb strings.Builder
		args := make([]any, 0, len(chunk)*7)

		sb.WriteString(`INSERT INTO symbols (file_id, name, kind, parent, line, end_line, signature) VALUES `)
		for i, sym := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?)")
			args = append(args, sym.FileID, sym.Name, sym.Kind, sym.Parent, sym.Line, sym.EndLine, sym.Signature)
		}

		if _, err := s.q.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("insert %d symbols: %w", len(chunk), err)
		}
	}

	return nil
}

// DeleteFiles removes many files by id.
func (s *Store) DeleteFiles(ctx context.Context, ids []int64) error {

	for len(ids) > 0 {
		n := min(len(ids), batchRows)
		chunk := ids[:n]
		ids = ids[n:]

		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		q := `DELETE FROM files WHERE id IN (?` + strings.Repeat(", ?", len(chunk)-1) + `)`
		if _, err := s.q.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}

	return nil
}

// TouchFile records a new mtime for a file whose content did not change.
func (s *Store) TouchFile(ctx context.Context, id, mtime int64) error {
	_, err := s.q.ExecContext(ctx, `UPDATE files SET mtime = ? WHERE id = ?`, mtime, id)
	return err
}

func (s *Store) DeleteFile(ctx context.Context, id int64) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	return err
}

const selectFile = `
	SELECT f.id, f.root_id, r.path AS root_path, f.path, f.ext, f.size, f.mtime, f.hash, f.content
	FROM files f JOIN roots r ON r.id = f.root_id`

func (s *Store) GetFile(ctx context.Context, id int64) (File, error) {
	var f File
	err := sqlx.GetContext(ctx, s.q, &f, selectFile+` WHERE f.id = ?`, id)
	return f, notFound(err)
}

func (s *Store) GetFileByPath(ctx context.Context, rootID int64, path string) (File, error) {
	var f File
	err := sqlx.GetContext(ctx, s.q, &f, selectFile+` WHERE f.root_id = ? AND f.path = ?`, rootID, path)
	return f, notFound(err)
}

// FileStates returns the stored state of every file under a root, keyed by path.
func (s *Store) FileStates(ctx context.Context, rootID int64) (map[string]FileState, error) {
	var rows []FileState
	err := sqlx.SelectContext(ctx, s.q, &rows, `SELECT id, path, size, mtime, hash, symbols_version FROM files WHERE root_id = ?`, rootID)
	if err != nil {
		return nil, err
	}
	states := make(map[string]FileState, len(rows))
	for _, r := range rows {
		states[r.Path] = r
	}
	return states, nil
}

// FileStatesUnder returns the stored state of the file at the slash-separated
// path rel and of every file below it, keyed by path. An empty rel means
// every file under the root.
func (s *Store) FileStatesUnder(ctx context.Context, rootID int64, rel string) (map[string]FileState, error) {
	if rel == "" {
		return s.FileStates(ctx, rootID)
	}
	// Paths below rel sort between "rel/" and "rel0" ('0' follows '/'),
	// so the (root_id, path) index answers this as a range scan.
	var rows []FileState
	err := sqlx.SelectContext(ctx, s.q, &rows, `
		SELECT id, path, size, mtime, hash, symbols_version FROM files
		WHERE root_id = ? AND (path = ? OR (path >= ? AND path < ?))`,
		rootID, rel, rel+"/", rel+"0")
	if err != nil {
		return nil, err
	}
	states := make(map[string]FileState, len(rows))
	for _, r := range rows {
		states[r.Path] = r
	}
	return states, nil
}
