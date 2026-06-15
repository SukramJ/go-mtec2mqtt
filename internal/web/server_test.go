// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
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

func (f *fakeBackend) Changes() (<-chan struct{}, func()) {
	if f.changes == nil {
		f.changes = make(chan struct{})
	}
	return f.changes, func() {}
}

func newTestServer(cfg Config, b Backend) *httptest.Server {
	return httptest.NewServer(New(cfg, b).handler)
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
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", nil)
	req.SetBasicAuth("admin", "wrong")
	res2, _ := http.DefaultClient.Do(req)
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-auth status = %d, want 401", res2.StatusCode)
	}

	// Correct credentials → 200.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", nil)
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

func TestSSEStreamsInitialFrame(t *testing.T) {
	ts := newTestServer(Config{}, &fakeBackend{})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
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
