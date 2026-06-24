// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"
)

// staticFS holds the compiled single-page app. It is hand-written
// vanilla HTML/CSS/JS (no build step) so the asset tree is committed
// as-is and embedded verbatim.
//
//go:embed static
var staticFS embed.FS

// Config parameterises the web server.
type Config struct {
	// Bind is the listen address "host:port".
	Bind string
	// User / Password enable HTTP Basic auth when both are non-empty.
	User     string
	Password string
	// Logger is optional; nil → slog.Default().
	Logger *slog.Logger
}

// Server is the embedded dashboard HTTP server.
type Server struct {
	cfg     Config
	backend Backend
	log     *slog.Logger
	handler http.Handler
}

// New builds a Server. It does not bind a socket — call [Server.Run].
func New(cfg Config, backend Backend) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, backend: backend, log: log}
	s.handler = s.withAuth(s.routes())
	return s
}

// routes wires the REST API and the embedded SPA onto a ServeMux.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/registers", s.handleRegisters)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/write", s.handleWrite)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	// The SPA: index.html + app.css + app.js. http.FileServerFS serves
	// "/" as index.html and 404s unknown paths — there is no client-side
	// routing to fall back for, so the default behaviour is exactly right.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Embedded path is a compile-time constant; this cannot fail in a
		// correctly built binary.
		panic("web: embed sub fs: " + err.Error())
	}
	mux.Handle("GET /", http.FileServerFS(sub))

	return mux
}

// withAuth wraps next with HTTP Basic auth when credentials are
// configured. With no credentials it returns next unchanged so there is
// zero per-request overhead on an unauthenticated deployment.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.User == "" && s.cfg.Password == "" {
		return next
	}
	wantUser := []byte(s.cfg.User)
	wantPass := []byte(s.cfg.Password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// Constant-time compares so a timing side-channel can't probe the
		// credentials. Both fields must match.
		userOK := subtle.ConstantTimeCompare([]byte(user), wantUser) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), wantPass) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="mtec2mqtt", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Run binds the socket and serves until ctx is cancelled, then shuts
// down gracefully. Returns nil on a clean shutdown; a bind failure
// surfaces as a non-nil error so the daemon's errgroup tears everything
// down (a misconfigured port should be loud, not silently ignored).
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Bind,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	s.log.Info("web.listening",
		slog.String("bind", s.cfg.Bind),
		slog.Bool("auth", s.cfg.User != ""))

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		s.log.Info("web.stopped")
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
