// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
)

// fakeBackend is a deterministic [Backend] for handler tests.
type fakeBackend struct {
	writeErr error
	lastKey  string
	lastVal  string
	changes  chan struct{}
	onCancel func() // invoked when a Changes subscription is released
}

func (f *fakeBackend) Snapshot() state.Snapshot {
	return state.Snapshot{
		Serial: "SN1",
		Groups: map[string]state.GroupView{
			"now-base": {Values: map[string]any{"power": 100.0}},
		},
	}
}

func (f *fakeBackend) Health() state.Health {
	return state.Health{Status: "ok", Serial: "SN1", Version: "test"}
}

func (f *fakeBackend) Config() state.ConfigView {
	return state.ConfigView{MQTTTopic: "MTEC"}
}

func (f *fakeBackend) Registers() []state.RegisterInfo {
	return []state.RegisterInfo{
		{Key: "40000", Name: "Charge", MQTT: "charge_power", Writable: true},
	}
}

func (f *fakeBackend) Write(_ context.Context, key, value string) error {
	f.lastKey, f.lastVal = key, value
	return f.writeErr
}

func (f *fakeBackend) Changes() (events <-chan struct{}, cancel func()) {
	if f.changes == nil {
		f.changes = make(chan struct{})
	}
	return f.changes, func() {
		if f.onCancel != nil {
			f.onCancel()
		}
	}
}

func newTestServer(cfg Config, b Backend) *httptest.Server {
	return httptest.NewServer(New(cfg, b).handler)
}

// bigRegisterBackend wraps fakeBackend to serve an arbitrarily large
// register catalog — used to force a JSON response body big enough to
// overflow shrunk socket buffers in
// TestWriteDeadlineDisconnectsStalledReaderOnNormalRoute.
type bigRegisterBackend struct {
	fakeBackend
	regs []state.RegisterInfo
}

func (b *bigRegisterBackend) Registers() []state.RegisterInfo { return b.regs }

// bufLimitListener shrinks the OS send buffer on every accepted
// connection so a stalled reader can fill it (combined with a shrunk
// client-side receive window) without needing a multi-megabyte response
// body to reliably provoke a blocking Write().
type bufLimitListener struct{ net.Listener }

func (l *bufLimitListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return c, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(2048)
	}
	return c, nil
}

// signalWriter is an io.Writer that pings a buffered channel (dropping
// the ping if the channel is already full) on every Write — used to
// observe that a slog.Logger call happened without racily inspecting a
// shared buffer from another goroutine.
type signalWriter struct{ ch chan struct{} }

func (w *signalWriter) Write(p []byte) (int, error) {
	select {
	case w.ch <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestHealthEndpoint(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var h state.Health
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Serial != "SN1" {
		t.Errorf("unexpected health: %+v", h)
	}
}

func TestStatusEndpointCombinesHealthAndSnapshot(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/api/status")
	defer func() { _ = res.Body.Close() }()
	var v liveView
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Health.Serial != "SN1" {
		t.Error("missing health in status")
	}
	if _, ok := v.Snapshot.Groups["now-base"]; !ok {
		t.Error("missing snapshot in status")
	}
}

func TestWriteSuccess(t *testing.T) {
	fb := &fakeBackend{}
	ts := newTestServer(Config{}, fb)
	defer ts.Close()

	body := strings.NewReader(`{"key":"charge_power","value":3000}`)
	res, err := http.Post(ts.URL+"/api/write", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if fb.lastKey != "charge_power" || fb.lastVal != "3000" {
		t.Errorf("backend got key=%q val=%q (numbers must normalise to ints)", fb.lastKey, fb.lastVal)
	}
}

func TestWriteErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown", modbus.ErrUnknownRegister, http.StatusNotFound},
		{"readonly", modbus.ErrNotWritable, http.StatusBadRequest},
		{"parse", modbus.ErrValueParse, http.StatusBadRequest},
		{"pseudo", modbus.ErrPseudoUnsupported, http.StatusBadRequest},
		{"disconnected", modbus.ErrNotConnected, http.StatusServiceUnavailable},
		{"transport", io.ErrUnexpectedEOF, http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(Config{}, &fakeBackend{writeErr: c.err})
			defer ts.Close()
			res, _ := http.Post(ts.URL+"/api/write", "application/json",
				strings.NewReader(`{"key":"x","value":1}`))
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != c.want {
				t.Errorf("status = %d, want %d", res.StatusCode, c.want)
			}
		})
	}
}

// TestWriteTransportFailureHidesDetailFromClient proves that a raw
// Modbus transport/framing error (the default branch of handleWrite's
// error switch) never reaches the client body verbatim — only a generic
// message does; the error detail is logged server-side only.
func TestWriteTransportFailureHidesDetailFromClient(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{writeErr: io.ErrUnexpectedEOF})
	defer ts.Close()

	res, err := http.Post(ts.URL+"/api/write", "application/json", strings.NewReader(`{"key":"x","value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusBadGateway)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body["error"], io.ErrUnexpectedEOF.Error()) {
		t.Errorf("raw transport error leaked to client: %q", body["error"])
	}
	if body["error"] != "write failed" {
		t.Errorf("error = %q, want generic \"write failed\"", body["error"])
	}
}

func TestWriteRejectsNonJSONContentType(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	// text/plain is exactly the content type a cross-site <form>/fetch
	// POST can send without triggering a CORS preflight — it must never
	// reach the write handler.
	res, err := http.Post(ts.URL+"/api/write", "text/plain", strings.NewReader(`{"key":"x","value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusUnsupportedMediaType)
	}
}

func TestWriteRejectsMissingContentType(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/write", strings.NewReader(`{"key":"x","value":1}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusUnsupportedMediaType)
	}
}

func TestWriteAllowsJSONWithCharsetParam(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	res, err := http.Post(ts.URL+"/api/write", "application/json; charset=utf-8",
		strings.NewReader(`{"key":"x","value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

func TestWriteRejectsForeignOrigin(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/write", strings.NewReader(`{"key":"x","value":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusForbidden)
	}
}

func TestWriteRejectsCrossSiteFetch(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/write", strings.NewReader(`{"key":"x","value":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusForbidden)
	}
}

func TestWriteAllowsSameOriginAndNoOrigin(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	// No Origin header at all (curl, older clients) must keep working.
	res, err := http.Post(ts.URL+"/api/write", "application/json", strings.NewReader(`{"key":"x","value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("no-origin status = %d, want 200", res.StatusCode)
	}

	// Origin matching the request's own host (the normal browser case,
	// and the Home Assistant Ingress case where the SPA only ever issues
	// relative fetches) must keep working too.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/write", strings.NewReader(`{"key":"x","value":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("same-origin status = %d, want 200", res2.StatusCode)
	}
}

// TestWriteAllowsSameOriginViaSecFetchSiteDespiteMismatchedForwardedHost
// proves that a browser-asserted Sec-Fetch-Site: same-origin bypasses the
// Origin/Host comparison entirely — needed behind the Home Assistant
// Ingress supervisor proxy, which may not set X-Forwarded-Host, or may
// set it to something that doesn't match the browser's Origin.
func TestWriteAllowsSameOriginViaSecFetchSiteDespiteMismatchedForwardedHost(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/write", strings.NewReader(`{"key":"x","value":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	// Both disagree with the request's own Host — simulating a proxy
	// that rewrites/omits X-Forwarded-Host. Sec-Fetch-Site: same-origin
	// is guaranteed by the browser itself and must win over this.
	req.Header.Set("Origin", "https://unrelated.example")
	req.Header.Set("X-Forwarded-Host", "also-unrelated.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Sec-Fetch-Site: same-origin must bypass the Origin/Host check)", res.StatusCode)
	}
}

func TestWriteMissingKey(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()
	res, _ := http.Post(ts.URL+"/api/write", "application/json", strings.NewReader(`{"value":1}`))
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestBasicAuth(t *testing.T) {
	ts := newTestServer(Config{User: "admin", Password: "s3cret"}, &fakeBackend{})
	defer ts.Close()

	// No credentials → 401.
	res, _ := http.Get(ts.URL + "/api/health")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", res.StatusCode)
	}

	// Wrong credentials → 401.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", http.NoBody)
	req.SetBasicAuth("admin", "wrong")
	res2, _ := http.DefaultClient.Do(req)
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-auth status = %d, want 401", res2.StatusCode)
	}

	// Correct credentials → 200.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", http.NoBody)
	req2.SetBasicAuth("admin", "s3cret")
	res3, _ := http.DefaultClient.Do(req2)
	_ = res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("good-auth status = %d, want 200", res3.StatusCode)
	}
}

func TestServesSPA(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()
	res, _ := http.Get(ts.URL + "/")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", res.StatusCode)
	}
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), "mtec2mqtt") {
		t.Error("index.html not served at /")
	}

	// The embedded asset tree must also be reachable, including the
	// localisation bundles served from the i18n subdirectory.
	for _, asset := range []string{"/app.js", "/app.css", "/i18n/en.json", "/i18n/de.json"} {
		r, err := http.Get(ts.URL + asset)
		if err != nil {
			t.Fatalf("GET %s: %v", asset, err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("asset %s status = %d", asset, r.StatusCode)
		}
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := res.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	csp := res.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
	// Must not break Home Assistant Ingress iframe embedding.
	if strings.Contains(csp, "frame-ancestors 'none'") {
		t.Error("CSP must not set frame-ancestors 'none' — breaks HA Ingress iframe embedding")
	}
	if res.Header.Get("X-Frame-Options") == "DENY" {
		t.Error("X-Frame-Options: DENY breaks HA Ingress iframe embedding")
	}

	// The header must not prevent the SPA's own asset tree from loading.
	for _, asset := range []string{"/", "/app.js", "/app.css"} {
		r, err := http.Get(ts.URL + asset)
		if err != nil {
			t.Fatalf("GET %s: %v", asset, err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("asset %s status = %d, want 200 (CSP must not block same-origin assets)", asset, r.StatusCode)
		}
	}
}

// TestWriteTrickledBodyTimesOut proves that a client which sends complete
// headers for POST /api/write but then withholds the JSON body cannot hold
// the handler (and its connection/goroutine) open indefinitely: the
// per-request read deadline aborts the decode with a 400.
func TestWriteTrickledBodyTimesOut(t *testing.T) {
	old := writeBodyTimeout
	writeBodyTimeout = 200 * time.Millisecond
	defer func() { writeBodyTimeout = old }()

	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Complete headers with a Content-Length promising a body that never
	// fully arrives — exactly the slow-client shape the deadline defends
	// against.
	_, err = io.WriteString(conn,
		"POST /api/write HTTP/1.1\r\nHost: t\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"key\"")
	if err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response within 3s — trickled body held the handler: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 after body read deadline", res.StatusCode)
	}
}

// TestRunReturnsBindErrorWithoutLoggingListening proves that Run reports a
// bind failure as a returned error and never logs "web.listening" for a
// socket it never actually bound — logging readiness ahead of a
// successful net.Listen would be a lie.
func TestRunReturnsBindErrorWithoutLoggingListening(t *testing.T) {
	// Occupy a port so the second bind attempt (inside Run) fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	srv := New(Config{Bind: addr, Logger: logger}, &fakeBackend{})
	if err := srv.Run(context.Background()); err == nil {
		t.Fatal("expected a bind error, got nil")
	}
	if strings.Contains(buf.String(), "web.listening") {
		t.Errorf("web.listening logged despite the bind failing: %s", buf.String())
	}
}

// TestWriteDeadlineDisconnectsStalledReaderOnNormalRoute proves that a
// client which issues a normal (non-SSE) GET request and then never
// reads the response cannot pin the handler goroutine forever: once the
// (deliberately shrunk) socket buffers fill, withWriteDeadline's per
// -request write deadline errors the blocked write inside writeJSON's
// Encode call. That failure is only observable via the log (see
// TestWriteJSONLogsEncodeFailure) since the status is already committed
// — its arrival proves the handler returned instead of hanging.
func TestWriteDeadlineDisconnectsStalledReaderOnNormalRoute(t *testing.T) {
	old := writeDeadline
	writeDeadline = 200 * time.Millisecond
	defer func() { writeDeadline = old }()

	// A register catalog large enough that the JSON body overflows the
	// shrunk socket buffers below, so the write actually blocks instead
	// of completing in a single non-blocking syscall.
	regs := make([]state.RegisterInfo, 20000)
	for i := range regs {
		regs[i] = state.RegisterInfo{
			Key: "40000", Name: "Padding register name for bulk transfer", MQTT: "padding_key", Unit: "W",
		}
	}
	fb := &bigRegisterBackend{regs: regs}

	sig := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(&signalWriter{ch: sig}, nil))

	ts := httptest.NewUnstartedServer(New(Config{Logger: logger}, fb).handler)
	ts.Listener = &bufLimitListener{Listener: ts.Listener}
	ts.Start()
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		// Shrink the receive buffer so the advertised window fills fast.
		_ = tc.SetReadBuffer(2048)
	}
	if _, err := conn.Write([]byte("GET /api/registers HTTP/1.1\r\nHost: stalled.test\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-sig:
		// writeJSON logged the encode failure — the deadline fired and
		// the handler returned instead of blocking indefinitely.
	case <-time.After(10 * time.Second):
		t.Fatal("write deadline did not unblock a stalled client on a normal (non-SSE) route")
	}
}

// TestWriteJSONLogsEncodeFailure proves that writeJSON no longer silently
// discards an Encode failure (which can only happen after the status is
// already committed via WriteHeader, so it can't be turned into an error
// response) — it must be logged instead.
func TestWriteJSONLogsEncodeFailure(t *testing.T) {
	sig := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(&signalWriter{ch: sig}, nil))
	s := New(Config{Logger: logger}, &fakeBackend{})

	rec := httptest.NewRecorder()
	// Channels aren't JSON-marshalable, so Encode fails deterministically
	// without needing a real stalled connection.
	s.writeJSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})

	select {
	case <-sig:
	case <-time.After(time.Second):
		t.Error("encode failure was not logged")
	}
}

// TestRunShutdownUnblocksSSE proves that cancelling the run context also
// cancels in-flight request contexts (via BaseContext), so a connected SSE
// client no longer forces srv.Shutdown to burn its full 5s deadline.
func TestRunShutdownUnblocksSSE(t *testing.T) {
	// Reserve a free port, then hand its address to Run.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := New(Config{Bind: addr}, &fakeBackend{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Connect an SSE client (retry until the listener is up) and read the
	// first byte so the handler is provably inside its streaming loop.
	var res *http.Response
	for range 100 {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/events", http.NoBody)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		res, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("connecting SSE client: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	buf := make([]byte, 1)
	if _, err := res.Body.Read(buf); err != nil {
		t.Fatalf("reading first SSE byte: %v", err)
	}

	start := time.Now()
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("Run returned error: %v", runErr)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Run did not return within 4s of cancellation — SSE stream held shutdown")
	}
	if d := time.Since(start); d >= 2*time.Second {
		t.Errorf("shutdown took %v, want prompt exit well under the 5s Shutdown deadline", d)
	}
}

// TestSSEStalledClientUnblocksViaWriteDeadline proves that a client which
// opens /api/events and then never reads the response cannot pin the
// handler goroutine forever: once the socket buffers fill, the per-frame
// write deadline errors the blocked write and the handler exits,
// releasing its Changes subscription (the deferred cancel runs).
func TestSSEStalledClientUnblocksViaWriteDeadline(t *testing.T) {
	old := sseWriteTimeout
	sseWriteTimeout = 200 * time.Millisecond
	defer func() { sseWriteTimeout = old }()

	handlerDone := make(chan struct{})
	fb := &fakeBackend{
		changes:  make(chan struct{}),
		onCancel: func() { close(handlerDone) },
	}
	ts := newTestServer(Config{}, fb)
	defer ts.Close()

	// Raw TCP client: send the request, then never read the response, so
	// the receive window fills and server-side writes eventually block.
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		// Shrink the receive buffer so the window fills fast.
		_ = tc.SetReadBuffer(4096)
	}
	if _, err := conn.Write([]byte("GET /api/events HTTP/1.1\r\nHost: stalled.test\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	// Pump change notifications so the handler keeps producing frames
	// until the socket buffers are full and the next write blocks.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case fb.changes <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()

	select {
	case <-handlerDone:
		// Handler exited and released its subscription — the write
		// deadline reaped the stalled stream.
	case <-time.After(15 * time.Second):
		t.Fatal("SSE handler still running: write deadline did not unblock the stalled client's stream")
	}
}

func TestSSEStreamsInitialFrame(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", http.NoBody)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// Read until the first complete SSE data line arrives.
	sc := bufio.NewScanner(res.Body)
	var gotEvent, gotData bool
	for sc.Scan() {
		line := sc.Text()
		if line == "event: update" {
			gotEvent = true
		}
		if strings.HasPrefix(line, "data: ") {
			gotData = true
			if !strings.Contains(line, `"health"`) {
				t.Errorf("data frame missing health: %q", line)
			}
			break
		}
	}
	if !gotEvent || !gotData {
		t.Errorf("incomplete SSE frame: event=%v data=%v", gotEvent, gotData)
	}
}
