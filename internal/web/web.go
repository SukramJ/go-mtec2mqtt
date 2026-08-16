// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"
)

// staticFS holds the compiled single-page app. It is hand-written
// vanilla HTML/CSS/JS (no build step) so the asset tree is committed
// as-is and embedded verbatim.
//
// Home-Assistant ingress: HA serves the add-on UI behind a path prefix
// (…/api/hassio_ingress/<token>/). All asset and API URLs in the SPA are
// kept relative (no leading slash) so the page works both when reached
// directly and behind that prefix — no server-side rewriting needed.
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
	s.handler = s.withSecurityHeaders(s.withAuth(s.withWriteDeadline(s.routes())))
	return s
}

// routes wires the REST API and the embedded SPA onto a ServeMux.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/registers", s.handleRegisters)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	// POST /api/write mutates the inverter, so it alone gets the extra
	// same-origin/Content-Type guard — the GET endpoints below are
	// read-only and pose no CSRF risk.
	mux.Handle("POST /api/write", s.requireSameOriginJSON(http.HandlerFunc(s.handleWrite)))
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

// requireSameOriginJSON guards state-changing requests (currently only
// POST /api/write) against cross-site submissions. A cross-site
// <form>/fetch POST that skips CORS preflight can only ever carry a
// "simple" Content-Type (e.g. text/plain) — never application/json —
// and the browser attaches Basic-Auth credentials automatically, so
// without this guard a foreign page could trigger a register write.
// Two independent checks close that gap; both are scoped to this one
// handler so the read-only GET endpoints stay overhead-free.
func (s *Server) requireSameOriginJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			s.writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}

		// Sec-Fetch-Site is sent by modern browsers and is not
		// spoofable from script; when present it is the most reliable
		// signal. "same-origin" and "none" (browser-initiated, e.g. a
		// bookmarklet or address-bar navigation) are fine.
		site := r.Header.Get("Sec-Fetch-Site")
		if site != "" && site != "same-origin" && site != "none" {
			s.writeError(w, http.StatusForbidden, "cross-site request rejected")
			return
		}

		// Origin is absent for many legitimate same-origin requests
		// (older browsers, curl, some proxies) — only reject when it is
		// present AND disagrees with the request's own Host. This also
		// covers the Home Assistant Ingress case: the SPA only ever
		// issues relative fetches, so under Ingress the browser's
		// Origin (if sent at all) is the same host the request arrives
		// on; X-Forwarded-Host is accepted as an alternative match for
		// setups where a reverse proxy rewrites the Host header but
		// preserves the original in X-Forwarded-Host. That header can't
		// be forged by the very cross-site requests this guard defends
		// against — setting a custom header forces a CORS preflight,
		// which the browser blocks here since no
		// Access-Control-Allow-Origin is ever returned.
		//
		// Sec-Fetch-Site: same-origin is a stronger, browser-guaranteed
		// signal than the Origin/Host comparison below, so when it is
		// present this comparison is skipped entirely. That matters
		// behind the Home Assistant Ingress supervisor proxy, which may
		// not set X-Forwarded-Host (or may set it to something that
		// doesn't match the browser's Origin) — without this bypass a
		// legitimate same-origin request would 403 in that setup.
		if site != "same-origin" {
			if origin := r.Header.Get("Origin"); origin != "" {
				originURL, err := url.Parse(origin)
				if err != nil || (originURL.Host != r.Host && originURL.Host != r.Header.Get("X-Forwarded-Host")) {
					s.writeError(w, http.StatusForbidden, "cross-origin request rejected")
					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// withSecurityHeaders sets a small set of hardening headers on every
// response. Home Assistant serves this UI embedded in an iframe via
// Ingress, so — unlike a typical hardened CSP — this deliberately does
// NOT set X-Frame-Options or a frame-ancestors directive; either would
// break that embedding.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// writeDeadline bounds how long the response write for a normal
// (non-streaming) request may take, measured from the moment the request
// enters withWriteDeadline. Without it, a client that stops reading mid
// -response (a "slow-read" client) blocks the handler's write call
// indefinitely — the server has no WriteTimeout (that would kill the SSE
// stream, see [Server.Run]) — pinning the handler goroutine and its file
// descriptor forever. A var, not a const, so tests can shorten it.
var writeDeadline = 30 * time.Second

// withWriteDeadline sets a per-request response write deadline via
// [http.ResponseController] on every route except the SSE stream
// (/api/events), which manages its own per-frame deadlines instead (see
// handleEvents / sseWriteTimeout) — a single request-scoped deadline
// would be far too short for a stream that is meant to stay open.
func (s *Server) withWriteDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			next.ServeHTTP(w, r)
			return
		}
		// Best-effort: not every ResponseWriter supports write deadlines,
		// and a failure to set one just means the pre-existing behaviour.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeDeadline))
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
		// Reap idle keep-alive connections so abandoned clients cannot pin
		// goroutines and file descriptors forever. This applies only
		// between requests, so the long-lived SSE stream at /api/events is
		// unaffected — which is also why ReadTimeout/WriteTimeout must stay
		// unset (either would kill that stream). The write-endpoint body
		// read is bounded per-request in handleWrite instead.
		IdleTimeout: 120 * time.Second,
		// Tie every request context to the run context so long-lived
		// handlers (the SSE stream) observe shutdown. Without this,
		// Shutdown below would always burn its full deadline while a
		// dashboard tab is connected, since Shutdown waits for active
		// handlers but never cancels their request contexts.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	// Bind synchronously so a failure (port in use, permission denied, …)
	// surfaces as a returned error before anything is logged as
	// listening — logging "web.listening" ahead of a successful bind
	// would claim readiness that was never achieved. A ListenConfig (vs.
	// the package-level net.Listen) ties the bind syscall to ctx too.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Bind)
	if err != nil {
		return fmt.Errorf("web: listen on %s: %w", s.cfg.Bind, err)
	}

	s.log.Info("web.listening",
		slog.String("bind", s.cfg.Bind),
		slog.Bool("auth", s.cfg.User != ""))

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		// ctx is cancelled here; derive a non-cancelled child so the graceful
		// shutdown still gets its full deadline without breaking the chain.
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			// Deadline expired with connections still active — close them
			// hard rather than abandoning them to process exit.
			_ = srv.Close()
		}
		s.log.Info("web.stopped")
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
