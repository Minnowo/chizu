// Package cmd defines chizu's command line interface.
package cmd

import (
	"chizu/src/version"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"chizu/src/database"

	"github.com/rs/zerolog"
	"github.com/urfave/cli/v3"
)

// DefaultDBName is the database file used when --db is not given.
const DefaultDBName = ".chizu.sqlite"

// ErrNoResults is returned by search when nothing matched, so the process can exit 1 like grep.
var ErrNoResults = errors.New("no results")

// Flags hold parse state, so each command gets fresh instances.

func dbFlag() cli.Flag {
	return &cli.StringFlag{
		Name:    "db",
		Aliases: []string{"d"},
		Usage:   "index database `FILE` (default: " + DefaultDBName + " in the current directory or a parent)",
		Sources: cli.EnvVars("CHIZU_DB"),
	}
}

func jsonFlag() cli.Flag {
	return &cli.BoolFlag{Name: "json", Usage: "print JSON"}
}

// New builds the chizu command. Output goes to w, logs and errors to errW.
func New(w, errW io.Writer) *cli.Command {

	return &cli.Command{
		Name:      "chizu",
		Usage:     "simple full text search for directories of files",
		Version:   version.Version + " (" + database.Driver + ")",
		Writer:    w,
		ErrWriter: errW,
		// Errors are returned to main, which decides the exit code.
		ExitErrHandler:  func(context.Context, *cli.Command, error) {},
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "log `LEVEL` (debug, info, warn, error)",
				Value:   "warn",
				Sources: cli.EnvVars("CHIZU_LOG_LEVEL"),
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "debug logging",
			},
		},
		Commands: []*cli.Command{
			initCommand(),
			indexCommand(),
			searchCommand(),
			rootsCommand(),
			filesCommand(),
			vacuumCommand(),
			removeCommand(),
			catCommand(),
			serveCommand(),
			mcpCommand(),
		},
	}
}

// logger returns the logger configured by the root command's flags.
func logger(cmd *cli.Command) zerolog.Logger {
	return loggerTo(cmd, cmd.Root().ErrWriter)
}

// loggerTo is logger writing to w.
func loggerTo(cmd *cli.Command, w io.Writer) zerolog.Logger {

	level, err := zerolog.ParseLevel(cmd.String("log-level"))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.WarnLevel
	}
	if cmd.Bool("verbose") {
		level = zerolog.DebugLevel
	}

	return zerolog.New(zerolog.ConsoleWriter{Out: w, NoColor: !isTerminal(cmd.Root().ErrWriter)}).
		Level(level).
		With().Timestamp().Logger()
}

// dbPath returns the database to use: --db / CHIZU_DB if set, otherwise
// the nearest .chizu.sqlite in the working directory or its parents,
// otherwise .chizu.sqlite in the working directory.
func dbPath(cmd *cli.Command) (string, error) {

	if p := cmd.String("db"); p != "" {
		return filepath.Abs(p)
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for dir := wd; ; {
		candidate := filepath.Join(dir, DefaultDBName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return filepath.Join(wd, DefaultDBName), nil
}

// openDB opens the database. Read-only commands set mustExist so a typo
// in --db does not silently create an empty index.
func openDB(ctx context.Context, cmd *cli.Command, mustExist bool, tokenizer string) (*database.DB, string, error) {

	path, err := dbPath(cmd)
	if err != nil {
		return nil, "", err
	}

	if mustExist {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil, path, fmt.Errorf("no index at %s (create one with: chizu index <dir>)", path)
		}
	}

	db, err := database.Open(ctx, path, database.Options{Tokenizer: tokenizer})
	return db, path, err
}

// isTerminal reports whether w is a terminal that should get colors.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
