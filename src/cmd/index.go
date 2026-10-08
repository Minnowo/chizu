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
			jsonFlag(),
		},
		Action: runIndex,
	}
}

func runIndex(ctx context.Context, cmd *cli.Command) error {

	log := logger(cmd)

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

	dirs := append(cmd.StringSlice("input"), cmd.Args().Slice()...)

	var all []indexer.Stats
	var errs []error
	start := time.Now()

	if len(dirs) == 0 {
		roots, err := db.ListRoots(ctx)
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			return fmt.Errorf("nothing is indexed in %s yet (run: chizu index <dir>)", dbFile)
		}
		all, err = indexer.IndexAll(ctx, db, opts)
		if err != nil {
			errs = append(errs, err)
		}
	} else {
		for _, dir := range dirs {
			stats, err := indexer.Index(ctx, db, dir, opts)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", dir, err))
				continue
			}
			all = append(all, stats)
		}
	}

	w := cmd.Root().Writer
	if cmd.Bool("json") {
		if all == nil {
			all = []indexer.Stats{}
		}
		if err := writeJSON(w, all); err != nil {
			return err
		}
	} else {
		for _, s := range all {
			fmt.Fprintf(w, "%s: %d added, %d updated, %d unchanged, %d removed, %d skipped\n",
				s.Root, s.Added, s.Updated, s.Unchanged, s.Removed, s.Skipped)
		}
	}

	log.Info().Dur("took", time.Since(start)).Str("db", dbFile).Msg("indexing done")

	return errors.Join(errs...)
}
