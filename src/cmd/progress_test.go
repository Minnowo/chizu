package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"chizu/src/indexer"

	"github.com/stretchr/testify/require"
)

func newTestStatus() (*statusLine, *bytes.Buffer, *time.Time) {
	var buf bytes.Buffer
	now := time.Unix(0, 0)
	s := &statusLine{w: &buf, now: func() time.Time { return now }}
	return s, &buf, &now
}

func TestStatusLine(t *testing.T) {
	s, buf, now := newTestStatus()

	s.update(indexer.Progress{Root: "/r", Files: 7, Path: "notes/a.md"})
	out := buf.String()
	require.True(t, strings.HasPrefix(out, "\r\x1b[K"))
	require.Contains(t, out, "7 files")
	require.Contains(t, out, "notes/a.md")

	// Redraws are throttled.
	s.update(indexer.Progress{Root: "/r", Files: 8, Path: "notes/b.md"})
	require.Equal(t, out, buf.String())

	*now = now.Add(time.Second)
	s.update(indexer.Progress{Root: "/r", Files: 9, Path: "notes/c.md"})
	require.Contains(t, buf.String(), "9 files")
}

func TestStatusLineTruncatesLongPaths(t *testing.T) {
	s, buf, _ := newTestStatus()
	s.update(indexer.Progress{Files: 1, Path: strings.Repeat("deep/", 40) + "file.md"})
	require.LessOrEqual(t, len([]rune(strings.TrimPrefix(buf.String(), "\r\x1b[K"))), 80)
	require.Contains(t, buf.String(), "file.md")
}

func TestStatusLineClear(t *testing.T) {
	s, buf, _ := newTestStatus()
	s.clear()
	require.Empty(t, buf.String(), "nothing drawn, nothing to clear")

	s.update(indexer.Progress{Files: 1, Path: "a"})
	buf.Reset()
	s.clear()
	require.Equal(t, "\r\x1b[K", buf.String())
}

func TestNoProgressWhenStderrIsNotATerminal(t *testing.T) {
	dir := notesDir(t)
	out := mustRun(t, "index", "--db", t.TempDir()+"/x.sqlite", dir)
	require.NotContains(t, out.stderr, "\r")
}

func TestStatusLineWriterClearsBeforeWriting(t *testing.T) {
	s, buf, _ := newTestStatus()
	s.update(indexer.Progress{Files: 1, Path: "a"})
	buf.Reset()

	_, err := s.writer(buf).Write([]byte("WRN something\n"))
	require.NoError(t, err)
	require.Equal(t, "\r\x1b[KWRN something\n", buf.String())
}
