//go:build !cgo || !sqlite_fts5

package database

import (
	"context"
	"database/sql"

	"modernc.org/sqlite"
)

// Driver names the SQLite driver compiled into this binary.
const Driver = "modernc.org/sqlite (pure go)"

func init() {
	d := &sqlite.Driver{}
	d.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), connectionPragmas, nil)
		return err
	})
	sql.Register(driverName, d)
}
