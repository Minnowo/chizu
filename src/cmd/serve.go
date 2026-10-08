package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"chizu/src/api"
	"chizu/src/database"
	"chizu/src/mcp"

	"github.com/rs/zerolog"
	"github.com/urfave/cli/v3"
)

func readOnlyFlag() cli.Flag {
	return &cli.BoolFlag{Name: "read-only", Usage: "disable indexing and removing directories"}
}

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve the HTTP API, web UI, and MCP (streamable HTTP at /mcp)",
		Flags: []cli.Flag{
			dbFlag(),
			&cli.StringFlag{Name: "addr", Aliases: []string{"a"}, Value: "127.0.0.1:7070", Usage: "listen `ADDRESS`", Sources: cli.EnvVars("CHIZU_ADDR")},
			readOnlyFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {

			log := logger(cmd)

			db, dbFile, err := openDB(ctx, cmd, false, "")
			if err != nil {
				return err
			}
			defer db.Close()

			ln, err := net.Listen("tcp", cmd.String("addr"))
			if err != nil {
				return err
			}

			srv := &http.Server{
				Handler:           serveHandler(db, dbFile, cmd.Bool("read-only"), log),
				ReadHeaderTimeout: 10 * time.Second,
			}

			fmt.Fprintf(cmd.Root().Writer, "chizu serving %s on http://%s (mcp: http://%s/mcp)\n", dbFile, ln.Addr(), ln.Addr())

			errc := make(chan error, 1)
			go func() { errc <- srv.Serve(ln) }()

			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := srv.Shutdown(shutdownCtx); err != nil {
					return err
				}
				if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			}
		},
	}
}

// serveHandler combines the API, web UI and MCP endpoint.
func serveHandler(db *database.DB, dbFile string, readOnly bool, log zerolog.Logger) http.Handler {

	r := api.New(api.Config{DB: db, DBPath: dbFile, ReadOnly: readOnly, Log: log})

	mcpServer := mcp.New(mcp.Config{DB: db, DBPath: dbFile, ReadOnly: readOnly, Log: log})
	r.Handle("/mcp", mcp.HTTPHandler(mcpServer))

	return r
}

func mcpCommand() *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "serve MCP over stdio (for Claude Code, Claude Desktop, and other MCP clients)",
		Flags: []cli.Flag{dbFlag(), readOnlyFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {

			log := logger(cmd)

			db, dbFile, err := openDB(ctx, cmd, false, "")
			if err != nil {
				return err
			}
			defer db.Close()

			s := mcp.New(mcp.Config{DB: db, DBPath: dbFile, ReadOnly: cmd.Bool("read-only"), Log: log})
			return mcp.ServeStdio(ctx, s, cmd.Root().Reader, cmd.Root().Writer)
		},
	}
}
