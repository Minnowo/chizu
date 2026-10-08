package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"chizu/src/ignore"
	"chizu/src/indexer"

	"github.com/urfave/cli/v3"
)

func initCommand() *cli.Command {
	return &cli.Command{
		Name:      "init",
		Usage:     "write a " + ignore.FileName + " with the default ignore rules",
		ArgsUsage: "[dir]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "force", Aliases: []string{"f"}, Usage: "overwrite an existing file"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			dir := "."
			if cmd.Args().Len() > 0 {
				dir = cmd.Args().First()
			}
			path, err := ignore.WriteDefault(dir, cmd.Bool("force"))
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.Root().Writer, "wrote %s\n", path)
			return nil
		},
	}
}

func indexCommand() *cli.Command {
	return &cli.Command{
		Name:      "index",
		Usage:     "index directories (with no arguments, re-index everything already indexed)",
		ArgsUsage: "[dir...]",
		Flags: []cli.Flag{
			dbFlag(),
			&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "`DIR` to index (repeatable)"},
			&cli.StringFlag{Name: "tokenizer", Usage: "tokenizer for a new database: unicode61 (words, stemming) or trigram (substrings)"},
			&cli.StringSliceFlag{Name: "ext", Aliases: []string{"e"}, Usage: "only index these extensions, e.g. --ext md,txt"},
			&cli.StringSliceFlag{Name: "include", Usage: "only index files matching these gitignore-style `PATTERN`s"},
			&cli.StringSliceFlag{Name: "exclude", Aliases: []string{"x"}, Usage: "skip paths matching these gitignore-style `PATTERN`s"},
			&cli.Int64Flag{Name: "max-size", Usage: "skip files larger than `BYTES` (default 10MiB)"},
			&cli.BoolFlag{Name: "clear-filters", Usage: "forget the filters saved from earlier runs"},
			&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}, Usage: "no progress output"},
			jsonFlag(),
		},
		Action: runIndex,
	}
}

func runIndex(ctx context.Context, cmd *cli.Command) error {

	w, errW := cmd.Root().Writer, cmd.Root().ErrWriter

	// Show a status line while indexing when stderr is a terminal. Logs go
	// through it so a warning clears the line instead of being glued onto it.
	status := newStatusLine(errW)
	log := loggerTo(cmd, status.writer(errW))

	db, dbFile, err := openDB(ctx, cmd, false, cmd.String("tokenizer"))
	if err != nil {
		return err
	}
	defer db.Close()

	opts := indexer.Options{
		Exts:         cmd.StringSlice("ext"),
		Include:      cmd.StringSlice("include"),
		Exclude:      cmd.StringSlice("exclude"),
		MaxSize:      cmd.Int64("max-size"),
		ClearFilters: cmd.Bool("clear-filters"),
		SkipPaths:    []string{dbFile, dbFile + "-wal", dbFile + "-shm", dbFile + "-journal"},
		Log:          log,
	}
	if isTerminal(errW) && !cmd.Bool("quiet") {
		opts.Progress = status.update
	}

	dirs := append(cmd.StringSlice("input"), cmd.Args().Slice()...)

	// No dirs: re-index every root already in the database.
	if len(dirs) == 0 {
		roots, err := db.ListRoots(ctx)
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			return fmt.Errorf("nothing is indexed in %s yet (run: chizu index <dir>)", dbFile)
		}
		for _, r := range roots {
			dirs = append(dirs, r.Path)
		}
	}

	all := []indexer.Stats{}
	failed := 0

	// Report each directory as it finishes, so it is always clear whether
	// indexing completed or failed.
	for _, dir := range dirs {
		start := time.Now()
		stats, err := indexer.Index(ctx, db, dir, opts)
		status.clear()

		if err != nil {
			failed++
			fmt.Fprintf(errW, "failed to index %s: %v\n", dir, err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}

		all = append(all, stats)
		if !cmd.Bool("json") {
			fmt.Fprintf(w, "indexed %s in %s: %d added, %d updated, %d unchanged, %d removed, %d skipped\n",
				stats.Root, time.Since(start).Round(time.Millisecond), stats.Added, stats.Updated, stats.Unchanged, stats.Removed, stats.Skipped)
		}
	}

	if cmd.Bool("json") {
		if err := writeJSON(w, all); err != nil {
			return err
		}
	}

	if failed > 0 {
		noun := "directories"
		if len(dirs) == 1 {
			noun = "directory"
		}
		return fmt.Errorf("%d of %d %s failed", failed, len(dirs), noun)
	}

	return nil
}
