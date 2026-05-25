// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// catalogYAML covers one register per supported HA platform plus a
// register without any HA hints (must be skipped). Keeping the
// fixture inline so a refactor of registers.yaml does not silently
// break the discovery tests.
const catalogYAML = `
"11000":
  name: Grid power
  length: 2
  type: I32
  unit: W
  mqtt: grid_power
  group: now-base
  hass_device_class: power
  hass_value_template: "{{ value | round(0) }}"
  hass_state_class: measurement

"10112":
  name: Fault flag
  length: 2
  type: BIT
  mqtt: fault_flag
  group: now-base
  hass_device_class: enum
  hass_value_items:
    1: "Mains Lost"
    2: "Grid Voltage Fault"

"52000":
  name: Operation mode
  length: 1
  type: U16
  writable: true
  mqtt: mode
  group: config
  hass_component_type: select
  hass_value_items:
    0: "General"
    1: "Eco"
    2: "Backup"

"52001":
  name: Inject limit
  length: 1
  type: U16
  scale: 10
  writable: true
  mqtt: grid_inject_limit
  group: config
  unit: "W"
  hass_component_type: number
  hass_device_class: power

"52002":
  name: Grid inject enable
  length: 1
  type: U16
  writable: true
  mqtt: grid_inject_switch
  group: config
  hass_component_type: switch
  hass_payload_on: "1"
  hass_payload_off: "0"

"99999":
  name: No HA hints
  length: 1
  type: U16
  mqtt: opaque
  group: config
`

func loadCatalog(t *testing.T) *registers.Map {
	t.Helper()
	m, _, err := registers.LoadFromString(catalogYAML)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newDiscovery(t *testing.T) *Discovery {
	d := New("homeassistant", "MTEC", loadCatalog(t))
	d.Initialize("SN12345", "V27.52.4.0", "8.0K-25A-3P")
	return d
}

// findEntry returns the first entry whose ConfigTopic contains the
// substring needle, or fails the test.
func findEntry(t *testing.T, d *Discovery, needle string) Entry {
	t.Helper()
	for _, e := range d.Entries() {
		if strings.Contains(e.ConfigTopic, needle) {
			return e
		}
	}
	t.Fatalf("no entry whose ConfigTopic contains %q\nentries: %s", needle, dumpTopics(d))
	return Entry{}
}

func dumpTopics(d *Discovery) string {
	parts := make([]string, len(d.Entries()))
	for i, e := range d.Entries() {
		parts[i] = e.ConfigTopic
	}
	return strings.Join(parts, "\n  ")
}

// unmarshalEntry parses the JSON Payload back into a map for
// field-level assertions.
func unmarshalEntry(t *testing.T, e Entry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// --- entry-set assertions --------------------------------------------------

func TestDiscoveryEntryCountAndSkipping(t *testing.T) {
	d := newDiscovery(t)
	// 11000 → sensor (1), 10112 → sensor (1), 52000 → select + sensor
	// (2), 52001 → number + sensor (2), 52002 → switch + binary_sensor
	// (2). Total = 8. The register without any hass_ field must be
	// skipped — that's the silent contract HasHassHints encodes.
	if got, want := len(d.Entries()), 8; got != want {
		t.Fatalf("entries: got %d, want %d\nentries:\n  %s",
			got, want, dumpTopics(d))
	}
}

func TestDiscoveryIsInitialized(t *testing.T) {
	d := New("homeassistant", "MTEC", loadCatalog(t))
	if d.IsInitialized() {
		t.Fatal("must report uninitialised before Initialize")
	}
	d.Initialize("SN", "V1", "model")
	if !d.IsInitialized() {
		t.Fatal("must report initialised after Initialize")
	}
}

func TestDiscoveryRebuildOnReinitialize(t *testing.T) {
	d := newDiscovery(t)
	first := len(d.Entries())
	// Re-init with a different serial — entries must rebuild, not append.
	d.Initialize("OTHER", "V2", "other-model")
	if got := len(d.Entries()); got != first {
		t.Fatalf("re-init changed entry count: %d → %d", first, got)
	}
	// State topic must reflect the new serial.
	e := findEntry(t, d, "MTEC_grid_power")
	p := unmarshalEntry(t, e)
	if !strings.Contains(p["state_topic"].(string), "/OTHER/") {
		t.Errorf("state_topic not updated for new serial: %v", p["state_topic"])
	}
}

// --- per-platform shape ----------------------------------------------------

func TestSensorEntryShape(t *testing.T) {
	d := newDiscovery(t)
	e := findEntry(t, d, "sensor/MTEC_grid_power/config")
	if e.CommandTopic != "" {
		t.Errorf("sensor must have no command topic, got %q", e.CommandTopic)
	}
	p := unmarshalEntry(t, e)
	want := map[string]any{
		"name":                "Grid power",
		"state_topic":         "MTEC/SN12345/now-base/grid_power/state",
		"unique_id":           "MTEC_grid_power",
		"unit_of_measurement": "W",
		"device_class":        "power",
		"value_template":      "{{ value | round(0) }}",
		"state_class":         "measurement",
		"enabled_by_default":  true,
	}
	assertSubset(t, p, want)
	if _, ok := p["device"].(map[string]any); !ok {
		t.Errorf("sensor missing device block")
	}
}

func TestBinarySensorOmitsUnit(t *testing.T) {
	d := newDiscovery(t)
	// 10112 is a BIT register without explicit component_type — defaults
	// to sensor. We still want to assert the platform routing path
	// works for binary_sensor. Use the SWITCH register's binary view
	// instead: switch creates a switch + a binary_sensor.
	e := findEntry(t, d, "binary_sensor/MTEC_grid_inject_switch/config")
	p := unmarshalEntry(t, e)
	if _, has := p["unit_of_measurement"]; has {
		t.Error("binary_sensor must not carry unit_of_measurement")
	}
	if got := p["payload_on"]; got != "1" {
		t.Errorf("payload_on: got %v, want \"1\"", got)
	}
	if got := p["payload_off"]; got != "0" {
		t.Errorf("payload_off: got %v, want \"0\"", got)
	}
}

func TestNumberEntryEmitsCommandTopic(t *testing.T) {
	d := newDiscovery(t)
	e := findEntry(t, d, "number/MTEC_grid_inject_limit/config")
	if e.CommandTopic != "MTEC/SN12345/config/grid_inject_limit/set" {
		t.Errorf("number command topic: got %q", e.CommandTopic)
	}
	p := unmarshalEntry(t, e)
	if p["mode"] != "box" {
		t.Errorf("number mode: got %v, want \"box\"", p["mode"])
	}
	// Numbers must be disabled by default — matches Python so they
	// don't clutter the UI for users who never push values.
	if p["enabled_by_default"] != false {
		t.Errorf("number must default to disabled, got %v", p["enabled_by_default"])
	}
	// A sensor view must also exist for the same register.
	if _, found := findEntryOK(d, "sensor/MTEC_grid_inject_limit/config"); !found {
		t.Error("number must also publish a sensor view")
	}
}

func TestSelectEntryOptionsAreSortedByCode(t *testing.T) {
	d := newDiscovery(t)
	e := findEntry(t, d, "select/MTEC_mode/config")
	p := unmarshalEntry(t, e)
	got, _ := p["options"].([]any)
	want := []string{"General", "Eco", "Backup"}
	if len(got) != len(want) {
		t.Fatalf("options length: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("options[%d]: got %v, want %q", i, got[i], want[i])
		}
	}
}

func TestSwitchAlsoPublishesBinarySensor(t *testing.T) {
	d := newDiscovery(t)
	if _, ok := findEntryOK(d, "switch/MTEC_grid_inject_switch/config"); !ok {
		t.Error("switch entry missing")
	}
	if _, ok := findEntryOK(d, "binary_sensor/MTEC_grid_inject_switch/config"); !ok {
		t.Error("switch did not also publish its binary_sensor view")
	}
}

// --- unregister path -------------------------------------------------------

func TestUnregisterEntriesAreEmptyPayload(t *testing.T) {
	d := newDiscovery(t)
	unreg := d.UnregisterEntries()
	if len(unreg) != len(d.Entries()) {
		t.Fatalf("count mismatch: unreg=%d entries=%d", len(unreg), len(d.Entries()))
	}
	for _, u := range unreg {
		if len(u.Payload) != 0 {
			t.Errorf("unregister payload must be empty for %s, got %d bytes",
				u.ConfigTopic, len(u.Payload))
		}
	}
}

// --- deterministic output --------------------------------------------------

func TestDiscoveryEntryOrderFollowsYAML(t *testing.T) {
	d := newDiscovery(t)
	// First two entries must be the sensors for the first two YAML
	// registers (11000, 10112), in YAML order. The remaining entries'
	// internal ordering varies by component type, but the *first*
	// entry for each register must follow the catalog.
	got := []string{
		d.Entries()[0].ConfigTopic,
		d.Entries()[1].ConfigTopic,
	}
	want := []string{
		"homeassistant/sensor/MTEC_grid_power/config",
		"homeassistant/sensor/MTEC_fault_flag/config",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// --- smoke test against the real catalog -----------------------------------

func TestDiscoveryAgainstRealCatalog(t *testing.T) {
	m, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := New("homeassistant", "MTEC", m)
	d.Initialize("SN12345", "V27.52.4.0", "8.0K-25A-3P")

	if len(d.Entries()) < 50 {
		t.Fatalf("real catalog produced suspiciously few entries: %d", len(d.Entries()))
	}
	// Every entry's JSON must round-trip cleanly — no half-formed
	// payloads slipping through.
	for i, e := range d.Entries() {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("entry %d (%s): invalid json: %v", i, e.ConfigTopic, err)
		}
		if _, has := m["unique_id"]; !has {
			t.Errorf("entry %d (%s) missing unique_id", i, e.ConfigTopic)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func findEntryOK(d *Discovery, needle string) (Entry, bool) {
	for _, e := range d.Entries() {
		if strings.Contains(e.ConfigTopic, needle) {
			return e, true
		}
	}
	return Entry{}, false
}

// assertSubset checks that every key/value in want is present in got
// with the same value. Used so tests pin down meaningful fields
// without having to enumerate the whole payload — additions to
// device{} etc. shouldn't break unrelated tests.
func assertSubset(t *testing.T, got, want map[string]any) {
	t.Helper()
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		if !equalAny(gv, v) {
			t.Errorf("%q: got %v (%T), want %v (%T)", k, gv, gv, v, v)
		}
	}
}

func equalAny(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case int:
		bv, ok := b.(int)
		return ok && av == bv
	}
	return false
}
