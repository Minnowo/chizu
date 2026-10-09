package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/vinovest/sqlx"
)

// migrations are applied in order; PRAGMA user_version records how many ran.
// {{TOKENIZE}} is replaced with the FTS5 tokenize argument.
var migrations = []string{
	`
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE roots (
	id         INTEGER PRIMARY KEY,
	path       TEXT NOT NULL UNIQUE,
	indexed_at INTEGER NOT NULL DEFAULT 0,
	options    TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE files (
	id      INTEGER PRIMARY KEY,
	root_id INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
	path    TEXT NOT NULL,
	ext     TEXT NOT NULL DEFAULT '',
	size    INTEGER NOT NULL DEFAULT 0,
	mtime   INTEGER NOT NULL DEFAULT 0,
	hash    TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	UNIQUE (root_id, path)
);

CREATE VIRTUAL TABLE files_fts USING fts5(
	path,
	content,
	content = 'files',
	content_rowid = 'id',
	tokenize = '{{TOKENIZE}}'
);

CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN
	INSERT INTO files_fts (rowid, path, content) VALUES (new.id, new.path, new.content);
END;

CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN
	INSERT INTO files_fts (files_fts, rowid, path, content) VALUES ('delete', old.id, old.path, old.content);
END;

CREATE TRIGGER files_au AFTER UPDATE OF path, content ON files BEGIN
	INSERT INTO files_fts (files_fts, rowid, path, content) VALUES ('delete', old.id, old.path, old.content);
	INSERT INTO files_fts (rowid, path, content) VALUES (new.id, new.path, new.content);
END;
`,
	// FTS5 flushes pending index data whenever it passes hashsize (1MB by
	// default); a larger buffer makes bulk indexing ~15% faster.
	`INSERT INTO files_fts (files_fts, rank) VALUES ('hashsize', 8388608);`,
}

// migrate brings the schema up to date and returns the database's tokenizer.
func migrate(ctx context.Context, db *sqlx.DB, tokenizer string) (string, error) {

	var version int
	if err := db.GetContext(ctx, &version, "PRAGMA user_version"); err != nil {
		return "", fmt.Errorf("read schema version: %w", err)
	}

	if version > len(migrations) {
		return "", fmt.Errorf("database schema version %d is newer than this chizu (%d)", version, len(migrations))
	}

	if version == 0 && tokenizer == "" {
		tokenizer = TokenizerUnicode
	}

	for i := version; i < len(migrations); i++ {

		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			return "", err
		}

		stmt := strings.ReplaceAll(migrations[i], "{{TOKENIZE}}", tokenizerSpecs[tokenizer])

		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			tx.Rollback()
			return "", fmt.Errorf("migration %d: %w", i+1, err)
		}

		if i == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('tokenizer', ?)`, tokenizer); err != nil {
				tx.Rollback()
				return "", fmt.Errorf("migration %d: %w", i+1, err)
			}
		}

		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return "", fmt.Errorf("migration %d: %w", i+1, err)
		}

		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("migration %d: %w", i+1, err)
		}
	}

	var stored string
	err := db.GetContext(ctx, &stored, `SELECT value FROM meta WHERE key = 'tokenizer'`)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("database is missing its tokenizer metadata")
	}
	if err != nil {
		return "", fmt.Errorf("read tokenizer: %w", err)
	}

	if tokenizer != "" && tokenizer != stored {
		return "", fmt.Errorf("%w: database uses %q, requested %q", ErrTokenizerMismatch, stored, tokenizer)
	}

	return stored, nil
}
