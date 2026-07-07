// SPDX-License-Identifier: LGPL-3.0-or-later
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

// TestLoadValidatesCatalogFields pins the load-time validation of
// length/scale/type: a bad entry must be skipped with a diagnostic
// instead of breaking its whole Modbus cluster (or spamming decode
// errors) at runtime.
func TestLoadValidatesCatalogFields(t *testing.T) {
	const broken = `
"31104":
  name: Fat-fingered length
  length: 1000
  group: day
"31105":
  name: Negative length
  length: -2
  group: day
"31106":
  name: Negative scale
  scale: -10
  group: day
"31107":
  name: Lowercase type
  type: u16
  group: day
"65530":
  name: Length past address space
  length: 8
  group: day
"31110":
  name: Healthy neighbour
  group: day
"pseudo-long":
  name: Pseudo registers skip the length check
  length: 200
  group: day
`
	m, diag, err := parse(strings.NewReader(broken), "test")
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"31110", "pseudo-long"}
	if len(m.All) != len(wantKeys) {
		t.Fatalf("kept registers: got %+v, want keys %v", m.All, wantKeys)
	}
	for i, w := range wantKeys {
		if m.All[i].Key != w {
			t.Errorf("All[%d].Key = %q, want %q", i, m.All[i].Key, w)
		}
	}
	if len(diag) != 5 {
		t.Fatalf("diagnostics: got %d (%v), want 5", len(diag), diag)
	}
	for i, key := range []string{"31104", "31105", "31106", "31107", "65530"} {
		if !strings.Contains(diag[i], key) || !strings.HasPrefix(diag[i], "skip ") {
			t.Errorf("diag[%d] = %q, want skip diagnostic for %q", i, diag[i], key)
		}
	}
}

// TestLoadRejectsMQTTUnsafeTopicSegments: wildcard characters in the
// mqtt/group fields (or the name fallback used as the topic key when
// mqtt is empty) would fail every publish client-side, so such entries
// are skipped. A '/' is legal MQTT and read paths work, so it only
// warns.
func TestLoadRejectsMQTTUnsafeTopicSegments(t *testing.T) {
	const broken = `
"20000":
  name: Wildcard in mqtt
  mqtt: bat+soc
  group: now-base
"20001":
  name: Wildcard in group
  mqtt: ok_key
  group: "now#base"
"20002":
  name: Fallback name + used as topic
  group: now-base
"20003":
  name: Hierarchical mqtt key
  mqtt: battery/soc
  group: now-base
"20004":
  name: Name with / but mqtt set
  mqtt: safe_key
  group: now-base
`
	m, diag, err := parse(strings.NewReader(broken), "test")
	if err != nil {
		t.Fatal(err)
	}
	// 20003 and 20004 are kept; the wildcard entries are skipped.
	if len(m.All) != 2 || m.All[0].Key != "20003" || m.All[1].Key != "20004" {
		t.Fatalf("kept registers: got %+v, want 20003 + 20004", m.All)
	}
	if len(diag) != 4 {
		t.Fatalf("diagnostics: got %d (%v), want 4", len(diag), diag)
	}
	for i, key := range []string{"20000", "20001", "20002"} {
		if !strings.HasPrefix(diag[i], "skip ") || !strings.Contains(diag[i], key) {
			t.Errorf("diag[%d] = %q, want skip diagnostic for %q", i, diag[i], key)
		}
	}
	// The slash case is warn-only: entry kept, diagnostic emitted.
	if strings.HasPrefix(diag[3], "skip ") || !strings.Contains(diag[3], "20003") {
		t.Errorf("diag[3] = %q, want warn-only diagnostic for 20003", diag[3])
	}
}

// TestLoadSkipsDuplicateKeys: yaml.Node decoding does not reject
// duplicate mapping keys, so the loader must — first definition wins
// consistently across All/ByKey/ByAddr (matching Clusterize).
func TestLoadSkipsDuplicateKeys(t *testing.T) {
	const dup = `
"10105":
  name: First definition
  scale: 10
  group: now-base
"10105":
  name: Second definition
  scale: 100
  group: now-base
`
	m, diag, err := parse(strings.NewReader(dup), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.All) != 1 {
		t.Fatalf("registers: got %d, want 1", len(m.All))
	}
	if r := m.ByKey["10105"]; r == nil || r.Scale != 10 {
		t.Errorf("ByKey kept %+v, want the first definition (scale 10)", r)
	}
	if r := m.ByAddr[10105]; r == nil || r.Scale != 10 {
		t.Errorf("ByAddr kept %+v, want the first definition (scale 10)", r)
	}
	if len(diag) != 1 || !strings.Contains(diag[0], "duplicate key") {
		t.Fatalf("expected duplicate-key diagnostic, got %v", diag)
	}
}

// TestLoadSkipsOutOfRangeNumericKeys: an all-digits key that overflows
// uint16 is a mistyped Modbus address, not a pseudo-register — it must
// be skipped with a diagnostic instead of becoming a never-polled
// entity that HA advertises forever.
func TestLoadSkipsOutOfRangeNumericKeys(t *testing.T) {
	const broken = `
"104430":
  name: Typoed address
  group: now-base
"consumption":
  name: Real pseudo register
  group: now-base
`
	m, diag, err := parse(strings.NewReader(broken), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.All) != 1 || m.All[0].Key != "consumption" {
		t.Fatalf("expected only the pseudo register, got %+v", m.All)
	}
	if len(diag) != 1 || !strings.Contains(diag[0], "104430") ||
		!strings.Contains(diag[0], "out of Modbus address range") {
		t.Fatalf("expected out-of-range diagnostic for 104430, got %v", diag)
	}
}

// TestLoadSkipsDuplicateMQTTKeys: two registers sharing an mqtt suffix
// collide on the HA unique_id and the write path — the later entry is
// skipped so a write can never land on the wrong holding register.
func TestLoadSkipsDuplicateMQTTKeys(t *testing.T) {
	const dup = `
"52000":
  name: Original writable register
  writable: true
  mqtt: mode
  group: config
"52001":
  name: Copy-pasted with unchanged mqtt
  writable: true
  mqtt: mode
  group: config
`
	m, diag, err := parse(strings.NewReader(dup), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.All) != 1 || m.All[0].Key != "52000" {
		t.Fatalf("expected only the first register, got %+v", m.All)
	}
	if r := m.FindByMQTT("mode"); r == nil || r.Address != 52000 {
		t.Errorf("FindByMQTT(\"mode\") = %+v, want register 52000", r)
	}
	if len(diag) != 1 || !strings.Contains(diag[0], "52001") ||
		!strings.Contains(diag[0], "duplicate mqtt key") {
		t.Fatalf("expected duplicate-mqtt diagnostic for 52001, got %v", diag)
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
	// The shipped catalog must pass every load-time validation — a
	// diagnostic here means either the catalog or the validator broke.
	if len(diag) != 0 {
		t.Errorf("unexpected diagnostics for shipped catalog: %v", diag)
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
