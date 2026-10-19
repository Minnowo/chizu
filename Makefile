VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X chizu/src/version.Version=$(VERSION)
BIN     := bin/chizu

# tree-sitter's parsers are C, so every build uses cgo. On Linux the binary
# is linked statically; netgo and osusergo avoid glibc's dlopen/NSS so that
# works. Set STATIC= to link dynamically (macOS cannot link statically).
STATIC  ?= $(if $(filter Linux,$(shell uname -s)),-linkmode external -extldflags "-static")
TAGS    := netgo osusergo

.PHONY: build build-cgo test test-cgo test-all fmt vet clean

# Pure Go SQLite driver (modernc.org/sqlite).
build:
	CGO_ENABLED=1 go build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS) $(STATIC)' -o $(BIN) .

build-windows:
	CC="zig cc -target x86_64-windows-gnu" \
	CXX="zig c++ -target x86_64-windows-gnu" \
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS) $(STATIC)' -o $(BIN).exe .

# cgo SQLite driver (mattn/go-sqlite3). sqlite_fts5 selects it and compiles in FTS5.
build-cgo:
	CGO_ENABLED=1 go build -trimpath -tags '$(TAGS) sqlite_fts5 sqlite_omit_load_extension' \
		-ldflags '$(LDFLAGS) $(STATIC)' -o $(BIN)-cgo .

test:
	CGO_ENABLED=1 go test ./...

test-cgo:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

test-all: test test-cgo

fmt:
	gofmt -w main.go src

vet:
	go vet ./...

clean:
	rm -rf bin
