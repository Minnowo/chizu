# chizu

Simple full text search for directories of files, backed by SQLite FTS5.
One static binary with a CLI, an HTTP API + web UI, and an MCP server.

```sh
chizu index ~/notes          # index a folder (incremental on re-runs)
chizu search heron           # search it
chizu serve                  # http://127.0.0.1:7070 (UI, /api/*, /mcp)
chizu mcp                    # MCP over stdio
```

## Database

The index lives in `.chizu.sqlite`. Without `--db` (or `CHIZU_DB`), chizu
uses the nearest `.chizu.sqlite` in the current directory or a parent, or
creates one in the current directory. One database can hold many folders:

```sh
chizu index --db main.sqlite -i ~/notes -i ~/docs ~/wiki
```

The tokenizer is chosen when a database is created:
`--tokenizer unicode61` (default; words, stemming) or `--tokenizer trigram`
(substring matching, larger index).

## Commands

| command | does |
|---|---|
| `init [dir]` | write a `.chizuignore` with the default rules |
| `index [dir...]` | index dirs; with no args, re-index every indexed dir |
| `search <query...>` | search (`--raw`, `--dir`, `--path`, `--ext`, `--limit`, `--offset`, `--lines`, `--json`) |
| `roots` | list indexed dirs |
| `remove <dir...>` | drop dirs from the index |
| `cat <path\|id>` | print an indexed file |
| `serve` | HTTP API, web UI and MCP (`--addr`, `--read-only`) |
| `mcp` | MCP over stdio (`--read-only`) |

`search` exits 1 when nothing matched and 2 on error, like grep.

## What gets indexed

Every file that looks like UTF-8 text (no NUL bytes in the first 8KB) and is
under `--max-size` (default 10MiB). Narrow it with `--ext md,txt`,
`--include '*.md'` and `--exclude 'drafts/'`. These filters are saved per
folder and reused by later re-indexes; `--clear-filters` forgets them.

Indexing reads and hashes files on all CPUs (`--workers` / `-j` to change)
and writes to SQLite from one goroutine in batches.

`.chizuignore` files use gitignore syntax and may appear in any directory.
If the indexed folder has no `.chizuignore`, built-in defaults apply: version
control dirs (`.git/` ...), dependency dirs (`node_modules/`, `vendor/`,
`.venv/` ...), build output (`build/`, `target/`, `out/`, `dist/`,
`.gradle/`, `CMakeFiles/`, `cmake-build-*/` ...), caches and chizu's own
database. `chizu init` writes the full list to a file; delete a line to
index those files.

## Queries

Each word must appear (stemmed: `running` finds `run`). Punctuation is safe.
`"quoted phrases"` stay together and `pre*` is a prefix search. With `--raw`
the query is FTS5 syntax: `AND`, `OR`, `NOT`, `NEAR(a b, 5)`, `path:term`.

## HTTP API

| endpoint | |
|---|---|
| `GET /api/search?q=&raw=&dir=&path=&ext=&limit=&offset=&lines=` | `{"results": [...]}` |
| `GET /api/roots` | `{"roots": [...]}` |
| `GET /api/file?path=` or `?id=` (`&raw=1` for text/plain) | file |
| `GET /api/info` | db, tokenizer, driver, version |
| `POST /api/index` `{"dirs": [], "exts": [], "include": [], "exclude": [], "max_size": 0, "clear_filters": false}` | stats |
| `DELETE /api/roots?path=` | remove |
| `/mcp` | MCP streamable HTTP |

## MCP

Tools: `search`, `list_roots`, `read_file`, plus `index` and `remove_root`
unless `--read-only`. For Claude Code:

```sh
claude mcp add chizu -- chizu mcp --db /path/to/.chizu.sqlite
```

## Building

```sh
make build       # static, pure Go (modernc.org/sqlite)    -> bin/chizu
make build-cgo   # static, cgo (mattn/go-sqlite3)          -> bin/chizu-cgo
make test-all    # tests against both drivers
```

The cgo driver is only used with `-tags sqlite_fts5` (which mattn needs for
FTS5); any other build uses the pure Go driver.

## Layout

```
main.go          entry point
src/cmd          CLI (urfave/cli)
src/database     SQLite connection (cgo / pure Go), schema, queries
src/ignore       .chizuignore matching
src/indexer      directory walking, text sniffing, incremental updates
src/search       query building, filters, snippets, line numbers
src/api          HTTP API (chi) and embedded web UI
src/mcp          MCP server (mcp-go)
```
