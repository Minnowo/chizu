package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vinovest/sqlx"
)

const driverName = "chizu_sqlite"

// connectionPragmas are run on every new connection in the pool.
const connectionPragmas = "PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"

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

	return &DB{DB: conn, Store: Store{q: conn}, tokenizer: tokenizer}, nil
}

// Tokenizer returns the FTS5 tokenizer this database was created with.
func (d *DB) Tokenizer() string {
	return d.tokenizer
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
