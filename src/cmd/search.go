package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chizu/src/database"
	"chizu/src/indexer"
	"chizu/src/search"

	"github.com/urfave/cli/v3"
)

const (
	colorPath  = "\x1b[35m"
	colorLine  = "\x1b[32m"
	colorMatch = "\x1b[1;31m"
	colorReset = "\x1b[0m"
)

func searchCommand() *cli.Command {
	return &cli.Command{
		Name:      "search",
		Aliases:   []string{"s"},
		Usage:     "search the index",
		ArgsUsage: "<query...>",
		Description: `Each word is searched literally and all must appear (stemmed with the
default tokenizer, so "running" also finds "run"). Use "double quotes"
for phrases and a trailing * for prefixes. --raw passes the query to
SQLite FTS5 unchanged, allowing AND, OR, NOT, NEAR() and column filters.`,
		Flags: []cli.Flag{
			dbFlag(),
			&cli.BoolFlag{Name: "raw", Aliases: []string{"r"}, Usage: "use FTS5 query syntax"},
			&cli.StringFlag{Name: "dir", Usage: "only search inside `DIR`"},
			&cli.StringFlag{Name: "path", Aliases: []string{"p"}, Usage: "only files whose path (relative to its indexed root) starts with `PREFIX`"},
			&cli.StringSliceFlag{Name: "ext", Aliases: []string{"e"}, Usage: "only these extensions, e.g. --ext md,txt"},
			&cli.IntFlag{Name: "limit", Aliases: []string{"n"}, Value: search.DefaultLimit, Usage: "max files"},
			&cli.IntFlag{Name: "offset", Usage: "skip this many files"},
			&cli.IntFlag{Name: "lines", Value: search.DefaultMaxLines, Usage: "max matching lines shown per file"},
			&cli.StringFlag{Name: "color", Value: "auto", Usage: "auto, always or never"},
			jsonFlag(),
		},
		Action: runSearch,
	}
}

func runSearch(ctx context.Context, cmd *cli.Command) error {

	if cmd.Args().Len() == 0 {
		return fmt.Errorf("missing query (usage: chizu search <query...>)")
	}

	db, _, err := openDB(ctx, cmd, true, "")
	if err != nil {
		return err
	}
	defer db.Close()

	results, err := search.Search(ctx, db, search.Query{
		Text:       strings.Join(cmd.Args().Slice(), " "),
		Raw:        cmd.Bool("raw"),
		Dir:        cmd.String("dir"),
		PathPrefix: cmd.String("path"),
		Exts:       cmd.StringSlice("ext"),
		Limit:      cmd.Int("limit"),
		Offset:     cmd.Int("offset"),
		MaxLines:   cmd.Int("lines"),
	})
	if err != nil {
		return err
	}

	w := cmd.Root().Writer

	if cmd.Bool("json") {
		if err := writeJSON(w, results); err != nil {
			return err
		}
	} else {
		color := false
		switch cmd.String("color") {
		case "always":
			color = true
		case "auto":
			color = isTerminal(w) && os.Getenv("NO_COLOR") == ""
		}
		printResults(w, results, color)
	}

	if len(results) == 0 {
		return ErrNoResults
	}
	return nil
}

func printResults(w io.Writer, results []search.Result, color bool) {

	wd, _ := os.Getwd()

	paint := func(c, s string) string {
		if !color {
			return s
		}
		return c + s + colorReset
	}

	for i, r := range results {
		if i > 0 {
			fmt.Fprintln(w)
		}

		fmt.Fprintln(w, paint(colorPath, displayPath(wd, r.Root, r.AbsPath)))

		for _, l := range r.Lines {
			var sb strings.Builder
			for _, s := range l.Segments {
				if s.Match {
					sb.WriteString(paint(colorMatch, s.Text))
				} else {
					sb.WriteString(s.Text)
				}
			}
			fmt.Fprintf(w, "%s: %s\n", paint(colorLine, strconv.Itoa(l.Number)), sb.String())
		}

		if more := r.LineCount - len(r.Lines); more > 0 {
			fmt.Fprintf(w, "… %d more matching lines\n", more)
		}
	}
}

// displayPath shows a result relative to the working directory when the
// working directory is inside the result's root, and absolute otherwise.
func displayPath(wd, root, abs string) string {
	if wd != "" && (wd == root || strings.HasPrefix(wd, root+string(filepath.Separator))) {
		if rel, err := filepath.Rel(wd, abs); err == nil {
			return rel
		}
	}
	return abs
}

func rootsCommand() *cli.Command {
	return &cli.Command{
		Name:  "roots",
		Usage: "list indexed directories",
		Flags: []cli.Flag{dbFlag(), jsonFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			db, _, err := openDB(ctx, cmd, true, "")
			if err != nil {
				return err
			}
			defer db.Close()

			roots, err := db.ListRoots(ctx)
			if err != nil {
				return err
			}

			w := cmd.Root().Writer
			if cmd.Bool("json") {
				return writeJSON(w, roots)
			}
			for _, r := range roots {
				fmt.Fprintf(w, "%s\t%d files\tindexed %s\n", r.Path, r.FileCount, time.Unix(r.IndexedAt, 0).Format(time.DateTime))
			}
			return nil
		},
	}
}

func filesCommand() *cli.Command {
	return &cli.Command{
		Name:      "files",
		Aliases:   []string{"ls"},
		Usage:     "list indexed files, optionally only those whose path contains TEXT",
		ArgsUsage: "[text]",
		Flags: []cli.Flag{
			dbFlag(),
			&cli.StringFlag{Name: "dir", Usage: "only files inside `DIR`"},
			&cli.StringFlag{Name: "path", Aliases: []string{"p"}, Usage: "only files whose path (relative to its indexed root) starts with `PREFIX`"},
			&cli.StringSliceFlag{Name: "ext", Aliases: []string{"e"}, Usage: "only these extensions, e.g. --ext md,txt"},
			&cli.IntFlag{Name: "limit", Aliases: []string{"n"}, Usage: "max files (default all)"},
			&cli.IntFlag{Name: "offset", Usage: "skip this many files"},
			jsonFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {

			db, _, err := openDB(ctx, cmd, true, "")
			if err != nil {
				return err
			}
			defer db.Close()

			list, err := search.ListFiles(ctx, db, search.FileQuery{
				Contains:   strings.Join(cmd.Args().Slice(), " "),
				Dir:        cmd.String("dir"),
				PathPrefix: cmd.String("path"),
				Exts:       cmd.StringSlice("ext"),
				Limit:      cmd.Int("limit"),
				Offset:     cmd.Int("offset"),
			})
			if err != nil {
				return err
			}

			w := cmd.Root().Writer
			if cmd.Bool("json") {
				if err := writeJSON(w, list); err != nil {
					return err
				}
			} else {
				wd, _ := os.Getwd()
				bw := bufio.NewWriter(w)
				for _, f := range list.Files {
					fmt.Fprintln(bw, displayPath(wd, f.Root, f.AbsPath))
				}
				if err := bw.Flush(); err != nil {
					return err
				}
			}

			if list.Total == 0 {
				return ErrNoResults
			}
			return nil
		},
	}
}

func removeCommand() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Aliases:   []string{"rm"},
		Usage:     "remove directories from the index",
		ArgsUsage: "<dir...>",
		Flags:     []cli.Flag{dbFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				return fmt.Errorf("missing directory (usage: chizu remove <dir...>)")
			}

			db, _, err := openDB(ctx, cmd, true, "")
			if err != nil {
				return err
			}
			defer db.Close()

			for _, dir := range cmd.Args().Slice() {
				if err := indexer.Remove(ctx, db, dir); err != nil {
					return err
				}
				fmt.Fprintf(cmd.Root().Writer, "removed %s\n", dir)
			}
			return nil
		},
	}
}

func catCommand() *cli.Command {
	return &cli.Command{
		Name:      "cat",
		Usage:     "print an indexed file's content, by path or file id",
		ArgsUsage: "<path|id>",
		Flags:     []cli.Flag{dbFlag(), jsonFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 1 {
				return fmt.Errorf("usage: chizu cat <path|id>")
			}

			db, _, err := openDB(ctx, cmd, true, "")
			if err != nil {
				return err
			}
			defer db.Close()

			f, err := lookupFile(ctx, db, cmd.Args().First())
			if err != nil {
				return err
			}

			if cmd.Bool("json") {
				return writeJSON(cmd.Root().Writer, f)
			}
			_, err = io.WriteString(cmd.Root().Writer, f.Content)
			return err
		},
	}
}

// lookupFile treats arg as a path, or as a file id if it is a number that is not an existing path.
func lookupFile(ctx context.Context, db *database.DB, arg string) (database.File, error) {
	if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
		if _, statErr := os.Stat(arg); errors.Is(statErr, os.ErrNotExist) {
			return search.GetFileByID(ctx, db, id)
		}
	}
	return search.GetFile(ctx, db, arg)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
