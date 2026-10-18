// Package symbols finds the definitions (functions, types, methods, ...) in
// source files with tree-sitter.
//
// Each language has a query in queries/ that captures a definition node as
// @definition.<kind>, its name as @name and, where the definition is not
// nested in its owner (a Go method's receiver, a Rust impl's type), the
// owner as @parent. Definitions nested in other definitions get the
// innermost one as their parent.
package symbols

import (
	"bytes"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"chizu/src/database"

	tskotlin "github.com/fwcd/tree-sitter-kotlin/bindings/go"
	tslua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	tszig "github.com/tree-sitter-grammars/tree-sitter-zig/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
	tsbash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tscsharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tsc "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tscpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsphp "github.com/tree-sitter/tree-sitter-php/bindings/go"
	tspython "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsscala "github.com/tree-sitter/tree-sitter-scala/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Version is stored with each file's symbols. Bump it when extraction
// changes so the next index re-parses files whose content did not change.
const Version = 1

const (
	// timeout bounds one parse; a file that takes longer gets no symbols.
	timeout = 5 * time.Second
	// chunk is how much source the parser is handed at a time; each piece
	// is copied into C memory for the length of the parse.
	chunk = 64 << 10
	// maxSignature is how many bytes of a definition's first line are kept.
	maxSignature = 200
)

//go:embed queries/*.scm
var queries embed.FS

// language is a grammar, its query and the file extensions it parses.
type language struct {
	name  string
	exts  []string
	ptr   func() unsafe.Pointer
	query string

	once sync.Once
	lang *sitter.Language
	q    *sitter.Query
	err  error
}

var languages = []*language{
	{name: "bash", exts: []string{"sh", "bash"}, ptr: tsbash.Language, query: "bash"},
	{name: "c", exts: []string{"c"}, ptr: tsc.Language, query: "c"},
	{name: "c_sharp", exts: []string{"cs"}, ptr: tscsharp.Language, query: "c_sharp"},
	// .h goes to C++, whose grammar also reads C headers.
	{name: "cpp", exts: []string{"cc", "cpp", "cxx", "c++", "h", "hh", "hpp", "hxx", "h++", "ipp", "tpp"}, ptr: tscpp.Language, query: "cpp"},
	{name: "go", exts: []string{"go"}, ptr: tsgo.Language, query: "go"},
	{name: "java", exts: []string{"java"}, ptr: tsjava.Language, query: "java"},
	{name: "javascript", exts: []string{"js", "mjs", "cjs", "jsx"}, ptr: tsjs.Language, query: "javascript"},
	{name: "kotlin", exts: []string{"kt", "kts"}, ptr: tskotlin.Language, query: "kotlin"},
	{name: "lua", exts: []string{"lua"}, ptr: tslua.Language, query: "lua"},
	{name: "php", exts: []string{"php"}, ptr: tsphp.LanguagePHP, query: "php"},
	{name: "python", exts: []string{"py", "pyi"}, ptr: tspython.Language, query: "python"},
	{name: "ruby", exts: []string{"rb", "rake", "gemspec"}, ptr: tsruby.Language, query: "ruby"},
	{name: "rust", exts: []string{"rs"}, ptr: tsrust.Language, query: "rust"},
	{name: "scala", exts: []string{"scala", "sc"}, ptr: tsscala.Language, query: "scala"},
	{name: "tsx", exts: []string{"tsx"}, ptr: tsts.LanguageTSX, query: "typescript"},
	{name: "typescript", exts: []string{"ts", "mts", "cts"}, ptr: tsts.LanguageTypescript, query: "typescript"},
	{name: "zig", exts: []string{"zig"}, ptr: tszig.Language, query: "zig"},
}

var byExt = func() map[string]*language {
	m := map[string]*language{}
	for _, l := range languages {
		for _, e := range l.exts {
			m[e] = l
		}
	}
	return m
}()

// load compiles the language's query the first time it is needed.
func (l *language) load() error {
	l.once.Do(func() {
		src, err := queries.ReadFile("queries/" + l.query + ".scm")
		if err != nil {
			l.err = err
			return
		}
		l.lang = sitter.NewLanguage(l.ptr())
		q, qerr := sitter.NewQuery(l.lang, string(src))
		if qerr != nil {
			l.err = fmt.Errorf("%s query: %s", l.name, qerr.Error())
			return
		}
		l.q = q
	})
	return l.err
}

// languageFor returns the language path is parsed as, or nil.
func languageFor(path string) *language {
	return byExt[strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))]
}

// Languages lists the names of the languages definitions are found in.
func Languages() []string {
	names := make([]string, len(languages))
	for i, l := range languages {
		names[i] = l.name
	}
	return names
}

// def is a definition found by the query, before parents are assigned.
type def struct {
	database.Symbol
	start, end uint
	pattern    uint
}

// Extract returns the definitions in src, in source order. Files in other
// languages have none.
func Extract(path string, src []byte) ([]database.Symbol, error) {

	l := languageFor(path)
	if l == nil {
		return nil, nil
	}
	if err := l.load(); err != nil {
		return nil, err
	}

	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(l.lang); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	read := func(off int, _ sitter.Point) []byte {
		if off >= len(src) {
			return nil
		}
		return src[off:min(off+chunk, len(src))]
	}
	tree := parser.ParseWithOptions(read, nil, &sitter.ParseOptions{
		ProgressCallback: func(sitter.ParseState) bool { return time.Now().After(deadline) },
	})
	if tree == nil {
		return nil, fmt.Errorf("parse %s: timed out after %s", path, timeout)
	}
	defer tree.Close()

	cursor := sitter.NewQueryCursor()
	defer cursor.Close()

	names := l.q.CaptureNames()

	// A node can match several patterns (a Go struct is also a type); the
	// first pattern in the query file wins.
	found := map[[2]uint]def{}

	matches := cursor.Matches(l.q, tree.RootNode(), src)
	for m := matches.Next(); m != nil; m = matches.Next() {

		var d def
		var node *sitter.Node
		var nameStart uint
		for _, c := range m.Captures {
			capture := names[c.Index]
			switch {
			case capture == "name":
				d.Name = strings.TrimSpace(c.Node.Utf8Text(src))
				nameStart = c.Node.StartByte()
			case capture == "parent":
				d.Parent = strings.TrimSpace(c.Node.Utf8Text(src))
			case strings.HasPrefix(capture, "definition."):
				d.Kind = strings.TrimPrefix(capture, "definition.")
				n := c.Node
				node = &n
			}
		}
		if node == nil || d.Name == "" {
			continue
		}

		key := [2]uint{node.StartByte(), node.EndByte()}
		if prev, ok := found[key]; ok && prev.pattern <= m.PatternIndex {
			continue
		}

		d.Line = int(node.StartPosition().Row) + 1
		d.EndLine = int(node.EndPosition().Row) + 1
		// A node that ends with its newline (a #define) ends on the line before.
		if end := node.EndPosition(); end.Column == 0 && end.Row > node.StartPosition().Row {
			d.EndLine--
		}
		d.Signature = lineAt(src, nameStart)
		d.start, d.end, d.pattern = key[0], key[1], m.PatternIndex
		found[key] = d
	}

	defs := make([]def, 0, len(found))
	for _, d := range found {
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].start != defs[j].start {
			return defs[i].start < defs[j].start
		}
		return defs[i].end > defs[j].end // outer before inner
	})

	// The innermost enclosing definition is the parent, unless the query named one.
	out := make([]database.Symbol, len(defs))
	var open []def
	for i, d := range defs {
		for len(open) > 0 && open[len(open)-1].end < d.end {
			open = open[:len(open)-1]
		}
		if d.Parent == "" && len(open) > 0 {
			d.Parent = open[len(open)-1].Name
		}
		out[i] = d.Symbol
		open = append(open, d)
	}

	return out, nil
}

// lineAt returns the trimmed line containing byte offset off, cut to maxSignature.
func lineAt(src []byte, off uint) string {

	if int(off) > len(src) {
		return ""
	}

	start := bytes.LastIndexByte(src[:off], '\n') + 1
	end := bytes.IndexByte(src[off:], '\n')
	if end < 0 {
		end = len(src)
	} else {
		end += int(off)
	}

	line := strings.TrimSpace(string(src[start:end]))
	if len(line) > maxSignature {
		line = strings.ToValidUTF8(line[:maxSignature], "") + "…"
	}
	return line
}
