package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
}

// FileState is what the indexer needs to decide whether a file changed.
type FileState struct {
	ID      int64  `db:"id"`
	Path    string `db:"path"`
	Size    int64  `db:"size"`
	ModTime int64  `db:"mtime"`
	Hash    string `db:"hash"`
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

// UpsertFile inserts or replaces the file at (RootID, Path) and returns its id.
func (s *Store) UpsertFile(ctx context.Context, f File) (int64, error) {
	var id int64
	err := sqlx.GetContext(ctx, s.q, &id, `
		INSERT INTO files (root_id, path, ext, size, mtime, hash, content)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (root_id, path) DO UPDATE SET
			ext = excluded.ext, size = excluded.size, mtime = excluded.mtime,
			hash = excluded.hash, content = excluded.content
		RETURNING id`,
		f.RootID, f.Path, f.Ext, f.Size, f.ModTime, f.Hash, f.Content)
	if err != nil {
		return 0, fmt.Errorf("upsert file %s: %w", f.Path, err)
	}
	return id, nil
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
	err := sqlx.SelectContext(ctx, s.q, &rows, `SELECT id, path, size, mtime, hash FROM files WHERE root_id = ?`, rootID)
	if err != nil {
		return nil, err
	}
	states := make(map[string]FileState, len(rows))
	for _, r := range rows {
		states[r.Path] = r
	}
	return states, nil
}
