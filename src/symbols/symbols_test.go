package symbols

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chizu/src/database"

	"github.com/stretchr/testify/require"
)

// brief renders symbols as "line-endLine kind parent.name" for comparison.
func brief(syms []database.Symbol) []string {
	out := []string{}
	for _, s := range syms {
		name := s.Name
		if s.Parent != "" {
			name = s.Parent + "." + name
		}
		out = append(out, fmt.Sprintf("%d-%d %s %s", s.Line, s.EndLine, s.Kind, name))
	}
	return out
}

func TestQueriesCompile(t *testing.T) {
	for _, l := range languages {
		require.NoError(t, l.load(), l.name)
	}
}

// Every language has a sample in testdata/.
func TestExtract(t *testing.T) {
	want := map[string][]string{
		"sample.c": {
			"1-1 constant LIMIT",
			"2-2 macro SQUARE",
			"4-6 struct point",
			"8-8 type point_t",
			"10-10 enum color",
			"12-14 function add",
			"16-18 function name",
		},
		"sample.cpp": {
			"1-21 module geo",
			"3-7 class geo.Shape",
			"5-5 method Shape.area",
			"6-6 method Shape.~Shape",
			"9-11 struct geo.Point",
			"13-15 method Circle.area",
			"17-17 function geo.add",
			"19-19 type geo.Id",
		},
		"sample.cs": {
			"1-15 module Geo",
			"3-8 class Geo.Greeter",
			"5-5 constructor Greeter.Greeter",
			"6-6 property Greeter.Name",
			"7-7 method Greeter.Hello",
			"10-10 interface Geo.IShape",
			"10-10 method IShape.Area",
			"12-12 struct Geo.Point",
			"14-14 enum Geo.Color",
		},
		"sample.go": {
			"3-3 constant Limit",
			"5-5 variable Default",
			"7-7 struct Store",
			"9-11 interface Reader",
			"13-13 type ID",
			"15-17 method Store.Open",
			"19-19 method Store.Close",
			"21-23 function New",
		},
		"sample.java": {
			"3-9 class Greeter",
			"4-4 constructor Greeter.Greeter",
			"6-8 method Greeter.hello",
			"11-13 interface Shape",
			"12-12 method Shape.area",
			"15-15 enum Color",
			"17-17 class Point",
		},
		"sample.js": {
			"1-5 class Greeter",
			"2-2 method Greeter.constructor",
			"3-3 method Greeter.hello",
			"4-4 method Greeter.#secret",
			"7-7 function main",
			"9-9 function add",
			"11-11 function gen",
			"13-13 function run",
		},
		"sample.kt": {
			"3-5 class Greeter",
			"4-4 method Greeter.hello",
			"7-9 interface Shape",
			"8-8 method Shape.area",
			"11-13 object Registry",
			"12-12 method Registry.all",
			"15-15 function main",
			"17-17 type Name",
		},
		"sample.lua": {
			"3-3 function M.helper",
			"5-5 method M.method",
			"7-7 function main",
			"9-9 function private",
		},
		"sample.php": {
			"2-2 module App",
			"4-4 interface Shape",
			"4-4 method Shape.area",
			"6-6 trait Greets",
			"6-6 method Greets.greet",
			"8-10 class Greeter",
			"9-9 method Greeter.hello",
			"12-12 function main",
		},
		"sample.py": {
			"1-1 constant LIMIT",
			"5-15 class Greeter",
			"6-7 method Greeter.hello",
			"10-11 method Greeter.make",
			"13-15 class Greeter.Inner",
			"14-15 method Inner.deep",
			"18-20 function main",
			"19-20 function main.nested",
		},
		"sample.rb": {
			"1-11 module Util",
			"2-10 class Util.Greeter",
			"3-5 method Greeter.hello",
			"7-9 method Greeter.make",
			"13-13 method main",
		},
		"sample.rs": {
			"1-1 constant LIMIT",
			"3-5 struct Point",
			"7-9 enum Color",
			"11-13 trait Shape",
			"12-12 method Shape.area",
			"16-18 method Point.new",
			"22-24 method Point.area",
			"27-29 module util",
			"28-28 function util.helper",
			"31-33 macro square",
			"35-35 function main",
		},
		"sample.scala": {
			"3-5 class Greeter",
			"4-4 method Greeter.hello",
			"7-9 object Registry",
			"8-8 method Registry.make",
			"11-13 trait Shape",
			"12-12 method Shape.area",
			"15-15 enum Color",
			"17-17 type Name",
			"19-19 function top",
		},
		"sample.sh": {
			"2-4 function greet",
			"6-8 function main",
		},
		"sample.ts": {
			"1-3 interface Shape",
			"2-2 method Shape.area",
			"5-5 type ID",
			"7-7 enum Color",
			"9-11 module Util",
			"10-10 function Util.helper",
			"13-15 class Base",
			"14-14 method Base.run",
			"17-20 class Circle",
			"18-18 method Circle.area",
			"19-19 method Circle.run",
			"22-22 function make",
		},
		"sample.tsx": {
			"1-1 interface Props",
			"3-5 function Hello",
			"7-7 function Bye",
		},
		"sample.zig": {
			"3-9 struct Point",
			"6-8 function Point.init",
			"11-11 enum Color",
			"13-13 function main",
		},
	}

	samples, err := filepath.Glob("testdata/sample.*")
	require.NoError(t, err)
	require.Len(t, samples, len(want))

	covered := map[string]bool{}
	for _, p := range samples {
		covered[languageFor(p).name] = true
		src, err := os.ReadFile(p)
		require.NoError(t, err)
		syms, err := Extract(p, src)
		require.NoError(t, err, p)
		require.Equal(t, want[filepath.Base(p)], brief(syms), p)
	}
	require.ElementsMatch(t, Languages(), keys(covered), "every language has a sample")
}

func keys(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExtractSignature(t *testing.T) {
	syms, err := Extract("a.go", []byte("package a\n\n// Doc comment.\nfunc   Add(a, b int) int {\n\treturn a + b\n}\n"))
	require.NoError(t, err)
	require.Len(t, syms, 1)
	require.Equal(t, "func   Add(a, b int) int {", syms[0].Signature, "the trimmed line the name is on")

	long := "package a\n\nfunc Long(" + strings.Repeat("a int, ", 100) + ") {}\n"
	syms, err = Extract("a.go", []byte(long))
	require.NoError(t, err)
	require.Len(t, syms, 1)
	require.Len(t, syms[0].Signature, maxSignature+len("…"))
}

func TestExtractNothing(t *testing.T) {
	for path, src := range map[string]string{
		"notes.md":   "# Heading\n\nsome text\n",
		"data.json":  `{"a": 1}`,
		"notes.txt":  "func main() {}",
		"Makefile":   "all:\n\techo hi\n",
		"empty.go":   "",
		"broken.go":  "func (",
		"no-ext":     "def f(): pass",
		"config.yml": "a: 1\n",
	} {
		syms, err := Extract(path, []byte(src))
		require.NoError(t, err, path)
		require.Empty(t, syms, path)
	}
}

func TestExtractExtensionsIgnoreCase(t *testing.T) {
	syms, err := Extract("MAIN.GO", []byte("package a\nfunc A() {}\n"))
	require.NoError(t, err)
	require.Len(t, syms, 1)
}

func TestExtractLargeFile(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package a\n")
	for i := range 20000 {
		fmt.Fprintf(&sb, "func F%d() int {\n\treturn %d\n}\n", i, i)
	}
	syms, err := Extract("a.go", []byte(sb.String()))
	require.NoError(t, err)
	require.Len(t, syms, 20000)
	require.Equal(t, "F19999", syms[19999].Name)
	require.Equal(t, 2+3*19999, syms[19999].Line)
}

func TestExtractConcurrently(t *testing.T) {
	src := []byte("package a\n\nfunc A() {}\n\nfunc B() {}\n")
	done := make(chan []database.Symbol)
	for range 16 {
		go func() {
			syms, _ := Extract("a.go", src)
			done <- syms
		}()
	}
	for range 16 {
		require.Len(t, <-done, 2)
	}
}
