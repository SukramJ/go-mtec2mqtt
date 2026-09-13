// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// This file is the experiment ADR 0070 phase 6 step 3 exists to run: does
// github.com/SukramJ/go-hamqtt reproduce, byte for byte, the discovery
// payloads this bridge publishes today — while publishing nothing?
//
// The answer is asserted against internal/hass/testdata/*.json, the pins
// steps 0-2 established, and never against the parallel path's own output.
// A test that compared the library against itself would pass every
// mutation. The goldens are NEVER regenerated from here: this file has no
// -update flag and calls nothing that writes.
//
// If a payload ever stops matching, the failure names the topic, the key
// and both sides, and the question it asks is "which side is wrong?" — not
// "which file needs updating?".

// libraryBodies renders the parallel path for one language, keyed by
// config topic.
func libraryBodies(t *testing.T, d *Discovery) map[string][]byte {
	t.Helper()
	got, err := Render(d)
	if err != nil {
		t.Fatalf("render through go-hamqtt: %v", err)
	}
	return got
}

// TestLibraryReproducesThePinnedPayloads is the experiment's headline: all
// 100 payloads in both shipped languages, compared against the pinned
// goldens rather than against the live builder.
//
// 200 comparisons. Each is a canonical re-encoding of both sides, so
// formatting cannot hide a value change and a value change cannot hide
// behind formatting.
func TestLibraryReproducesThePinnedPayloads(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			pinned := readPinnedEntries(t, "discovery_"+lang+".json")
			got := libraryBodies(t, realDiscovery(t, lang))

			if len(got) != len(pinned) {
				t.Errorf("rendered %d payloads, pinned %d", len(got), len(pinned))
			}
			matched := 0
			for _, want := range pinned {
				raw, ok := got[want.Topic]
				if !ok {
					t.Errorf("%s: the library renders no payload for this pinned topic", want.Topic)
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Errorf("%s: library payload is not valid json: %v", want.Topic, err)
					continue
				}
				wb, gb := canonical(t, want.Payload), canonical(t, payload)
				if !bytes.Equal(wb, gb) {
					t.Errorf("%s: the library does not reproduce the pinned payload\n pinned: %s\nlibrary: %s",
						want.Topic, wb, gb)
					continue
				}
				matched++
			}
			for cfgTopic := range got {
				if !pinnedHasTopic(pinned, cfgTopic) {
					t.Errorf("%s: the library renders a config topic that is not pinned", cfgTopic)
				}
			}
			if matched != 100 {
				t.Errorf("byte-equal payloads: %d of 100", matched)
			}
		})
	}
}

// TestLibraryReproducesTheShippedBytesExactly is the stricter half of the
// same question, and it is not redundant.
//
// The test above compares decoded documents, which is the right question
// for "does Home Assistant receive the same configuration". This one
// compares the raw bytes the two paths hand to the MQTT client. Both
// encode a map with encoding/json, so key order is sorted on both sides
// and the bytes are comparable — which makes "byte-for-byte" a literal
// claim rather than a figure of speech.
func TestLibraryReproducesTheShippedBytesExactly(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			d := realDiscovery(t, lang)
			got := libraryBodies(t, d)
			n := 0
			for _, e := range d.Entries() {
				raw, ok := got[e.ConfigTopic]
				if !ok {
					t.Errorf("%s: not rendered by the library", e.ConfigTopic)
					continue
				}
				if !bytes.Equal(raw, e.Payload) {
					t.Errorf("%s: bytes differ\n shipped: %s\n library: %s", e.ConfigTopic, e.Payload, raw)
					continue
				}
				n++
			}
			if n != 100 {
				t.Errorf("byte-identical payloads: %d of 100", n)
			}
		})
	}
}

// TestLibraryReproducesThePinnedIdentity checks the two strings Home
// Assistant has no migration path for, on their own, against the identity
// pin. A payload diff and an identity diff have to be two different
// failures — only one of them is ever allowed to be non-empty.
func TestLibraryReproducesThePinnedIdentity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want []goldenIdentity
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	got := make([]goldenIdentity, 0, len(want))
	for cfgTopic, body := range libraryBodies(t, realDiscovery(t, "en")) {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		uid, _ := payload["unique_id"].(string)
		eid, _ := payload["default_entity_id"].(string)
		got = append(got, goldenIdentity{ConfigTopic: cfgTopic, UniqueID: uid, DefaultEntityID: eid})
	}
	sort.Slice(got, func(i, j int) bool { return got[i].ConfigTopic < got[j].ConfigTopic })
	sort.Slice(want, func(i, j int) bool { return want[i].ConfigTopic < want[j].ConfigTopic })

	if wb, gb := canonical(t, want), canonical(t, got); !bytes.Equal(wb, gb) {
		t.Errorf("the library's entity identity differs from the pinned one\n pinned: %s\nlibrary: %s", wb, gb)
	}
}

// TestLibraryReproducesTheDeviceNameVariants extends the comparison to the
// two inputs the goldens cannot cover, because a golden is one fixture:
// a configured DEVICE_NAME (which folds a slug into all 100 entity-id
// seeds) and the HASS_UNIQUE_ID_INCLUDE_SERIAL opt-in (which rewrites all
// 100 unique_ids).
//
// Without this, [RenderContext.DeviceSlug] and
// [RenderContext.SerialInUniqueID] would be unreached by every assertion
// in this file — mutating either would change nothing any test reads,
// which is the blind spot this programme keeps finding the expensive way.
// The umlaut rows are deliberate: they are where this package's slugify
// and the library's topic.Slug disagree, and the parallel path must use
// the former.
func TestLibraryReproducesTheDeviceNameVariants(t *testing.T) {
	m, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		deviceName   string
		serialScoped bool
	}{
		{"plain", "", false},
		{"device_name_ascii", "MrBurns", false},
		{"device_name_umlaut", "Küche", false},
		{"device_name_slugs_to_nothing", "ÜÄÖ", false},
		{"serial_scoped_unique_ids", "", true},
		{"both", "Küche", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := New("homeassistant", "MTEC", m, "en", DefaultVirtualSwitches(50, 50), tc.deviceName)
			d.IncludeSerialInUniqueIDs(tc.serialScoped)
			d.Initialize(goldenSerial, goldenFirmware, goldenEquipment)

			got := libraryBodies(t, d)
			n := 0
			for _, e := range d.Entries() {
				if !bytes.Equal(got[e.ConfigTopic], e.Payload) {
					t.Errorf("%s: bytes differ\n shipped: %s\n library: %s", e.ConfigTopic, e.Payload, got[e.ConfigTopic])
					continue
				}
				n++
			}
			if n != len(d.Entries()) {
				t.Errorf("byte-identical payloads: %d of %d", n, len(d.Entries()))
			}
		})
	}
}

// TestRenderedBodiesValidateAgainstHomeAssistantSchemas answers a question
// this bridge had never asked of its own output: does Home Assistant's
// discovery schema actually accept these payloads?
//
// Nothing in this repository has ever validated a discovery payload —
// appendEntry marshals a map and publishes it, and Home Assistant discards
// a malformed config in silence, with no error on the wire and no line in
// its log that names the cause. The pilot found the same of its own bridge
// and it passed; that it passed was previously unknown.
//
// All 200 bodies (100 entities x two languages) go through
// discovery.ValidateBody, which is the per-entity-form entry point to the
// same per-component rules discovery.Validate applies.
func TestRenderedBodiesValidateAgainstHomeAssistantSchemas(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			checked := 0
			for cfgTopic, raw := range libraryBodies(t, realDiscovery(t, lang)) {
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatalf("%s: %v", cfgTopic, err)
				}
				platform := hacatalog.Platform(splitTopic(cfgTopic)[1])
				if err := discovery.ValidateBody(platform, body); err != nil {
					t.Errorf("%s: Home Assistant's %s schema rejects this payload: %v", cfgTopic, platform, err)
					continue
				}
				checked++
			}
			if checked != 100 {
				t.Errorf("validated %d of 100 bodies", checked)
			}
		})
	}
}

// TestRenderedBundleIsRefusedForDuplicateUniqueIDs is the finding this
// step was told not to resolve, recorded as an assertion.
//
// Nine unique_ids are published twice, under two platforms each, because a
// writable register also gets a read-only view. That is legal in the
// per-entity form — Home Assistant keys the registry on (domain,
// integration, unique_id) and sensor and number are different domains —
// and the 200 assertions above prove those payloads are unaffected.
//
// Inside a device bundle it is not legal, and the library says so without
// needing a live Home Assistant: discovery.Render accepts the duplication
// silently (it refuses duplicate component KEYS, and these nine differ),
// but discovery.Validate reports all nine as BLOCKING issues and the
// result matches discovery.ErrInvalidBundle — meaning a runtime that
// validates before publishing would withhold the whole device.
//
// The measurement recorded this as unmeasured and expected it to need a
// live HA. It does not: v0.31.0's validator has the check
// (discovery/validate.go, `seenUnique`), which a grep for "UniqueID" in
// that file does not find because the identifiers are spelled `uniqueID`
// and `seenUnique`.
//
// This is NOT resolved here. The catalogue is unchanged, the duplication
// is reproduced deliberately, and the decision between re-keying the nine
// sensor views (an orphaning change, its own release) and dropping them
// belongs to step 6.
func TestRenderedBundleIsRefusedForDuplicateUniqueIDs(t *testing.T) {
	d := realDiscovery(t, "en")

	bundle, err := RenderBundle(d, discovery.Origin{Name: "go-mtec2mqtt", SW: goldenFirmware})
	if err != nil {
		t.Fatalf("discovery.Render refuses the entity set outright: %v", err)
	}
	if len(bundle.Components) != 100 {
		t.Errorf("bundle carries %d components, want 100", len(bundle.Components))
	}

	byUID := map[string][]string{}
	for _, comp := range bundle.Components {
		byUID[comp.UniqueID] = append(byUID[comp.UniqueID], string(comp.Platform))
	}
	if len(byUID) != 91 {
		t.Errorf("distinct unique_ids in the bundle = %d, want 91", len(byUID))
	}
	dup := map[string][]string{}
	for uid, platforms := range byUID {
		if len(platforms) > 1 {
			sort.Strings(platforms)
			dup[uid] = platforms
		}
	}
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
	if wb, gb := canonical(t, want), canonical(t, dup); !bytes.Equal(wb, gb) {
		t.Errorf("the duplicated unique_ids changed:\n want: %s\n  got: %s", wb, gb)
	}

	err = discovery.Validate(bundle)
	if err == nil {
		t.Fatal("discovery.Validate accepts a bundle carrying nine duplicated unique_ids — " +
			"this is new information and step 6's fallback is no longer needed; update this test")
	}
	var verr *discovery.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate returned %T, want *discovery.ValidationError: %v", err, err)
	}
	if !verr.Blocking() {
		t.Errorf("Validate reports the duplication as advisory only; it is expected to block")
	}
	if !errors.Is(err, discovery.ErrInvalidBundle) {
		t.Errorf("Validate's result does not match ErrInvalidBundle, so a runtime would publish it anyway")
	}
	if len(verr.Issues) != len(want) {
		t.Errorf("Validate reports %d issues, want one per duplicated unique_id (%d): %v",
			len(verr.Issues), len(want), verr.Issues)
	}
}

// TestLegacyTopicFormMatchesThePinnedTopics settles which
// publisher.LegacyTopicFunc reproduces this fleet's retained config
// topics. Nothing uses it yet; step 6 does, and getting it wrong is
// silent: the wrong form retracts nothing, the bundle is published while
// the per-entity configs are still retained, and Home Assistant refuses
// that with one "WARNING [mqtt.entity] Received a conflicting MQTT
// discovery message" and no entities.
//
// Verdict: publisher.LegacyTopicByUniqueID, the FOUR-segment form. Every
// pinned topic is "<prefix>/<platform>/<unique_id>/config" with no node-id
// level, so the five-segment publisher.LegacyTopicWithNodeID default — the
// behaviour a consumer gets by saying nothing — matches none of them. See
// [LegacyConfigTopicForm].
func TestLegacyTopicFormMatchesThePinnedTopics(t *testing.T) {
	pinned := readPinnedEntries(t, "discovery_en.json")
	if len(pinned) != 100 {
		t.Fatalf("pinned %d topics, want 100", len(pinned))
	}

	nodeID := discovery.NodeID(NewDevice(realDiscovery(t, "en")))
	byUniqueID, withNodeID := 0, 0
	for _, e := range pinned {
		parts := splitTopic(e.Topic)
		if len(parts) != 4 {
			t.Errorf("%s: %d levels, want 4 — the fleet is not on the four-segment form", e.Topic, len(parts))
			continue
		}
		uid, _ := e.Payload["unique_id"].(string)
		if parts[2] != uid {
			t.Errorf("%s: third level %q is not the payload's unique_id %q", e.Topic, parts[2], uid)
		}
		seed, _ := e.Payload["default_entity_id"].(string)
		legacy := publisher.LegacyEntity{
			Prefix:   parts[0],
			Platform: parts[1],
			NodeID:   nodeID,
			ObjectID: seed,
			UniqueID: uid,
		}
		if publisher.LegacyTopicByUniqueID(legacy) == e.Topic {
			byUniqueID++
		}
		if publisher.LegacyTopicWithNodeID(legacy) == e.Topic {
			withNodeID++
		}
	}
	if byUniqueID != 100 {
		t.Errorf("LegacyTopicByUniqueID reproduces %d of 100 pinned config topics; step 6 must set it", byUniqueID)
	}
	if withNodeID != 0 {
		t.Errorf("LegacyTopicWithNodeID reproduces %d pinned config topics; it was expected to reproduce none", withNodeID)
	}
	if LegacyConfigTopicForm != "publisher.LegacyTopicByUniqueID" {
		t.Errorf("LegacyConfigTopicForm = %q, but the topics say publisher.LegacyTopicByUniqueID", LegacyConfigTopicForm)
	}
}

// TestLayoutRendersThisBridgesTopics pins the Layout directly, including
// the three things the payload comparison cannot reach.
//
// A mutation that flips Slot.Bucket or Slot.Channel changes nothing in any
// rendered payload, because this bridge's Layout ignores both — the poll
// group rides in Slot.Path and there is no channel level. Ignoring them is
// the deliberate decision (model.Bucket's vocabulary is paramset names,
// this bridge's level is a poll group), so it is asserted rather than left
// as a blind spot. The pilot found the same of Slot.Bucket and turned it
// into an explicit assertion; this is the second consumer to record it.
//
// Layout.Availability is likewise unreachable from any payload — every
// entity declares model.BridgeOnly(), so model.LevelDevice never resolves
// — and is asserted for the same reason.
func TestLayoutRendersThisBridgesTopics(t *testing.T) {
	l := Layout{Root: "MTEC"}
	s := hamodel.Slot{Address: goldenSerial, Path: []string{"now-base", "grid_power"}}

	if got, want := l.State(s), "MTEC/MT1234567890/now-base/grid_power/state"; got != want {
		t.Errorf("State = %q, want %q", got, want)
	}
	if got, want := l.Command(s), "MTEC/MT1234567890/now-base/grid_power/set"; got != want {
		t.Errorf("Command = %q, want %q", got, want)
	}
	if got, want := l.Bridge(), BridgeStatusTopic("MTEC"); got != want {
		t.Errorf("Bridge = %q, want %q — the entities would reference a topic nothing publishes", got, want)
	}
	if got, want := l.Availability(s), "MTEC/MT1234567890/availability"; got != want {
		t.Errorf("Availability = %q, want %q", got, want)
	}

	// Bucket and Channel are inert for this bridge, by decision.
	for _, b := range []hamodel.Bucket{
		hamodel.BucketUnset, hamodel.BucketValues, hamodel.BucketMaster,
		hamodel.BucketCalculated, hamodel.BucketCustom,
	} {
		withBucket := s
		withBucket.Bucket = b
		if got := l.State(withBucket); got != l.State(s) {
			t.Errorf("Bucket %v changes the state topic: %q — this bridge's level is a poll group, not a paramset", b, got)
		}
	}
	withChannel := s
	withChannel.Channel = "3"
	if got := l.State(withChannel); got != l.State(s) {
		t.Errorf("Slot.Channel changes the state topic: %q — this bridge has no channel level", got)
	}

	// The hyphenated poll groups survive: three of the ten contain one, and
	// a Layout that routed them through topic.Slug rather than topic.Safe
	// would keep them too, but a Layout that folded them would silently
	// re-point 40-odd entities at topics nothing publishes.
	for _, group := range []string{"now-base", "now-grid", "now-backup", "now-battery", "now-inverter", "now-pv"} {
		s := hamodel.Slot{Address: goldenSerial, Path: []string{group, "x"}}
		if got, want := l.State(s), "MTEC/"+goldenSerial+"/"+group+"/x/state"; got != want {
			t.Errorf("State = %q, want %q", got, want)
		}
	}
}

// TestRenderedDeviceBlockIsTheBareSerial pins the device-registry key on
// the parallel path. model.Identifier renders "<namespace>:<value>" for a
// non-empty namespace, and this fleet is registered under the bare serial
// with no namespace at all — so an identifier built the library's
// recommended way would re-key the device, leaving the old one behind with
// its area and its name override while the entities moved.
func TestRenderedDeviceBlockIsTheBareSerial(t *testing.T) {
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
	for cfgTopic, raw := range libraryBodies(t, realDiscovery(t, "en")) {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		dev, ok := payload["device"].(map[string]any)
		if !ok {
			t.Fatalf("%s: the library renders no device block", cfgTopic)
		}
		if got := canonical(t, dev); !bytes.Equal(got, wantBytes) {
			t.Fatalf("%s: device block differs\n pinned: %s\nlibrary: %s", cfgTopic, wantBytes, got)
		}
	}
}

// TestRenderedPayloadsCarryNoOrigin records a deliberate omission. Home
// Assistant requires an `origin` block on a device bundle and
// discovery.RenderComponent stamps one whenever it is given a name — so
// the per-entity path must be handed an empty Origin, or all 100 payloads
// grow a key the installed base has never seen. That would be a payload
// diff, and it is not this step's.
func TestRenderedPayloadsCarryNoOrigin(t *testing.T) {
	for cfgTopic, raw := range libraryBodies(t, realDiscovery(t, "en")) {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		if _, has := payload["origin"]; has {
			t.Errorf("%s: carries an origin block; the shipped payloads carry none", cfgTopic)
		}
		if _, has := payload["platform"]; has {
			t.Errorf("%s: carries a platform key; the per-entity form's topic already says so and "+
				"Home Assistant declares the key on no platform", cfgTopic)
		}
	}
}

// --- helpers ----------------------------------------------------------------

// readPinnedEntries reads a golden file. It only ever reads: this file
// carries no -update flag, and regenerating a pin from the code under test
// is how a comparison becomes vacuous.
func readPinnedEntries(t *testing.T, name string) []goldenEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed test fixture path
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	var out []goldenEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("testdata/%s is not valid json: %v", name, err)
	}
	return out
}

func pinnedHasTopic(entries []goldenEntry, cfgTopic string) bool {
	for _, e := range entries {
		if e.Topic == cfgTopic {
			return true
		}
	}
	return false
}
