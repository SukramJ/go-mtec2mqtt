// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package registers

import (
	"strings"
	"testing"
)

// Minimal YAML covering: numeric register, pseudo-register, defaults,
// hass hints, value-items map. Keeps the test independent from the
// real registers.yaml so refactors of the catalog don't break tests.
const sampleYAML = `
"consumption":
  name: Household consumption
  unit: W
  mqtt: consumption
  group: now-base
  hass_device_class: power
  hass_value_template: "{{ value | round(0) }}"

"10000":
  name: Inverter serial number
  length: 8
  type: STR
  mqtt: serial_no
  group: static

"10105":
  name: Inverter status
  length: 1
  type: U16
  mqtt: inverter_status
  group: now-base
  hass_device_class: enum
  hass_value_items:
    0: "wait for on-grid"
    1: "self-check"
    2: "on-grid"

"52000":
  name: Operation mode
  length: 1
  type: U16
  writable: true
  mqtt: mode
  group: config
`

func TestLoadParsesAllShapes(t *testing.T) {
	m, diag, err := parse(strings.NewReader(sampleYAML), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(diag) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diag)
	}
	if got := len(m.All); got != 4 {
		t.Fatalf("registers: got %d, want 4", got)
	}

	// Order must match YAML — coordinator + HA-discovery rely on it.
	wantKeys := []string{"consumption", "10000", "10105", "52000"}
	for i, w := range wantKeys {
		if m.All[i].Key != w {
			t.Errorf("All[%d].Key = %q, want %q", i, m.All[i].Key, w)
		}
	}

	// Pseudo-register has no address; lookup by address must miss.
	if r := m.ByKey["consumption"]; r == nil || r.Address != 0 || r.IsModbus() {
		t.Errorf("pseudo register misclassified: %+v", r)
	}
	// Modbus register: address parsed and indexed.
	r := m.ByAddr[10000]
	if r == nil || r.Type != DataSTR || r.Length != 8 || !r.IsModbus() {
		t.Errorf("unexpected register 10000: %+v", r)
	}
	// Defaults applied: length and scale fall back to 1.
	mode := m.ByAddr[52000]
	if mode == nil || mode.Length != 1 || mode.Scale != 1 || !mode.Writable {
		t.Errorf("unexpected mode register: %+v", mode)
	}
	// Value items parsed with int keys.
	status := m.ByAddr[10105]
	if got := status.HassValueItems[2]; got != "on-grid" {
		t.Errorf("HassValueItems[2] = %q, want \"on-grid\"", got)
	}
	if !status.HasHassHints() {
		t.Error("status should have hass hints")
	}

	// Groups recorded in first-encounter order.
	wantGroups := []Group{GroupBase, GroupStatic, GroupConfig}
	if len(m.Groups) != len(wantGroups) {
		t.Fatalf("Groups: got %v, want %v", m.Groups, wantGroups)
	}
	for i, g := range wantGroups {
		if m.Groups[i] != g {
			t.Errorf("Groups[%d] = %q, want %q", i, m.Groups[i], g)
		}
	}
}

func TestLoadSkipsEntriesWithoutName(t *testing.T) {
	const broken = `
"10100":
  length: 1
  type: U16
  group: now-base
"10101":
  name: Good register
  group: now-base
`
	m, diag, err := parse(strings.NewReader(broken), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.All) != 1 || m.All[0].Key != "10101" {
		t.Fatalf("expected only the named register, got %+v", m.All)
	}
	if len(diag) != 1 || !strings.Contains(diag[0], "10100") {
		t.Fatalf("expected diagnostic for 10100, got %v", diag)
	}
}

func TestByGroupAndFindByMQTT(t *testing.T) {
	m, _, err := parse(strings.NewReader(sampleYAML), "test")
	if err != nil {
		t.Fatal(err)
	}
	base := m.ByGroup(GroupBase)
	if len(base) != 2 {
		t.Fatalf("now-base group: got %d, want 2 (consumption + status)", len(base))
	}
	if r := m.FindByMQTT("mode"); r == nil || r.Address != 52000 {
		t.Errorf("FindByMQTT(\"mode\") = %+v, want register 52000", r)
	}
	if r := m.FindByMQTT("does-not-exist"); r != nil {
		t.Errorf("unknown lookup must return nil, got %+v", r)
	}
}

// Smoke-test against the real registers.yaml shipped with the repo:
// it must parse cleanly and contain at least the registers the
// coordinator references by address (serial number + grid power).
func TestLoadRealCatalog(t *testing.T) {
	m, diag, err := Load("../../registers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diag {
		t.Logf("diagnostic: %s", d)
	}
	if len(m.All) < 50 {
		t.Fatalf("suspiciously small catalog: %d entries", len(m.All))
	}
	for _, addr := range []uint16{10000 /* serial */, 11000 /* grid power */} {
		if _, ok := m.ByAddr[addr]; !ok {
			t.Errorf("expected register %d to be present in catalog", addr)
		}
	}
}
