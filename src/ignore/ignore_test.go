package ignore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func parse(t *testing.T, base, text string) *Rules {
	t.Helper()
	r, err := Parse(base, strings.NewReader(text))
	require.NoError(t, err)
	return r
}

func TestPatternMatching(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		isDir   bool
		want    bool
	}{
		// Unanchored names match at any depth.
		{"*.log", "a.log", false, true},
		{"*.log", "deep/dir/a.log", false, true},
		{"*.log", "a.log.txt", false, false},
		{"build", "build", true, true},
		{"build", "src/build", false, true},

		// Trailing slash only matches directories.
		{"tmp/", "tmp", true, true},
		{"tmp/", "tmp", false, false},
		{"tmp/", "a/tmp", true, true},

		// A leading or middle slash anchors to the ignore file's directory.
		{"/root.txt", "root.txt", false, true},
		{"/root.txt", "sub/root.txt", false, false},
		{"docs/*.md", "docs/a.md", false, true},
		{"docs/*.md", "x/docs/a.md", false, false},
		{"docs/*.md", "docs/sub/a.md", false, false},

		// Double star.
		{"**/foo", "foo", false, true},
		{"**/foo", "a/b/foo", false, true},
		{"a/**/b", "a/b", false, true},
		{"a/**/b", "a/x/y/b", false, true},
		{"a/**", "a/x/y", false, true},
		{"a/**", "a", true, false},

		// Character classes and single-char wildcard.
		{"file?.txt", "file1.txt", false, true},
		{"file?.txt", "file10.txt", false, false},
		{"[abc].md", "b.md", false, true},
		{"[!abc].md", "b.md", false, false},
		{"[!abc].md", "d.md", false, true},

		// Escapes.
		{`\#notcomment`, "#notcomment", false, true},
		{`\!bang`, "!bang", false, true},
	}

	for _, c := range cases {
		t.Run(c.pattern+"~"+c.path, func(t *testing.T) {
			var m Matcher
			m.Add(parse(t, "", c.pattern))
			require.Equal(t, c.want, m.Ignored(c.path, c.isDir))
		})
	}
}

func TestCommentsAndBlankLines(t *testing.T) {
	r := parse(t, "", "# comment\n\n   \n*.log\n")
	require.Len(t, r.patterns, 1)
}

func TestNegationLastMatchWins(t *testing.T) {
	var m Matcher
	m.Add(parse(t, "", "*.md\n!keep.md\n"))
	require.True(t, m.Ignored("a.md", false))
	require.False(t, m.Ignored("keep.md", false))
	require.False(t, m.Ignored("x.txt", false))
}

func TestNestedRulesAreRelativeToTheirDirectory(t *testing.T) {
	var m Matcher
	m.Add(parse(t, "", "*.log\n"))
	m.Add(parse(t, "sub", "/local.txt\n!important.log\n"))

	require.True(t, m.Ignored("sub/local.txt", false))
	require.False(t, m.Ignored("local.txt", false))
	require.False(t, m.Ignored("other/local.txt", false))

	// Deeper files override the parent's rules.
	require.False(t, m.Ignored("sub/important.log", false))
	require.True(t, m.Ignored("important.log", false))
	// But rules from sub/ do not leak into sibling directories.
	require.True(t, m.Ignored("subway/important.log", false))
}

func TestLoadDirUsesDefaultsWithoutIgnoreFile(t *testing.T) {
	dir := t.TempDir()

	m, err := NewMatcher(dir)
	require.NoError(t, err)
	require.True(t, m.Ignored(".git", true))
	require.True(t, m.Ignored(".chizu.sqlite", false))
	require.False(t, m.Ignored("notes.md", false))
}

func TestIgnoreFileReplacesDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, FileName), []byte("*.tmp\n"), 0o644))

	m, err := NewMatcher(dir)
	require.NoError(t, err)
	require.False(t, m.Ignored(".git", true), "user removed .git from the ignore file, so it is indexed")
	require.True(t, m.Ignored("x.tmp", false))
}

func TestLoadNestedDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", FileName), []byte("secret.md\n"), 0o644))

	m, err := NewMatcher(dir)
	require.NoError(t, err)
	require.NoError(t, m.LoadDir(dir, "sub"))
	require.True(t, m.Ignored("sub/secret.md", false))
	require.False(t, m.Ignored("secret.md", false))

	// A directory without an ignore file is fine.
	require.NoError(t, m.LoadDir(dir, "nothing-here"))
}

func TestWriteDefault(t *testing.T) {
	dir := t.TempDir()

	path, err := WriteDefault(dir, false)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, FileName), path)

	// The written file behaves exactly like the built-in defaults.
	m, err := NewMatcher(dir)
	require.NoError(t, err)
	require.True(t, m.Ignored(".git", true))
	require.True(t, m.Ignored(".chizu.sqlite-wal", false))

	_, err = WriteDefault(dir, false)
	require.ErrorIs(t, err, os.ErrExist)

	_, err = WriteDefault(dir, true)
	require.NoError(t, err)
}
