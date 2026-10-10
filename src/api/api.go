// Package api serves the JSON HTTP API and the web UI.
package api

import (
	"chizu/src/version"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"chizu/src/database"
	"chizu/src/indexer"
	"chizu/src/search"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

//go:embed ui/index.html ui/ponkotsu.css
var ui embed.FS

type Config struct {
	DB *database.DB
	// DBPath is reported by /api/info and never indexed.
	DBPath string
	// ReadOnly rejects index and remove requests.
	ReadOnly bool
	Log      zerolog.Logger
}

type server struct {
	Config
}

// New returns a router serving the API under /api and the UI at /.
func New(cfg Config) chi.Router {

	s := &server{Config: cfg}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.logRequests)

	r.Get("/", s.handleUI)
	r.Get("/ponkotsu.css", s.handleCSS)

	r.Route("/api", func(r chi.Router) {
		r.Get("/info", s.handleInfo)
		r.Get("/search", s.handleSearch)
		r.Get("/roots", s.handleRoots)
		r.Get("/files", s.handleFiles)
		r.Get("/file", s.handleFile)

		r.Group(func(r chi.Router) {
			r.Use(s.requireWritable)
			r.Post("/index", s.handleIndex)
			r.Delete("/roots", s.handleRemove)
			r.Post("/vacuum", s.handleVacuum)
		})
	})

	return r
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.Log.Debug().Str("method", r.Method).Str("path", r.URL.Path).Int("status", ww.Status()).Msg("request")
	})
}

func (s *server) requireWritable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ReadOnly {
			writeError(w, http.StatusForbidden, errors.New("server is read-only"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// statusFor maps package errors onto HTTP status codes.
func statusFor(err error) int {
	switch {
	case errors.Is(err, search.ErrEmptyQuery), errors.Is(err, search.ErrBadQuery):
		return http.StatusBadRequest
	case errors.Is(err, search.ErrNotIndexed), errors.Is(err, database.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	data, err := ui.ReadFile("ui/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *server) handleCSS(w http.ResponseWriter, r *http.Request) {
	data, err := ui.ReadFile("ui/ponkotsu.css")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Write(data)
}

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"db":        s.DBPath,
		"tokenizer": s.DB.Tokenizer(),
		"driver":    database.Driver,
		"version":   version.Version,
		"read_only": s.ReadOnly,
	})
}

// intParam parses an optional integer query parameter.
func intParam(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("invalid " + name + ": " + v)
	}
	return n, nil
}

// listParam reads a parameter given repeatedly and/or comma-separated.
func listParam(r *http.Request, name string) []string {
	var out []string
	for _, v := range r.URL.Query()[name] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func boolParam(r *http.Request, name string) bool {
	b, _ := strconv.ParseBool(r.URL.Query().Get(name))
	return b
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {

	q := search.Query{
		Text:       r.URL.Query().Get("q"),
		Raw:        boolParam(r, "raw"),
		Dir:        r.URL.Query().Get("dir"),
		PathPrefix: r.URL.Query().Get("path"),
		Exts:       listParam(r, "ext"),
	}

	var err error
	for name, dst := range map[string]*int{"limit": &q.Limit, "offset": &q.Offset, "lines": &q.MaxLines} {
		if *dst, err = intParam(r, name); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}

	results, err := search.Search(r.Context(), s.DB, q)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *server) handleRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.DB.ListRoots(r.Context())
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
}

func (s *server) handleFiles(w http.ResponseWriter, r *http.Request) {

	q := search.FileQuery{
		Dir:        r.URL.Query().Get("dir"),
		PathPrefix: r.URL.Query().Get("path"),
		Contains:   r.URL.Query().Get("q"),
		Exts:       listParam(r, "ext"),
	}

	var err error
	for name, dst := range map[string]*int{"limit": &q.Limit, "offset": &q.Offset} {
		if *dst, err = intParam(r, name); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}

	list, err := search.ListFiles(r.Context(), s.DB, q)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	writeJSON(w, http.StatusOK, list)
}

func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {

	var f database.File
	var err error

	switch {
	case r.URL.Query().Get("id") != "":
		id, perr := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if perr != nil {
			writeError(w, http.StatusBadRequest, errors.New("invalid id"))
			return
		}
		f, err = search.GetFileByID(r.Context(), s.DB, id)
	case r.URL.Query().Get("path") != "":
		f, err = search.GetFile(r.Context(), s.DB, r.URL.Query().Get("path"))
	default:
		writeError(w, http.StatusBadRequest, errors.New("id or path is required"))
		return
	}

	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	if boolParam(r, "raw") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, f.Content)
		return
	}

	writeJSON(w, http.StatusOK, f)
}

type indexRequest struct {
	indexer.Options
	Dirs         []string `json:"dirs"`
	ClearFilters bool     `json:"clear_filters"`
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {

	var req indexRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	opts := req.Options
	opts.ClearFilters = req.ClearFilters
	opts.Log = s.Log
	if s.DBPath != "" {
		opts.SkipPaths = []string{s.DBPath, s.DBPath + "-wal", s.DBPath + "-shm", s.DBPath + "-journal"}
	}

	stats := []indexer.Stats{}
	errs := []string{}

	if len(req.Dirs) == 0 {
		all, err := indexer.IndexAll(r.Context(), s.DB, opts)
		stats = append(stats, all...)
		if err != nil {
			errs = append(errs, strings.Split(err.Error(), "\n")...)
		}
	} else {
		for _, dir := range req.Dirs {
			st, err := indexer.Index(r.Context(), s.DB, dir, opts)
			if err != nil {
				errs = append(errs, dir+": "+err.Error())
				continue
			}
			stats = append(stats, st)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"stats": stats, "errors": errs})
}

func (s *server) handleRemove(w http.ResponseWriter, r *http.Request) {

	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path is required"))
		return
	}

	if err := indexer.Remove(r.Context(), s.DB, path); err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleVacuum(w http.ResponseWriter, r *http.Request) {

	before, err := s.DB.Size()
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	if err := s.DB.Vacuum(r.Context()); err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	after, err := s.DB.Size()
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]int64{"before": before, "after": after})
}
