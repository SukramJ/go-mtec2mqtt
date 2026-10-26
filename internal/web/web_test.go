// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
)

// fakeBackend is a deterministic [Backend].
type fakeBackend struct {
	writeErr         error
	lastKey, lastVal string
}

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func (f *fakeBackend) Snapshot() state.Snapshot {
	return state.Snapshot{
		Serial: "MTEC1", Firmware: "27.52", Equipment: "8kW-3P-3G25",
		Groups: map[string]state.GroupView{
			"now_base": {UpdatedAt: t0, Values: map[string]any{"inverter_status": "OK", "pv": 1200.0}},
			"config":   {UpdatedAt: t0.Add(-time.Minute), Values: map[string]any{"mode": "General mode"}},
			"evcc":     {UpdatedAt: t0, Values: map[string]any{"battery/soc": 64.0}},
		},
	}
}

func (f *fakeBackend) Health() state.Health {
	return state.Health{
		Status: "ok", Version: "3.2.0", ModbusConnected: true, ModbusAddr: "10.0.0.5:5743",
		MQTTServer: "broker:1883", MQTTTopic: "mtec", Serial: "MTEC1", Firmware: "27.52", StartedAt: t0,
	}
}

func (f *fakeBackend) Config() state.ConfigView {
	return state.ConfigView{ModbusIP: "10.0.0.5", ModbusPort: 5743, ModbusSlave: 252, MQTTServer: "broker", MQTTPort: 1883, RefreshNow: 10}
}

func (f *fakeBackend) Registers() []state.RegisterInfo {
	return []state.RegisterInfo{
		{Key: "10105", Name: "Inverter status", MQTT: "inverter_status", Group: "now_base", ValueItems: map[int]string{0: "Wait", 2: "OK"}},
		{Key: "11028", Name: "PV power", MQTT: "pv", Group: "now_base", Unit: "W"},
		{
			Key: "50000", Name: "Mode", MQTT: "mode", Group: "config", Writable: true, Component: "select",
			ValueItems: map[int]string{3: "Economic mode", 0: "General mode", 1: "UPS mode"},
		},
		{Key: "52502", Name: "Charge limit", MQTT: "charge_limit", Group: "config", Unit: "W", Writable: true, Component: "number"},
		{Key: "charge_active", Name: "Charge active", MQTT: "charge_active", Group: "config", Writable: true, Component: "switch"},
		{Key: "evcc/battery/soc", Name: "Battery state of charge", MQTT: "battery/soc", Group: "evcc", Unit: "%"},
	}
}

func (f *fakeBackend) Write(_ context.Context, key, value string) error {
	f.lastKey, f.lastVal = key, value
	return f.writeErr
}

func (f *fakeBackend) Changes() (events <-chan struct{}, cancel func()) {
	return make(chan struct{}), func() {}
}

// The inverter is one device; values are labelled from the catalog, the
// evcc values by their "role/key", and every group keeps its own age.
func TestSnapshotMapping(t *testing.T) {
	snap := (&adapter{b: &fakeBackend{}}).Snapshot()
	if len(snap.Devices) != 1 {
		t.Fatalf("devices = %d", len(snap.Devices))
	}
	d := snap.Devices[0]
	if d.ID != "MTEC1" || d.Title != "M-TEC 8kW-3P-3G25" || d.Subtitle != "SN MTEC1  ·  FW 27.52" || !d.UpdatedAt.Equal(t0) {
		t.Errorf("device = %+v", d)
	}
	if !d.GroupUpdatedAt["config"].Equal(t0.Add(-time.Minute)) {
		t.Errorf("config age = %v", d.GroupUpdatedAt["config"])
	}
	byKey := map[string]bridgeui.Entry{}
	for _, e := range d.Entries {
		byKey[e.Key] = e
	}
	if e := byKey["pv"]; e.Name != "PV power" || e.Unit != "W" || e.Group != "now_base" {
		t.Errorf("pv = %+v", e)
	}
	if e := byKey["inverter_status"]; e.Tone != "ok" {
		t.Errorf("status tone = %q", e.Tone)
	}
	if e := byKey["battery/soc"]; e.Name != "Battery state of charge" || e.Unit != "%" {
		t.Errorf("evcc soc = %+v", e)
	}
}

func TestValueTone(t *testing.T) {
	enum := state.RegisterInfo{ValueItems: map[int]string{0: "Fault"}}
	for _, c := range []struct {
		v    any
		reg  state.RegisterInfo
		want string
	}{
		{"OK", enum, "ok"}, {"Fault", enum, "warn"}, {"Unknown", enum, ""}, {"text", state.RegisterInfo{}, ""}, {1.0, enum, ""},
	} {
		if got := valueTone(c.v, c.reg); got != c.want {
			t.Errorf("valueTone(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// Writable registers become items: a value map a select in code order, a
// switch a switch, anything else a number.
func TestItems(t *testing.T) {
	items := (&adapter{b: &fakeBackend{}}).Items()
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	mode := items[0]
	if mode.Key != "mode" || mode.Component != "select" || len(mode.Options) != 3 ||
		mode.Options[0].Value != "General mode" || mode.Options[2].Label != "Economic mode" {
		t.Errorf("mode = %+v", mode)
	}
	if items[1].Component != "number" || items[1].Unit != "W" || items[2].Component != "switch" {
		t.Errorf("items = %+v", items)
	}
}

func TestWriteErrorClasses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want error
	}{
		{modbus.ErrUnknownRegister, bridgeui.ErrNotWritable},
		{modbus.ErrNotWritable, bridgeui.ErrNotWritable},
		{modbus.ErrValueParse, bridgeui.ErrInvalidValue},
		{modbus.ErrPseudoUnsupported, bridgeui.ErrInvalidValue},
		{modbus.ErrNotConnected, bridgeui.ErrNotReady},
	} {
		err := (&adapter{b: &fakeBackend{writeErr: c.err}}).Write(t.Context(), "", "mode", "x")
		if !errors.Is(err, c.want) || !errors.Is(err, c.err) {
			t.Errorf("%v → %v, want %v", c.err, err, c.want)
		}
	}
	other := errors.New("framing")
	if err := (&adapter{b: &fakeBackend{writeErr: other}}).Write(t.Context(), "", "mode", "x"); !errors.Is(err, other) {
		t.Errorf("transport error = %v", err)
	}
}

// Every tile label and section title is in both bundles, so none shows as
// a raw key; the overrides of go-bridgeui's built-ins are kept.
func TestLabelsTranslated(t *testing.T) {
	tr, err := bridgeui.LoadTranslations(i18nFS, "i18n")
	if err != nil {
		t.Fatal(err)
	}
	a := &adapter{b: &fakeBackend{}}
	var keys []string
	for _, tl := range append(a.Health().Tiles, a.Config()...) {
		keys = append(keys, tl.Label)
	}
	for _, s := range layout.Sections {
		keys = append(keys, s.Title, s.Nav)
		for _, g := range s.Groups {
			keys = append(keys, "group."+g)
		}
	}
	for _, g := range layout.SystemGroups {
		keys = append(keys, "group."+g)
	}
	keys = append(keys, "status.degraded", "control.hint")
	for _, lang := range []string{"en", "de"} {
		for _, k := range keys {
			if _, ok := tr[lang][k]; !ok {
				t.Errorf("%s.json lacks %q", lang, k)
			}
		}
	}
}

// End to end through the go-bridgeui server: the page, the bundle with
// this bridge's labels, a write without a device.
func TestServer(t *testing.T) {
	fb := &fakeBackend{}
	ts := httptest.NewServer(New(Config{Language: "de"}, fb).Handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/i18n/de.json")
	if err != nil {
		t.Fatal(err)
	}
	var de map[string]string
	if err := json.NewDecoder(res.Body).Decode(&de); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if de["status.degraded"] != "Modbus getrennt" || de["btn.set"] != "Setzen" {
		t.Errorf("bundle: degraded=%q set=%q", de["status.degraded"], de["btn.set"])
	}

	res, err = http.Post(ts.URL+"/api/write", "application/json", strings.NewReader(`{"key":"charge_limit","value":3000}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || fb.lastKey != "charge_limit" || fb.lastVal != "3000" {
		t.Errorf("write: %d %q=%q", res.StatusCode, fb.lastKey, fb.lastVal)
	}

	res, err = http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var h bridgeui.Health
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.UptimeSeconds <= 0 || len(h.Tiles) != 6 || h.Tiles[0].Text != "val.connected" {
		t.Errorf("health = %+v", h)
	}
}
