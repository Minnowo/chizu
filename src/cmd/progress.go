package cmd

import (
	"fmt"
	"io"
	"time"

	"chizu/src/indexer"
)

const (
	statusInterval = 100 * time.Millisecond
	statusWidth    = 80
	clearLine      = "\r\x1b[K"
)

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// statusLine redraws a single "still working" line, throttled so huge
// directories do not spend their time writing to the terminal.
type statusLine struct {
	w     io.Writer
	now   func() time.Time
	last  time.Time
	frame int
	drawn bool
}

func newStatusLine(w io.Writer) *statusLine {
	return &statusLine{w: w, now: time.Now}
}

func (s *statusLine) update(p indexer.Progress) {

	now := s.now()
	if s.drawn && now.Sub(s.last) < statusInterval {
		return
	}
	s.last = now

	prefix := fmt.Sprintf("%c indexing %d files  ", spinner[s.frame%len(spinner)], p.Files)
	s.frame++

	path := []rune(p.Path)
	if room := statusWidth - len([]rune(prefix)); len(path) > room {
		path = append([]rune("…"), path[len(path)-room+1:]...)
	}

	fmt.Fprint(s.w, clearLine+prefix+string(path))
	s.drawn = true
}

// clear erases the line, if anything was drawn.
func (s *statusLine) clear() {
	if s.drawn {
		fmt.Fprint(s.w, clearLine)
		s.drawn = false
	}
}

// writer returns w wrapped so the status line is cleared before each write,
// keeping log lines from being glued onto it.
func (s *statusLine) writer(w io.Writer) io.Writer {
	return clearingWriter{s: s, w: w}
}

type clearingWriter struct {
	s *statusLine
	w io.Writer
}

func (c clearingWriter) Write(p []byte) (int, error) {
	c.s.clear()
	return c.w.Write(p)
}
