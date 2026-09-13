// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// The golden files in testdata/ pin every byte this bridge publishes to
// Home Assistant's discovery tree, built by the real builder over the real
// registers.yaml with the real default topic roots. They exist so the
// ADR 0070 migration (see notes/adr0070-phase6-measurement.md) can be
// carried out as a sequence of steps whose payload effect is *visible*:
// before these files, one payload of a hundred was asserted anywhere, and
// `device.identifiers` — the device-registry key, which has no migration
// path — was asserted by nothing at all.
//
// Regenerate with:
//
//	go test ./internal/hass -run TestDiscoveryGolden -update-discovery-golden
//
// and read the diff. A non-empty diff is a change to what an installed
// base receives; it is never "just a test update".
//
// # Pinned defects
//
// Several rows below are pinned as *defects*, so that the later steps that
// fix them produce a diff a reviewer can see. Naming them here so nobody
// "fixes" the golden file instead of the code:
//
//   - F1/F2 — fixed in step 2b, and the pins now assert the fix:
//     TestEveryPayloadDeclaresBridgeAvailability (every payload declares
//     the bridge status topic) and
//     TestAvailabilityTopicIsOutsideTheDiscoveryTree (it is not in Home
//     Assistant's own tree). The first of the two was
//     TestGoldenPinsTheAvailabilityDefect, which asserted the absence.
//   - F3/F8 — fixed in step 2a. Pinned in the coordinator package
//     (TestPublishQoSAndRetain, TestNilValueIsNotPublished), where the
//     publish happens.
//   - F7 — the two synthetic switches are `enabled_by_default: true` while
//     every real control is `false`. Still pinned as an inconsistency by
//     TestGoldenPinsTheEnabledByDefaultInconsistency, deliberately: it is
//     not a defect, and changing a default toggles entities on in
//     installed fleets.
//   - F10 — the state-topic tree is wider than the discovery tree. Still
//     pinned as a defect in the coordinator package's topic golden; the
//     filter is a later step.
//
// # The duplicated unique_ids — settled, not open
//
// Nine `unique_id`s are published twice, under two platforms each (the
// number/select/switch entities also get a read-only sensor view). That is
// legal in the per-entity discovery form, because Home Assistant keys the
// registry on (domain, integration, unique_id).
//
// Whether a *device bundle* may carry them is SETTLED, and no live Home
// Assistant session is outstanding for it. go-hamqtt v0.32.0 narrowed
// discovery.Validate's duplicate check to key on (platform, unique_id) —
// Home Assistant's own (domain, platform, unique_id) registry index —
// after step 3 of this migration pinned v0.31.0's refusal and the bump
// turned that pin red. The step 3b live-HA gate is cancelled;
// notes/adr0070-phase6-steps45-results.md §1 records it, and
// NewEntities' doc comment in hamqtt.go carries the same statement.
// TestRenderedBundleAcceptsTheDuplicatedUniqueIDs is the bundle-side
// assertion; TestGoldenPinsTheDuplicateUniqueIDs below pins the
// duplication in the per-entity form and names the nine.

var updateDiscoveryGolden = flag.Bool("update-discovery-golden", false,
	"rewrite internal/hass/testdata/*.json from the current builder output")

// Fixed device identity for the golden. The serial, firmware and
// equipment string come from the STATIC register read at runtime; these
// are plausible stand-ins, chosen once and never changed, because every
// topic and every `device` block is keyed on them.
const (
	goldenSerial    = "MT1234567890"
	goldenFirmware  = "1.2.3"
	goldenEquipment = "EB-10kW"
)

// goldenEntry is one pinned discovery publication: the retained config
// topic, and the payload stored *decoded* so a reviewer reads JSON in the
// diff rather than an escaped blob. Comparison is on a canonical
// re-encoding of both sides, never on the file's own bytes — a test that
// compared the file against itself would pass every mutation, since the
// file is regenerated from the production code it is meant to guard.
type goldenEntry struct {
	Topic   string         `json:"topic"`
	Payload map[string]any `json:"payload"`
}

// goldenIdentity is the identity plane on its own: the two strings that
// cannot be changed without orphaning an installed base's entities. Kept
// in a separate file from the payloads so an identity change and a payload
// change produce two different diffs.
type goldenIdentity struct {
	ConfigTopic     string `json:"config_topic"`
	UniqueID        string `json:"unique_id"`
	DefaultEntityID string `json:"default_entity_id"`
}

// realDiscovery builds the shipped discovery plane: the real catalog, the
// real default topic roots ("homeassistant" / "MTEC" — the values an
// untouched deployment runs with), the two built-in synthetic switches and
// no operator device name.
func realDiscovery(t *testing.T, lang string) *Discovery {
	t.Helper()
	m, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	// The on-values are the coordinator's write payloads and never reach a
	// discovery payload; any value builds the same entities.
	d := New("homeassistant", "MTEC", m, lang, DefaultVirtualSwitches(50, 50), "")
	d.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	return d
}

// goldenEntries decodes the built entries into the pinned shape, sorted by
// topic so the file is stable and the diff is readable.
func goldenEntries(t *testing.T, d *Discovery) []goldenEntry {
	t.Helper()
	out := make([]goldenEntry, 0, len(d.Entries()))
	for _, e := range d.Entries() {
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("%s: payload is not valid json: %v", e.ConfigTopic, err)
		}
		out = append(out, goldenEntry{Topic: e.ConfigTopic, Payload: payload})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

// canonical re-encodes v with sorted keys and no indentation. Both sides of
// every comparison go through it, so formatting can never hide a value
// change and a reformatted golden file can never look like a payload change.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	bs, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	return bs
}

// readOrUpdateGolden compares got against testdata/<name>, or rewrites the
// file when -update-discovery-golden is set.
func readOrUpdateGolden(t *testing.T, name string, got any) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	pretty, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	pretty = append(pretty, '\n')
	if *updateDiscoveryGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, pretty, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("wrote %s", path)
		return pretty
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test fixture path
	if err != nil {
		t.Fatalf("read %s (regenerate with -update-discovery-golden): %v", name, err)
	}
	return want
}

// TestDiscoveryGolden pins all 100 discovery payloads, in both shipped
// languages, against the real catalog. `name` and `options` are localised;
// every identity string is not, which is the point of pinning both.
func TestDiscoveryGolden(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			d := realDiscovery(t, lang)
			got := goldenEntries(t, d)

			if len(got) != 100 {
				t.Errorf("entry count = %d, want 100 (89 annotated registers + 9 dual emissions + 2 synthetic switches)", len(got))
			}

			raw := readOrUpdateGolden(t, "discovery_"+lang+".json", got)
			var want []goldenEntry
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("golden discovery_%s.json is not valid json: %v", lang, err)
			}
			compareEntries(t, want, got)
		})
	}
}

// compareEntries diffs the two sides per topic, on canonical encodings, so
// a failure names the entity and the payload rather than dumping 33 KB.
func compareEntries(t *testing.T, want, got []goldenEntry) {
	t.Helper()
	wantByTopic := map[string]goldenEntry{}
	for _, e := range want {
		wantByTopic[e.Topic] = e
	}
	gotByTopic := map[string]goldenEntry{}
	for _, e := range got {
		if _, dup := gotByTopic[e.Topic]; dup {
			t.Errorf("duplicate config topic emitted: %s", e.Topic)
		}
		gotByTopic[e.Topic] = e
	}
	for topic, w := range wantByTopic {
		g, ok := gotByTopic[topic]
		if !ok {
			t.Errorf("config topic no longer published: %s", topic)
			continue
		}
		if wb, gb := canonical(t, w.Payload), canonical(t, g.Payload); !bytes.Equal(wb, gb) {
			t.Errorf("%s payload changed:\n golden: %s\n  built: %s", topic, wb, gb)
		}
	}
	for topic := range gotByTopic {
		if _, ok := wantByTopic[topic]; !ok {
			t.Errorf("new config topic not in golden: %s", topic)
		}
	}
}

// TestDiscoveryIdentityGolden pins the 100 (unique_id, default_entity_id)
// pairs on their own, and proves they are language-independent by
// asserting the *same* file for both languages: a German deployment must
// address the same entities as an English one.
func TestDiscoveryIdentityGolden(t *testing.T) {
	build := func(lang string) []goldenIdentity {
		out := make([]goldenIdentity, 0, 100)
		for _, e := range goldenEntries(t, realDiscovery(t, lang)) {
			uid, _ := e.Payload["unique_id"].(string)
			eid, _ := e.Payload["default_entity_id"].(string)
			if uid == "" || eid == "" {
				t.Fatalf("%s: unique_id=%q default_entity_id=%q, both must be present", e.Topic, uid, eid)
			}
			out = append(out, goldenIdentity{ConfigTopic: e.Topic, UniqueID: uid, DefaultEntityID: eid})
		}
		return out
	}

	en := build("en")
	raw := readOrUpdateGolden(t, "identity.json", en)
	var want []goldenIdentity
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("golden identity.json is not valid json: %v", err)
	}
	if !bytes.Equal(canonical(t, want), canonical(t, en)) {
		t.Errorf("entity identity changed — this orphans an installed base's entities:\n golden: %s\n  built: %s",
			canonical(t, want), canonical(t, en))
	}
	if de := build("de"); !bytes.Equal(canonical(t, en), canonical(t, de)) {
		t.Errorf("identity differs between languages:\n en: %s\n de: %s", canonical(t, en), canonical(t, de))
	}
}

// TestGoldenPinsTheDeviceBlock pins the device registry key itself. Before
// this test `device.identifiers` — the bare serial, with no namespace
// prefix — was asserted nowhere, and a library default that namespaces it
// would have re-keyed the device with nothing failing.
func TestGoldenPinsTheDeviceBlock(t *testing.T) {
	want := map[string]any{
		"identifiers":   []any{goldenSerial},
		"manufacturer":  "M-TEC",
		"model":         "Energy-Butler",
		"model_id":      goldenEquipment,
		"name":          "MTEC EnergyButler",
		"serial_number": goldenSerial,
		"sw_version":    goldenFirmware,
	}
	wantBytes := canonical(t, want)
	for _, lang := range []string{"en", "de"} {
		entries := goldenEntries(t, realDiscovery(t, lang))
		for _, e := range entries {
			dev, ok := e.Payload["device"].(map[string]any)
			if !ok {
				t.Fatalf("%s (%s): no device block", e.Topic, lang)
			}
			if got := canonical(t, dev); !bytes.Equal(got, wantBytes) {
				t.Fatalf("%s (%s): device block changed:\n golden: %s\n  built: %s", e.Topic, lang, wantBytes, got)
			}
		}
	}
}

// TestGoldenPinsTheDuplicateUniqueIDs pins the nine unique_ids that are
// published twice under two platforms each. This is current behaviour, not
// an endorsement: see the package comment above.
//
// That a device bundle may carry them is settled — go-hamqtt v0.32.0, see
// TestRenderedBundleAcceptsTheDuplicatedUniqueIDs — so this test pins the
// per-entity form only, and no live Home Assistant session is outstanding
// for it.
func TestGoldenPinsTheDuplicateUniqueIDs(t *testing.T) {
	want := map[string][]string{
		"MTEC_charge_limit":        {"number", "sensor"},
		"MTEC_discharge_limit":     {"number", "sensor"},
		"MTEC_grid_inject_limit":   {"number", "sensor"},
		"MTEC_off_grid_soc_limit":  {"number", "sensor"},
		"MTEC_on_grid_soc_limit":   {"number", "sensor"},
		"MTEC_grid_inject_switch":  {"binary_sensor", "switch"},
		"MTEC_off_grid_soc_switch": {"binary_sensor", "switch"},
		"MTEC_on_grid_soc_switch":  {"binary_sensor", "switch"},
		"MTEC_mode":                {"select", "sensor"},
	}

	entries := goldenEntries(t, realDiscovery(t, "en"))
	platforms := map[string][]string{}
	for _, e := range entries {
		uid, _ := e.Payload["unique_id"].(string)
		// topic is "<base>/<platform>/<unique_id>/config".
		parts := splitTopic(e.Topic)
		if len(parts) != 4 {
			t.Fatalf("unexpected config topic shape: %s", e.Topic)
		}
		platforms[uid] = append(platforms[uid], parts[1])
	}
	if len(platforms) != 91 {
		t.Errorf("distinct unique_ids = %d, want 91 across 100 config topics", len(platforms))
	}
	got := map[string][]string{}
	for uid, ps := range platforms {
		if len(ps) > 1 {
			sort.Strings(ps)
			got[uid] = ps
		}
	}
	if !bytes.Equal(canonical(t, want), canonical(t, got)) {
		t.Errorf("the set of doubly-published unique_ids changed:\n golden: %s\n  built: %s",
			canonical(t, want), canonical(t, got))
	}
}

// TestEveryPayloadDeclaresBridgeAvailability is what F1's negative pin
// became. It used to assert that not one of the 100 payloads carried any
// availability source — the defect — and it now asserts the fixed
// contract: every payload declares exactly one, the daemon's own status
// topic, with the two payload words the daemon really writes.
//
// "Every" is the assertion, not "some": availability is attached in
// appendEntry, the single funnel all six platform builders pass through,
// precisely because an entity that forgot it is indistinguishable from a
// healthy one until the daemon dies.
func TestEveryPayloadDeclaresBridgeAvailability(t *testing.T) {
	want := []any{map[string]any{
		"topic":                 "MTEC/bridge/status",
		"payload_available":     "online",
		"payload_not_available": "offline",
	}}
	wantBytes := canonical(t, want)

	for _, lang := range []string{"en", "de"} {
		n := 0
		for _, e := range goldenEntries(t, realDiscovery(t, lang)) {
			n++
			got, ok := e.Payload["availability"]
			if !ok {
				t.Errorf("%s (%s) declares no availability source — a dead daemon "+
					"leaves this entity showing its last value forever (F1)", e.Topic, lang)
				continue
			}
			if gb := canonical(t, got); !bytes.Equal(gb, wantBytes) {
				t.Errorf("%s (%s) availability = %s, want %s", e.Topic, lang, gb, wantBytes)
			}
			if mode := e.Payload["availability_mode"]; mode != "all" {
				t.Errorf("%s (%s) availability_mode = %v, want \"all\"", e.Topic, lang, mode)
			}
			// The flat pre-2024 form and the list form are both read by
			// Home Assistant, and a payload carrying both is a
			// contradiction it resolves silently.
			for _, k := range []string{"availability_topic", "payload_available", "payload_not_available"} {
				if _, has := e.Payload[k]; has {
					t.Errorf("%s (%s) carries both the list and the flat %q form", e.Topic, lang, k)
				}
			}
		}
		if n != 100 {
			t.Errorf("%s: checked %d payloads, want 100", lang, n)
		}
	}
}

// TestAvailabilityTopicIsOutsideTheDiscoveryTree is F2, which F1 hid and
// which hid F1 in turn: the daemon's status topic used to be
// "<hass_base>/status/lwt" — homeassistant/status/lwt by default, one
// level under the topic Home Assistant publishes its own birth message to
// and which this daemon subscribes to. Because nothing referenced it,
// nothing ever surfaced that it was in another integration's tree.
//
// Now that 100 entities point at it, the tree it sits in is load-bearing,
// so it is asserted rather than left to the golden diff: the topic must be
// under this daemon's own publish root and must not be under the discovery
// prefix, whatever either root is configured to.
func TestAvailabilityTopicIsOutsideTheDiscoveryTree(t *testing.T) {
	const hassBase, mqttRoot = "homeassistant", "MTEC"

	if got := BridgeStatusTopic(mqttRoot); got != "MTEC/bridge/status" {
		t.Fatalf("BridgeStatusTopic(%q) = %q, want MTEC/bridge/status", mqttRoot, got)
	}
	m, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := New(hassBase, mqttRoot, m, "en", DefaultVirtualSwitches(50, 50), "")
	d.Initialize(goldenSerial, goldenFirmware, goldenEquipment)

	for _, e := range goldenEntries(t, d) {
		list, _ := e.Payload["availability"].([]any)
		if len(list) != 1 {
			t.Fatalf("%s: availability has %d entries, want 1", e.Topic, len(list))
		}
		entry, _ := list[0].(map[string]any)
		topic, _ := entry["topic"].(string)
		if strings.HasPrefix(topic, hassBase+"/") {
			t.Errorf("%s: availability topic %q is inside Home Assistant's own tree (F2)", e.Topic, topic)
		}
		if !strings.HasPrefix(topic, mqttRoot+"/") {
			t.Errorf("%s: availability topic %q is not under this daemon's publish root", e.Topic, topic)
		}
		if topic != BridgeStatusTopic(mqttRoot) {
			t.Errorf("%s: availability topic %q is not the one the daemon publishes", e.Topic, topic)
		}
	}
}

// TestGoldenPinsTheEnabledByDefaultInconsistency pins F7: every
// catalog-derived control is shipped disabled so a user opts in before
// writing to the inverter, while the two synthetic switches — which write
// to the same registers — are shipped enabled.
func TestGoldenPinsTheEnabledByDefaultInconsistency(t *testing.T) {
	disabled := map[string]bool{}
	for _, e := range goldenEntries(t, realDiscovery(t, "en")) {
		uid, _ := e.Payload["unique_id"].(string)
		enabled, ok := e.Payload["enabled_by_default"].(bool)
		if !ok {
			t.Fatalf("%s: no enabled_by_default", e.Topic)
		}
		if _, isControl := e.Payload["command_topic"]; isControl {
			disabled[uid] = !enabled
		}
	}
	for _, uid := range []string{
		"MTEC_charge_limit", "MTEC_discharge_limit", "MTEC_grid_inject_limit",
		"MTEC_off_grid_soc_limit", "MTEC_on_grid_soc_limit", "MTEC_mode",
		"MTEC_grid_inject_switch", "MTEC_off_grid_soc_switch", "MTEC_on_grid_soc_switch",
	} {
		if !disabled[uid] {
			t.Errorf("%s: enabled_by_default is true, want false (every real control ships disabled)", uid)
		}
	}
	// F7: the synthetic switches break that policy.
	for _, uid := range []string{"MTEC_charge_active", "MTEC_discharge_active"} {
		if disabled[uid] {
			t.Errorf("%s: enabled_by_default is false — F7 is fixed; update this test and the goldens together", uid)
		}
	}
}

// TestGoldenPlatformCensus pins the per-platform entity counts, so a
// re-platformed entity is caught even if its payload happens to round-trip.
func TestGoldenPlatformCensus(t *testing.T) {
	want := map[string]int{
		"sensor":        86,
		"number":        5,
		"switch":        5,
		"binary_sensor": 3,
		"select":        1,
	}
	got := map[string]int{}
	for _, e := range goldenEntries(t, realDiscovery(t, "en")) {
		got[splitTopic(e.Topic)[1]]++
	}
	if !bytes.Equal(canonical(t, want), canonical(t, got)) {
		t.Errorf("platform census changed:\n golden: %s\n  built: %s", canonical(t, want), canonical(t, got))
	}
}

// TestDeviceNameSlugDivergenceRows pins the three DEVICE_NAME inputs on
// which this bridge's slugify and a transliterating slug disagree, wired
// through the real catalog rather than through slugify alone. They are the
// rows the ADR 0070 slug decision turns on:
//
//   - "Küche"   → today "k_che";      a transliterating slug gives "kueche"
//   - "Café"    → today "caf";        a transliterating slug gives "cafe"
//   - "ÜÄÖ"     → today "", which collapses to the *generic* identity and
//     silently discards the operator's configured name from all 100
//     entity ids; a slug with a non-empty fallback would prefix all 100.
//
// This pins today's behaviour. It is not a decision to keep it.
func TestDeviceNameSlugDivergenceRows(t *testing.T) {
	m, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		deviceName string
		wantEntity string
		wantDevice string
	}{
		{"Küche", "sensor.k_che_grid_power", "Küche"},
		{"Café", "sensor.caf_grid_power", "Café"},
		{"ÜÄÖ", "sensor.grid_power", "ÜÄÖ"},
		{"", "sensor.grid_power", "MTEC EnergyButler"},
	}
	for _, tc := range cases {
		t.Run("name="+tc.deviceName, func(t *testing.T) {
			d := New("homeassistant", "MTEC", m, "en", nil, tc.deviceName)
			d.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
			var payload map[string]any
			for _, e := range d.Entries() {
				if e.ConfigTopic == "homeassistant/sensor/MTEC_grid_power/config" {
					if err := json.Unmarshal(e.Payload, &payload); err != nil {
						t.Fatal(err)
					}
				}
			}
			if payload == nil {
				t.Fatal("MTEC_grid_power not published")
			}
			if got := payload["default_entity_id"]; got != tc.wantEntity {
				t.Errorf("default_entity_id = %v, want %q", got, tc.wantEntity)
			}
			dev, _ := payload["device"].(map[string]any)
			if got := dev["name"]; got != tc.wantDevice {
				t.Errorf("device.name = %v, want %q", got, tc.wantDevice)
			}
		})
	}
}

// splitTopic splits an MQTT topic into its levels.
func splitTopic(topic string) []string {
	out := []string{}
	start := 0
	for i := range len(topic) {
		if topic[i] == '/' {
			out = append(out, topic[start:i])
			start = i + 1
		}
	}
	return append(out, topic[start:])
}
