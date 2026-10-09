package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/vinovest/sqlx"
)

const driverName = "chizu_sqlite"

// journalSizeLimit is how large the WAL file may stay after a checkpoint.
// Without it a big index leaves a WAL as large as its changes for as long
// as any other process (chizu serve, chizu mcp) has the database open.
const journalSizeLimit = 64 << 20

// connectionPragmas are run on every new connection in the pool.
var connectionPragmas = fmt.Sprintf("PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000; PRAGMA journal_size_limit = %d;", journalSizeLimit)

const (
	TokenizerUnicode = "unicode61"
	TokenizerTrigram = "trigram"
)

// tokenizerSpecs maps a tokenizer name to its FTS5 tokenize= argument.
var tokenizerSpecs = map[string]string{
	TokenizerUnicode: "porter unicode61 remove_diacritics 2",
	TokenizerTrigram: "trigram",
}

func init() {
	sqlx.BindDriver(driverName, sqlx.QUESTION)
}

// Options control how a database is opened.
type Options struct {
	// Tokenizer is only used when the database is created. If set on an
	// existing database it must match the stored tokenizer.
	Tokenizer string
}

// DB is a chizu index database.
type DB struct {
	*sqlx.DB
	Store
	path      string
	tokenizer string
}

// Open opens (creating if needed) the index database at path.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {

	if opts.Tokenizer != "" {
		if _, ok := tokenizerSpecs[opts.Tokenizer]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTokenizer, opts.Tokenizer)
		}
	}

	conn, err := sqlx.ConnectContext(ctx, driverName, path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	conn.SetMaxOpenConns(5)
	conn.SetConnMaxLifetime(10 * time.Minute)

	if _, err := conn.ExecContext(ctx, "PRAGMA journal_mode = WAL;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	tokenizer, err := migrate(ctx, conn, opts.Tokenizer)
	if err != nil {
		conn.Close()
		return nil, err
	}

	return &DB{DB: conn, Store: Store{q: conn}, path: path, tokenizer: tokenizer}, nil
}

// Tokenizer returns the FTS5 tokenizer this database was created with.
func (d *DB) Tokenizer() string {
	return d.tokenizer
}

// Path returns the database file's path.
func (d *DB) Path() string {
	return d.path
}

// Size returns the bytes used on disk by the database and its WAL.
func (d *DB) Size() (int64, error) {
	var total int64
	for _, p := range []string{d.path, d.path + "-wal"} {
		st, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += st.Size()
	}
	return total, nil
}

// Checkpoint copies the WAL into the database and truncates the WAL file.
// SQLite does this itself when the last connection closes, but not while
// another process has the database open.
func (d *DB) Checkpoint(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Vacuum merges the full text index and rebuilds the database file so space
// freed by deleted files is returned to the filesystem.
func (d *DB) Vacuum(ctx context.Context) error {
	if _, err := d.ExecContext(ctx, `INSERT INTO files_fts (files_fts) VALUES ('optimize')`); err != nil {
		return fmt.Errorf("optimize full text index: %w", err)
	}
	if _, err := d.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return d.Checkpoint(ctx)
}

// WithTx runs fn inside a transaction, committing if fn returns nil.
func (d *DB) WithTx(ctx context.Context, fn func(*Store) error) error {

	tx, err := d.BeginTxx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}

	if err := fn(&Store{q: tx}); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}
