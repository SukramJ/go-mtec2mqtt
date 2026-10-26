// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// components renders d's document and returns its components as decoded
// JSON, keyed by component key ("<platform>.<mqtt key>").
func components(t *testing.T, d *Discovery) map[string]map[string]any {
	t.Helper()
	b, err := RenderBundle(d, BundleOrigin(goldenOriginSW))
	if err != nil {
		t.Fatalf("RenderBundle: %v", err)
	}
	out := map[string]map[string]any{}
	for k, v := range bundleBody(t, b)["components"].(map[string]any) {
		out[k] = v.(map[string]any)
	}
	return out
}

func component(t *testing.T, comps map[string]map[string]any, key string) map[string]any {
	t.Helper()
	c, ok := comps[key]
	if !ok {
		t.Fatalf("no component %s", key)
	}
	return c
}

func TestDiscoveryIsInitialized(t *testing.T) {
	d := New("MTEC", realCatalog(t), "en", nil, "")
	if d.IsInitialized() {
		t.Fatal("initialized before Initialize")
	}
	d.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	if !d.IsInitialized() {
		t.Fatal("not initialized after Initialize")
	}
}

// A writable register yields its control and a read-only view; a switch
// also a binary sensor. Only the controls carry a command topic.
func TestControlsAndTheirReadOnlyViews(t *testing.T) {
	comps := components(t, realDiscovery(t, "en"))
	for _, key := range []string{"number.charge_limit", "select.mode", "switch.grid_inject_switch"} {
		if _, ok := component(t, comps, key)["command_topic"].(string); !ok {
			t.Errorf("%s has no command_topic", key)
		}
	}
	for _, key := range []string{"sensor.charge_limit", "sensor.mode", "binary_sensor.grid_inject_switch"} {
		if _, ok := component(t, comps, key)["command_topic"]; ok {
			t.Errorf("read-only view %s carries a command_topic", key)
		}
	}
	if got := component(t, comps, "number.charge_limit")["command_topic"]; got != "MTEC/set/MT1234567890/config/charge_limit" {
		t.Errorf("number command_topic = %v", got)
	}
}

// Enum options: the select offers the labels in code order, the enum
// sensor the same plus "Unknown"; in German the labels are German.
func TestEnumOptions(t *testing.T) {
	en := components(t, realDiscovery(t, "en"))
	if got := component(t, en, "select.mode")["options"]; !equalStrings(got, "General mode", "Economic mode", "UPS mode", "Off grid") {
		t.Errorf("select options = %v", got)
	}
	if got := component(t, en, "sensor.mode")["options"]; !equalStrings(got, "General mode", "Economic mode", "UPS mode", "Off grid", "Unknown") {
		t.Errorf("enum sensor options = %v", got)
	}
	if _, ok := component(t, en, "sensor.grid_power")["options"]; ok {
		t.Error("a non-enum sensor carries options")
	}
	de := components(t, realDiscovery(t, "de"))
	if got := component(t, de, "select.mode")["options"]; !equalStrings(got, "Allgemeiner Modus", "Sparmodus", "USV-Modus", "Inselbetrieb") {
		t.Errorf("German select options = %v", got)
	}
	// The wire carries the English token; the German select translates it.
	if tmpl, _ := component(t, de, "select.mode")["command_template"].(string); !strings.Contains(tmpl, "'Sparmodus': 'Economic mode'") {
		t.Errorf("German select command_template does not map labels to tokens: %q", tmpl)
	}
}

// Names follow LANGUAGE; entity ids and unique_ids do not.
func TestEntityIDSeedsStayEnglishUnderTranslation(t *testing.T) {
	en := components(t, realDiscovery(t, "en"))
	de := components(t, realDiscovery(t, "de"))
	if got := component(t, de, "sensor.grid_power")["name"]; got != "Netzleistung" {
		t.Errorf("German name = %v", got)
	}
	if got := component(t, en, "sensor.grid_power")["name"]; got != "Grid power" {
		t.Errorf("English name = %v", got)
	}
	for key, c := range en {
		for _, field := range []string{"unique_id", "default_entity_id"} {
			if c[field] != de[key][field] {
				t.Errorf("%s: %s differs by language: %v vs %v", key, field, c[field], de[key][field])
			}
		}
	}
	if got := component(t, en, "select.mode")["default_entity_id"]; got != "select.inverter_operation_mode" {
		t.Errorf("entity id seed = %v, want the slugged English name", got)
	}
}

// The synthetic switches: localised name, enabled by default (unlike the
// real controls), and a command topic under their group.
func TestVirtualSwitchEntities(t *testing.T) {
	en := components(t, realDiscovery(t, "en"))
	de := components(t, realDiscovery(t, "de"))
	for _, key := range []string{"charge_active", "discharge_active"} {
		c := component(t, en, "switch."+key)
		if c["unique_id"] != "MTEC_"+key || c["enabled_by_default"] != true {
			t.Errorf("%s: unique_id %v enabled %v", key, c["unique_id"], c["enabled_by_default"])
		}
		if c["command_topic"] != "MTEC/set/MT1234567890/config/"+key {
			t.Errorf("%s: command_topic %v", key, c["command_topic"])
		}
	}
	if got := component(t, de, "switch.charge_active")["name"]; got != "Laden aktiv" {
		t.Errorf("German name = %v", got)
	}
	if got := component(t, en, "number.charge_limit")["enabled_by_default"]; got != false {
		t.Errorf("a real control is enabled by default: %v", got)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Grid power phase A": "grid_power_phase_a",
		"  MrBurns  ":        "mrburns",
		"Wohnzimmer-Süd":     "wohnzimmer_s_d",
		"PV-1 (W)":           "pv_1_w",
		"___":                "",
		"":                   "",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// DEVICE_NAME renames the device and seeds the entity ids; unique_ids and
// the node id stay.
func TestDeviceNameRewritesNameAndEntityIDsOnly(t *testing.T) {
	plain := components(t, realDiscovery(t, "en"))
	named := New("MTEC", realCatalog(t), "en", DefaultVirtualSwitches(50, 50), " Mr Burns ")
	named.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	got := components(t, named)
	c := component(t, got, "sensor.grid_power")
	if c["default_entity_id"] != "sensor.mr_burns_grid_power" {
		t.Errorf("default_entity_id = %v", c["default_entity_id"])
	}
	for key, p := range plain {
		if p["unique_id"] != got[key]["unique_id"] {
			t.Errorf("%s: DEVICE_NAME changed unique_id %v → %v", key, p["unique_id"], got[key]["unique_id"])
		}
	}
	b, err := RenderBundle(named, BundleOrigin(goldenOriginSW))
	if err != nil {
		t.Fatal(err)
	}
	if dev, _ := bundleBody(t, b)["device"].(map[string]any); dev["name"] != "Mr Burns" {
		t.Errorf("device name = %v", dev["name"])
	}
}

func TestSerialScopedUniqueIDsOptIn(t *testing.T) {
	d := New("MTEC", realCatalog(t), "en", DefaultVirtualSwitches(50, 50), "")
	d.IncludeSerialInUniqueIDs(true)
	d.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	comps := components(t, d)
	if got := component(t, comps, "sensor.grid_power")["unique_id"]; got != "MTEC_MT1234567890_grid_power" {
		t.Errorf("unique_id = %v", got)
	}
	if got := component(t, comps, "switch.charge_active")["unique_id"]; got != "MTEC_MT1234567890_charge_active" {
		t.Errorf("virtual switch unique_id = %v", got)
	}
	if got := component(t, components(t, realDiscovery(t, "en")), "sensor.grid_power")["unique_id"]; got != "MTEC_grid_power" {
		t.Errorf("default unique_id = %v", got)
	}
}

// unsupportedPlatformYAML declares two registers whose hass_component_type
// this builder cannot emit: `button` and a typo.
const unsupportedPlatformYAML = `
"52010":
  name: Restart inverter
  length: 1
  type: U16
  writable: true
  mqtt: restart
  group: config
  hass_component_type: button

"52011":
  name: Typo platform
  length: 1
  type: U16
  writable: true
  mqtt: typo
  group: config
  hass_component_type: sensro

"11000":
  name: Grid power
  length: 2
  type: I32
  unit: W
  mqtt: grid_power
  group: now_base
  hass_device_class: power
`

// A platform this builder cannot emit says so instead of producing nothing,
// and a second Initialize does not accumulate.
func TestUnsupportedComponentTypeIsLoud(t *testing.T) {
	m, _, err := registers.LoadFromString(unsupportedPlatformYAML)
	if err != nil {
		t.Fatal(err)
	}
	d := New("MTEC", m, "en", nil, "")
	d.Initialize("SER", "1.0", "EB")

	comps := components(t, d)
	if len(comps) != 1 || comps["sensor.grid_power"] == nil {
		t.Fatalf("components %v, want the plain sensor alone", slices.Collect(maps.Keys(comps)))
	}
	diags := d.Diagnostics()
	if len(diags) != 2 {
		t.Fatalf("diagnostics = %v, want one per unsupported register", diags)
	}
	joined := strings.Join(diags, "\n")
	for _, want := range []string{"restart", `"button"`, "typo", `"sensro"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostics do not name %s:\n%s", want, joined)
		}
	}
	d.Initialize("SER", "1.0", "EB")
	if got := len(d.Diagnostics()); got != 2 {
		t.Errorf("after a second Initialize: %d diagnostics, want 2", got)
	}
}

// The shipped catalog must be silent, or the warning is noise.
func TestSupportedCatalogProducesNoDiagnostics(t *testing.T) {
	if diags := realDiscovery(t, "en").Diagnostics(); len(diags) != 0 {
		t.Errorf("the shipped catalog produced diagnostics: %v", diags)
	}
}

func equalStrings(v any, want ...string) bool {
	list, ok := v.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i, w := range want {
		if list[i] != w {
			return false
		}
	}
	return true
}
