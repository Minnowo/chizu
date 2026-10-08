// Package ignore implements .chizuignore files, which use gitignore syntax.
//
// Each directory may contain a .chizuignore whose patterns are relative to
// that directory. Rules from deeper files are checked after their parents,
// and the last matching pattern wins. If the root directory has no
// .chizuignore, Defaults are used in its place.
package ignore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// FileName is the name of the ignore file looked for in each directory.
const FileName = ".chizuignore"

// Defaults are the rules used when the root has no .chizuignore.
var Defaults = []string{
	".git/",
	".hg/",
	".svn/",
	"node_modules/",
	".chizu.sqlite",
	".chizu.sqlite-*",
}

type pattern struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// Rules are the patterns from a single ignore file.
type Rules struct {
	// base is the slash-separated directory the rules are relative to ("" for the root).
	base     string
	patterns []pattern
}

// Parse reads gitignore-style patterns relative to base.
func Parse(base string, r io.Reader) (*Rules, error) {

	rules := &Rules{base: strings.Trim(filepath.ToSlash(base), "/")}

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		p, ok, err := compile(sc.Text())
		if err != nil {
			return nil, err
		}
		if ok {
			rules.patterns = append(rules.patterns, p)
		}
	}

	return rules, sc.Err()
}

func compile(line string) (pattern, bool, error) {

	line = strings.TrimSuffix(line, "\r")

	// Trailing spaces are ignored unless escaped.
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}

	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false, nil
	}

	var p pattern

	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}

	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimRight(line, "/")
	}

	if line == "" {
		return pattern{}, false, nil
	}

	// A slash anywhere but the end anchors the pattern to its directory.
	if strings.Contains(line, "/") {
		line = strings.TrimPrefix(line, "/")
	} else {
		line = "**/" + line
	}

	segs := strings.Split(line, "/")

	var sb strings.Builder
	sb.WriteString("^")
	for i, seg := range segs {
		last := i == len(segs)-1
		if seg == "**" {
			if last {
				sb.WriteString(".*")
			} else {
				sb.WriteString("(?:.*/)?")
			}
			continue
		}
		sb.WriteString(segmentRegex(seg))
		if !last {
			sb.WriteString("/")
		}
	}
	sb.WriteString("$")

	re, err := regexp.Compile(sb.String())
	if err != nil {
		return pattern{}, false, fmt.Errorf("bad ignore pattern %q: %w", line, err)
	}
	p.re = re

	return p, true, nil
}

// segmentRegex converts one path segment's glob into a regexp.
func segmentRegex(seg string) string {

	var sb strings.Builder

	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch c {
		case '\\':
			if i+1 < len(seg) {
				i++
				sb.WriteString(regexp.QuoteMeta(string(seg[i])))
			}
		case '*':
			sb.WriteString("[^/]*")
		case '?':
			sb.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(seg[i+1:], ']')
			if end < 0 {
				sb.WriteString(`\[`)
				continue
			}
			class := seg[i+1 : i+1+end]
			i += end + 1
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			sb.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}

	return sb.String()
}

// match reports whether any pattern matched rel, and if so whether it ignores it.
func (r *Rules) match(rel string, isDir bool) (matched, ignored bool) {

	if r.base != "" {
		if !strings.HasPrefix(rel, r.base+"/") {
			return false, false
		}
		rel = rel[len(r.base)+1:]
	}

	for _, p := range r.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if p.re.MatchString(rel) {
			matched, ignored = true, !p.negate
		}
	}

	return matched, ignored
}

// Matches reports whether the last pattern matching rel is a non-negated one.
func (r *Rules) Matches(rel string, isDir bool) bool {
	_, ignored := r.match(rel, isDir)
	return ignored
}

// Matcher combines the rules of every ignore file seen during a walk.
// Paths are checked top-down: once a directory is ignored the walker should
// skip it, so its contents are never checked.
type Matcher struct {
	sets []*Rules
}

// NewMatcher loads root's .chizuignore, or Defaults if it does not exist.
func NewMatcher(root string) (*Matcher, error) {

	m := &Matcher{}

	f, err := os.Open(filepath.Join(root, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		rules, err := Parse("", strings.NewReader(strings.Join(Defaults, "\n")))
		if err != nil {
			return nil, err
		}
		m.Add(rules)
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rules, err := Parse("", f)
	if err != nil {
		return nil, err
	}
	m.Add(rules)

	return m, nil
}

// Add appends a set of rules. Parents must be added before their children.
func (m *Matcher) Add(r *Rules) {
	m.sets = append(m.sets, r)
}

// LoadDir loads the ignore file in root/rel, if there is one.
// The root's own file is loaded by NewMatcher.
func (m *Matcher) LoadDir(root, rel string) error {

	if rel == "" || rel == "." {
		return nil
	}

	f, err := os.Open(filepath.Join(root, rel, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	rules, err := Parse(rel, f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(rel, FileName), err)
	}
	m.Add(rules)

	return nil
}

// Ignored reports whether the slash-separated path rel (relative to the root) is ignored.
func (m *Matcher) Ignored(rel string, isDir bool) bool {

	ignored := false

	for _, set := range m.sets {
		if matched, ign := set.match(rel, isDir); matched {
			ignored = ign
		}
	}

	return ignored
}

// DefaultFile is the content written by WriteDefault.
func DefaultFile() string {
	var sb strings.Builder
	sb.WriteString("# chizu ignore rules (gitignore syntax).\n")
	sb.WriteString("# These are the built-in defaults; delete a line to index those files.\n")
	sb.WriteString("# Subdirectories may have their own .chizuignore.\n\n")
	for _, d := range Defaults {
		sb.WriteString(d + "\n")
	}
	return sb.String()
}

// WriteDefault writes a .chizuignore containing Defaults into dir and returns its path.
// It fails with fs.ErrExist if the file already exists, unless force is set.
func WriteDefault(dir string, force bool) (string, error) {

	path := filepath.Join(dir, FileName)

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flags |= os.O_EXCL
	}

	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return path, err
	}

	if _, err := f.WriteString(DefaultFile()); err != nil {
		f.Close()
		return path, err
	}

	return path, f.Close()
}
