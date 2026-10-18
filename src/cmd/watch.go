package cmd

import (
	"context"
	"fmt"
	"time"

	"chizu/src/database"
	"chizu/src/indexer"
	"chizu/src/watch"

	"github.com/rs/zerolog"
	"github.com/urfave/cli/v3"
)

func debounceFlag() cli.Flag {
	return &cli.DurationFlag{Name: "debounce", Value: watch.DefaultDebounce, Usage: "wait this long for changes to stop before re-indexing"}
}

func watchCommand() *cli.Command {
	return &cli.Command{
		Name:      "watch",
		Usage:     "index directories, then keep re-indexing them as files change (with no arguments, every indexed directory)",
		ArgsUsage: "[dir...]",
		Description: `Indexes each directory once, like chizu index, then waits for changes
and re-indexes the directory they happened in. Re-indexing is incremental,
so only changed files are read. Runs until interrupted.`,
		Flags:  append(indexFlags(), debounceFlag()),
		Action: runWatch,
	}
}

func runWatch(ctx context.Context, cmd *cli.Command) error {

	w, errW := cmd.Root().Writer, cmd.Root().ErrWriter

	status := newStatusLine(errW)
	log := loggerTo(cmd, status.writer(errW))

	db, dbFile, err := openDB(ctx, cmd, false, cmd.String("tokenizer"))
	if err != nil {
		return err
	}
	defer db.Close()

	dirs := append(cmd.StringSlice("input"), cmd.Args().Slice()...)
	if len(dirs) == 0 {
		roots, err := db.ListRoots(ctx)
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			return fmt.Errorf("nothing is indexed in %s yet (run: chizu watch <dir>)", dbFile)
		}
	}

	opts := indexOptions(cmd, dbFile, log)
	if isTerminal(errW) && !cmd.Bool("quiet") {
		opts.Progress = status.update
	}

	return watch.Run(ctx, db, dirs, watch.Options{
		Index:    opts,
		Debounce: cmd.Duration("debounce"),
		Log:      log,
		Indexed: func(stats indexer.Stats, took time.Duration, initial bool, err error) {
			status.clear()
			switch {
			case err != nil:
				fmt.Fprintf(errW, "failed to index %s: %v\n", stats.Root, err)
			case initial || stats.Added+stats.Updated+stats.Removed > 0:
				printStats(w, stats, took)
			}
			if initial {
				fmt.Fprintf(w, "watching %s\n", stats.Root)
			}
		},
	})
}

func watchFlag() cli.Flag {
	return &cli.BoolFlag{Name: "watch", Aliases: []string{"w"}, Usage: "re-index indexed directories as their files change"}
}

// startWatch watches every indexed root in the background until ctx is
// done, when --watch is set and the database is writable, and reports
// whether it did. Results are logged.
func startWatch(ctx context.Context, cmd *cli.Command, db *database.DB, dbFile string, log zerolog.Logger) bool {

	if !cmd.Bool("watch") {
		return false
	}
	if cmd.Bool("read-only") {
		log.Warn().Msg("--watch is ignored with --read-only")
		return false
	}

	go func() {
		err := watch.Run(ctx, db, nil, watch.Options{
			Index:    indexer.Options{SkipPaths: dbFiles(dbFile), Log: log},
			Debounce: cmd.Duration("debounce"),
			Log:      log,
			Indexed: func(stats indexer.Stats, took time.Duration, initial bool, err error) {
				if err == nil && (initial || stats.Added+stats.Updated+stats.Removed > 0) {
					log.Info().Str("root", stats.Root).Dur("took", took).
						Int("added", stats.Added).Int("updated", stats.Updated).Int("removed", stats.Removed).
						Msg("indexed")
				}
			},
		})
		if err != nil {
			log.Error().Err(err).Msg("watch stopped")
		}
	}()
	return true
}
