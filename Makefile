VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X chizu/src/version.Version=$(VERSION)
BIN     := bin/chizu

.PHONY: build build-cgo test test-cgo test-all fmt vet clean

# Static pure-Go binary (modernc.org/sqlite).
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) .

# Static cgo binary (mattn/go-sqlite3). sqlite_fts5 selects the mattn driver
# and compiles in FTS5; the rest avoid glibc dlopen/NSS for a static link.
build-cgo:
	CGO_ENABLED=1 go build -trimpath -tags 'sqlite_fts5 sqlite_omit_load_extension netgo osusergo' \
		-ldflags '$(LDFLAGS) -linkmode external -extldflags "-static"' -o $(BIN)-cgo .

test:
	CGO_ENABLED=0 go test ./...

test-cgo:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

test-all: test test-cgo

fmt:
	gofmt -w main.go src

vet:
	go vet ./...

clean:
	rm -rf bin
