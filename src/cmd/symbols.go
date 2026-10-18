package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"chizu/src/search"

	"github.com/urfave/cli/v3"
)

const colorKind = "\x1b[36m"

func defCommand() *cli.Command {
	return &cli.Command{
		Name:      "def",
		Usage:     "find where a function, type, method, ... is defined",
		ArgsUsage: "<name>",
		Description: `Looks up definitions found by parsing source files with tree-sitter.
Names match ignoring case and can be qualified by their parent:
Store.Open, Store::open. If no name matches exactly, names containing
it are shown.`,
		Flags: []cli.Flag{
			dbFlag(),
			&cli.BoolFlag{Name: "body", Aliases: []string{"b"}, Usage: "print each definition's source"},
			&cli.StringSliceFlag{Name: "kind", Aliases: []string{"k"}, Usage: "only these kinds, e.g. --kind function,method"},
			&cli.StringFlag{Name: "dir", Usage: "only search inside `DIR`"},
			&cli.StringFlag{Name: "path", Aliases: []string{"p"}, Usage: "only files whose path (relative to its indexed root) starts with `PREFIX`"},
			&cli.StringSliceFlag{Name: "ext", Aliases: []string{"e"}, Usage: "only these extensions, e.g. --ext go,py"},
			&cli.IntFlag{Name: "limit", Aliases: []string{"n"}, Value: search.DefaultDefLimit, Usage: "max definitions"},
			&cli.IntFlag{Name: "offset", Usage: "skip this many definitions"},
			maxTokensFlag(),
			colorFlag(),
			jsonFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {

			if cmd.Args().Len() != 1 {
				return fmt.Errorf("usage: chizu def <name>")
			}

			db, _, err := openDB(ctx, cmd, true, "")
			if err != nil {
				return err
			}
			defer db.Close()

			defs, err := search.Definitions(ctx, db, search.DefQuery{
				Name:       cmd.Args().First(),
				Kinds:      cmd.StringSlice("kind"),
				Dir:        cmd.String("dir"),
				PathPrefix: cmd.String("path"),
				Exts:       cmd.StringSlice("ext"),
				Limit:      cmd.Int("limit"),
				Offset:     cmd.Int("offset"),
				Body:       cmd.Bool("body"),
			})
			if err != nil {
				return err
			}

			found := len(defs)
			defs, budget := search.FitDefinitions(defs, cmd.Int("max-tokens"))

			w := cmd.Root().Writer
			if cmd.Bool("json") {
				if err := writeJSON(w, defs); err != nil {
					return err
				}
			} else {
				printDefinitions(w, defs, useColor(cmd, w))
			}
			printNote(cmd, budget)

			if found == 0 {
				return ErrNoResults
			}
			return nil
		},
	}
}

func printDefinitions(w io.Writer, defs []search.Definition, color bool) {

	wd, _ := os.Getwd()
	paint := painter(color)

	if len(defs) > 0 && !defs[0].Exact {
		fmt.Fprintln(w, "no exact match; names containing it:")
	}

	for i, d := range defs {
		if i > 0 && len(d.Body) > 0 {
			fmt.Fprintln(w)
		}

		loc := fmt.Sprintf("%s:%d", displayPath(wd, d.Root, d.AbsPath), d.Line)
		fmt.Fprintf(w, "%s %s %s %s\n", paint(colorPath, loc), paint(colorID, fmt.Sprintf("(id %d)", d.FileID)),
			paint(colorKind, d.Kind), d.QualifiedName())

		if len(d.Body) == 0 {
			fmt.Fprintf(w, "  %s\n", d.Signature)
			continue
		}
		for j, l := range d.Body {
			fmt.Fprintf(w, "%s: %s\n", paint(colorLine, strconv.Itoa(d.Line+j)), l)
		}
		if last := d.Line + len(d.Body) - 1; last < d.EndLine {
			fmt.Fprintf(w, "… %d more lines\n", d.EndLine-last)
		}
	}
}

func outlineCommand() *cli.Command {
	return &cli.Command{
		Name:      "outline",
		Usage:     "list the definitions in an indexed source file, by path or file id",
		ArgsUsage: "<path|id>",
		Flags:     []cli.Flag{dbFlag(), colorFlag(), jsonFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {

			if cmd.Args().Len() != 1 {
				return fmt.Errorf("usage: chizu outline <path|id>")
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

			outline, err := search.Outline(ctx, db, f.ID)
			if err != nil {
				return err
			}

			w := cmd.Root().Writer
			if cmd.Bool("json") {
				if err := writeJSON(w, outline); err != nil {
					return err
				}
			} else {
				paint := painter(useColor(cmd, w))
				wd, _ := os.Getwd()
				abs := filepath.Join(f.RootPath, filepath.FromSlash(f.Path))
				fmt.Fprintf(w, "%s %s\n", paint(colorPath, displayPath(wd, f.RootPath, abs)),
					paint(colorID, fmt.Sprintf("(id %d, %d lines)", f.ID, strings.Count(f.Content, "\n")+1)))
				for _, e := range outline {
					lines := fmt.Sprintf("%d-%d", e.Line, e.EndLine)
					fmt.Fprintf(w, "%s%s %s %s\n", strings.Repeat("  ", e.Depth+1), paint(colorLine, lines), paint(colorKind, e.Kind), e.Name)
				}
			}

			if len(outline) == 0 {
				return ErrNoResults
			}
			return nil
		},
	}
}
