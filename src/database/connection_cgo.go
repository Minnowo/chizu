//go:build cgo && sqlite_fts5

package database

import (
	"database/sql"

	"github.com/mattn/go-sqlite3"
)

// Driver names the SQLite driver compiled into this binary.
const Driver = "mattn/go-sqlite3 (cgo)"

func init() {
	sql.Register(driverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			_, err := conn.Exec(connectionPragmas, nil)
			return err
		},
	})
}
