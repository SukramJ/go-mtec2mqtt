// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// This file began as the experiment ADR 0070 phase 6 step 3 existed to run:
// does github.com/SukramJ/go-hamqtt reproduce, byte for byte, the discovery
// payloads this bridge published — while publishing nothing? Since 2.0.0
// (mqtt-smarthome 2.0) the library renders the new topics and the status
// object's templates, so the question it asks now is narrower and just as
// strict: does the library change ONLY those keys?
//
// The answer is asserted against internal/hass/testdata/*.json, the
// pre-2.0 pins steps 0-2 established, and never against the parallel
// path's own output.
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

// smartHomeKeys are the payload keys 2.0.0 moved (mqtt-smarthome 2.0,
// openccu-loom ADR 0083): the topics, the availability list, the value and
// command templates that read the status object, and the on/off payloads
// that became plain booleans. Everything else in a payload — every
// identity, name, unit, class, option list and the device block — is
// unchanged, and the tests below hold it to the pinned bytes.
var smartHomeKeys = []string{
	"state_topic", "command_topic", "availability", "availability_mode",
	"value_template", "command_template", "payload_on", "payload_off",
}

// withoutSmartHomeKeys returns a copy of payload without [smartHomeKeys].
func withoutSmartHomeKeys(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	for _, k := range smartHomeKeys {
		delete(out, k)
	}
	return out
}

// assertTheMoveChangesOnlyTheSmartHomeKeys compares one pre-2.0 payload
// (pinned, or the frozen builder's) with the library's 2.0 rendering of the
// same entity: identical outside [smartHomeKeys], and inside them exactly
// the 2.0 values — topics re-pointed from `<root>/<serial>/<group>/<key>/state`
// to `<root>/status/<serial>/<group>/<key>` (the group in its snake_case
// spelling), both availability sources, templates reading `value_json.val`,
// and booleans as true/false.
func assertTheMoveChangesOnlyTheSmartHomeKeys(t *testing.T, cfgTopic string, old, got map[string]any) {
	t.Helper()
	if wb, gb := canonical(t, withoutSmartHomeKeys(old)), canonical(t, withoutSmartHomeKeys(got)); !bytes.Equal(wb, gb) {
		t.Errorf("%s: a key outside the 2.0 move changed\n pre-2.0: %s\n     2.0: %s", cfgTopic, wb, gb)
		return
	}
	newTopic := func(oldTopic, suffix string) string {
		parts := splitTopic(oldTopic) // <root>/<serial>/<group>/<key>/<suffix>
		if len(parts) != 5 || parts[4] != suffix {
			t.Fatalf("%s: %q is not a pre-2.0 %s topic", cfgTopic, oldTopic, suffix)
		}
		group := strings.ReplaceAll(parts[2], "-", "_")
		if suffix == "set" {
			return CommandTopic(parts[0], parts[1], group, parts[3])
		}
		return StateTopic(parts[0], parts[1], group, parts[3])
	}
	if st, ok := old["state_topic"].(string); ok {
		if want := newTopic(st, "state"); got["state_topic"] != want {
			t.Errorf("%s: state_topic %v, want %q", cfgTopic, got["state_topic"], want)
		}
		if vt, _ := got["value_template"].(string); !strings.Contains(vt, "value_json.val") {
			t.Errorf("%s: value_template %q does not read the status object's val", cfgTopic, vt)
		}
	}
	if ct, ok := old["command_topic"].(string); ok {
		if want := newTopic(ct, "set"); got["command_topic"] != want {
			t.Errorf("%s: command_topic %v, want %q", cfgTopic, got["command_topic"], want)
		}
	}
	if _, ok := old["payload_on"]; ok {
		if got["payload_on"] != "true" || got["payload_off"] != "false" {
			t.Errorf("%s: payload_on/off %v/%v, want true/false", cfgTopic, got["payload_on"], got["payload_off"])
		}
	}
	avail, _ := got["availability"].([]any)
	var topics []string
	for _, a := range avail {
		entry, _ := a.(map[string]any)
		topics = append(topics, fmt.Sprint(entry["topic"]))
	}
	root, serial := splitTopic(fmt.Sprint(old["state_topic"]))[0], splitTopic(fmt.Sprint(old["state_topic"]))[1]
	if want := []string{ConnectedTopic(root), OnlineTopic(root, serial)}; !slices.Equal(topics, want) {
		t.Errorf("%s: availability topics %v, want %v", cfgTopic, topics, want)
	}
	if got["availability_mode"] != "all" {
		t.Errorf("%s: availability_mode %v, want all", cfgTopic, got["availability_mode"])
	}
}

// TestLibraryChangesOnlyTheSmartHomeKeysOfThePinnedPayloads holds the
// library's rendering of all 100 entities in both shipped languages to the
// pinned pre-2.0 goldens: byte-equal (canonically re-encoded) outside the
// keys 2.0.0 moved, and exactly the 2.0 values inside them.
func TestLibraryChangesOnlyTheSmartHomeKeysOfThePinnedPayloads(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			pinned := readPinnedEntries(t, "discovery_"+lang+".json")
			got := libraryBodies(t, realDiscovery(t, lang))

			if len(got) != len(pinned) {
				t.Errorf("rendered %d payloads, pinned %d", len(got), len(pinned))
			}
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
				assertTheMoveChangesOnlyTheSmartHomeKeys(t, want.Topic, want.Payload, payload)
			}
			for cfgTopic := range got {
				if !pinnedHasTopic(pinned, cfgTopic) {
					t.Errorf("%s: the library renders a config topic that is not pinned", cfgTopic)
				}
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
// the former. The reference is the frozen pre-2.0 builder, and only the
// keys 2.0.0 moved may differ from it.
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
			if len(got) != len(d.Entries()) {
				t.Errorf("rendered %d payloads, the frozen builder %d", len(got), len(d.Entries()))
			}
			for _, e := range d.Entries() {
				raw, ok := got[e.ConfigTopic]
				if !ok {
					t.Errorf("%s: not rendered by the library", e.ConfigTopic)
					continue
				}
				var old, payload map[string]any
				if err := json.Unmarshal(e.Payload, &old); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				assertTheMoveChangesOnlyTheSmartHomeKeys(t, e.ConfigTopic, old, payload)
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

// TestRenderedBundleAcceptsTheDuplicatedUniqueIDs is the step-3 pin,
// updated to the answer go-hamqtt v0.32.0 gives.
//
// Nine unique_ids are published twice, under two platforms each, because a
// writable register also gets a read-only view. Step 3 measured
// discovery.Validate (v0.31.0) refusing that inside a device bundle, and
// pinned the refusal so the day the library or the catalogue changed would
// be a failing test rather than a surprise. It was the library: v0.32.0
// narrowed the duplicate check to key on (platform, unique_id), mirroring
// Home Assistant's own (domain, platform, unique_id) registry index, and
// the nine are now legal in the bundle exactly as they have always been
// legal in the per-entity form.
//
// That pin did its job: the bump turned it red rather than letting the
// change land unnoticed, and this is the updated expectation rather than a
// deleted assertion. The consequences are recorded in
// notes/adr0070-phase6-step3-results.md §3, which this supersedes: the
// former step 3b live-HA gate is cancelled, and step 6 needs neither of the
// two fallbacks the measurement named — no re-keying of the nine sensor
// views, no dropping them, no catalogue change at all.
//
// Both shipped languages are asserted, not just English: the duplication is
// a property of the catalogue rather than of a rendering, but "we only ever
// validated the language the test happened to pick" is exactly the kind of
// blind spot this programme keeps finding.
func TestRenderedBundleAcceptsTheDuplicatedUniqueIDs(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			d := realDiscovery(t, lang)

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
			// Every duplicate is a (platform, unique_id) pair that is
			// distinct — which is precisely the key v0.32.0 uses. Asserted
			// rather than assumed, because a catalogue that ever emitted the
			// SAME platform twice under one unique_id would still be refused,
			// and that refusal would be correct.
			seen := map[string]bool{}
			for _, comp := range bundle.Components {
				key := string(comp.Platform) + "\x00" + comp.UniqueID
				if seen[key] {
					t.Errorf("two components share (platform=%s, unique_id=%s)", comp.Platform, comp.UniqueID)
				}
				seen[key] = true
			}

			if err := discovery.Validate(bundle); err != nil {
				if verr, ok := errors.AsType[*discovery.ValidationError](err); ok {
					t.Fatalf("discovery.Validate refuses the bundle with %d issues (blocking=%v): %v",
						len(verr.Issues), verr.Blocking(), verr.Issues)
				}
				t.Fatalf("discovery.Validate refuses the bundle: %v", err)
			}
		})
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
// the things the payload comparison cannot reach: the mqtt-smarthome 2.0
// grammar `<name>/<function>/<item…>`, and that Slot.Bucket and
// Slot.Channel are inert for this bridge — the poll group rides in
// Slot.Path and there is no channel level. Ignoring them is the deliberate
// decision (model.Bucket's vocabulary is paramset names, this bridge's
// level is a poll group), so it is asserted rather than left as a blind
// spot.
func TestLayoutRendersThisBridgesTopics(t *testing.T) {
	l := NewLayout("MTEC")
	s := hamodel.Slot{Address: goldenSerial, Path: []string{"now_base", "grid_power"}}

	for _, tc := range []struct{ name, got, want string }{
		{"State", l.State(s), "MTEC/status/MT1234567890/now_base/grid_power"},
		{"Command", l.Command(s), "MTEC/set/MT1234567890/now_base/grid_power"},
		{"Availability", l.Availability(s), "MTEC/status/MT1234567890/online"},
		{"Bridge", l.Bridge(), "MTEC/connected"},
		{"Connected", l.Connected(), "MTEC/connected"},
		{"Info", l.Info(), "MTEC/info"},
		{"Maintenance", l.Maintenance("stats"), "MTEC/maintenance/stats"},
		{"StateTopic", StateTopic("MTEC", goldenSerial, "now_base", "grid_power"), "MTEC/status/MT1234567890/now_base/grid_power"},
		{"CommandTopic", CommandTopic("MTEC", goldenSerial, "config", "mode"), "MTEC/set/MT1234567890/config/mode"},
		{"CommandFilter", CommandFilter("MTEC"), "MTEC/set/+/+/+"},
		{"ConnectedTopic", ConnectedTopic("mtec"), "mtec/connected"},
		{"OnlineTopic", OnlineTopic("mtec", goldenSerial), "mtec/status/MT1234567890/online"},
		{"LegacyStateTopic", LegacyStateTopic("MTEC", goldenSerial, registers.GroupBase, "grid_power"), "MTEC/MT1234567890/now-base/grid_power/state"},
		{"LegacyCommandTopic", LegacyCommandTopic("MTEC", goldenSerial, registers.GroupConfig, "mode"), "MTEC/MT1234567890/config/mode/set"},
		{"LegacyBridgeStatusTopic", LegacyBridgeStatusTopic("MTEC"), "MTEC/bridge/status"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// Bucket and Channel are inert for this bridge, by decision.
	for _, b := range []hamodel.Bucket{
		hamodel.BucketUnset, hamodel.BucketValues, hamodel.BucketMaster,
		hamodel.BucketCalculated, hamodel.BucketCustom,
	} {
		withBucket := s
		withBucket.Bucket = b
		if b != hamodel.BucketUnset && l.State(withBucket) == l.State(s) {
			continue // topic.SmartHome renders a set bucket; this bridge never sets one
		}
		if b == hamodel.BucketUnset && l.State(withBucket) != l.State(s) {
			t.Errorf("BucketUnset changes the state topic: %q", l.State(withBucket))
		}
	}
	for _, e := range NewEntities(realDiscovery(t, "en")) {
		for _, b := range e.Bindings() {
			if b.Slot.Bucket != hamodel.BucketUnset || b.Slot.Channel != "" {
				t.Errorf("%s binds a slot with bucket %v / channel %q; this bridge's level is a poll group",
					e.Key(), b.Slot.Bucket, b.Slot.Channel)
			}
		}
	}

	// A multi-level root is kept verbatim and reported non-conformant; an
	// unusable one renders nothing rather than a wrong topic.
	if ml := NewLayout("home/mtec"); ml.Connected() != "home/mtec/connected" || ml.Conformant() {
		t.Errorf("multi-level layout: connected %q, conformant %v", ml.Connected(), ml.Conformant())
	}
	if bad := NewLayout("mtec/#"); bad.Connected() != "" || bad.State(s) != "" {
		t.Errorf("a wildcard root renders %q / %q, want nothing", bad.Connected(), bad.State(s))
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

// TestLayoutStateTopicEqualsTheConfigsOwnStateTopic closes the one trap
// §5.2 of the phase-6 measurement left open, and it is the reason step 4
// may publish through [StateTopic] at all.
//
// go-hamqtt's rule is blunt: publish through
// publisher.StatePublisher.PublishComponentValue or
// publisher.ComponentStateTopic, NEVER through topic.Layout.State. The
// reason is that a Layout returns a topic for all 32 Home Assistant
// platforms while the renderer projects `state_topic` into a config on
// only 22 of them, so on climate, water_heater, lawn_mower, camera, tag,
// button, device_automation, image, notify and scene the layout hands out
// a plausible-looking topic that no config references — and the entity
// stays `unknown` for the life of the fleet with nothing logged anywhere.
//
// This bridge's poll loop cannot take that route: it publishes by (group,
// key) off a Modbus read, and five of its state topics are read by no
// entity at all (F10), so there is no component to ask. What it can do is
// PROVE the two answers are the same string for every entity it actually
// renders — which is what this asserts, over all 100 components, in both
// languages. The day a platform without a `state_topic` joins this
// catalogue — `button` is already a declared platform of this bridge with
// an empty dispatch case, F6 — this test goes red instead of the fleet
// going quiet.
func TestLayoutStateTopicEqualsTheConfigsOwnStateTopic(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			d := realDiscovery(t, lang)
			ctx := NewRenderContext(d)
			dev := NewDevice(d)

			checked := 0
			for _, e := range NewEntities(d) {
				ent, ok := e.(*Entity)
				if !ok {
					t.Fatalf("%s is not an *Entity", e.Key())
				}
				comp, err := discovery.RenderComponent(ctx, dev, ent, discovery.Origin{})
				if err != nil {
					t.Fatalf("render %s: %v", ent.Key(), err)
				}
				// The config's own answer, read off the rendered component
				// rather than derived — the only string provably equal to
				// what Home Assistant was told to read.
				fromComponent, err := publisher.ComponentStateTopic(comp)
				if err != nil {
					t.Fatalf("%s declares no state_topic (%v) — this bridge's poll loop "+
						"publishes to Layout.State for every register, so such an entity "+
						"would be permanently unknown", ent.Key(), err)
				}
				bind := ent.Binds[0].Slot
				fromLayout := StateTopic(d.mqttTopic, bind.Address, bind.Path[0], bind.Path[1])
				if fromComponent != fromLayout {
					t.Errorf("%s: config says %q, the poll loop publishes to %q",
						ent.Key(), fromComponent, fromLayout)
				}
				checked++
			}
			if checked != 100 {
				t.Errorf("checked %d components, want 100", checked)
			}
		})
	}
}

// TestOwnsConfigTopicIsNarrow pins the sweep's ownership predicate against
// the populations of a shared discovery tree it must decline.
//
// This is the predicate whose width decides what a sweep can destroy.
// openccu-loom's PR #817 is the live example: its retraction prefixes
// turned out to own 100 % of a sibling daemon's configs. Every row below
// is a topic that exists on somebody's broker.
func TestOwnsConfigTopicIsNarrow(t *testing.T) {
	const prefix = "homeassistant"
	cases := []struct {
		topic string
		want  bool
		why   string
	}{
		{"homeassistant/sensor/MTEC_grid_power/config", true, "this bridge's own form"},
		{"homeassistant/switch/MTEC_on_grid_soc_switch/config", true, "a writable control"},
		{"homeassistant/binary_sensor/MTEC_grid_inject_switch/config", true, "the sensor view of one"},
		{"homeassistant/number/MTEC_charge_limit/config", true, "a number control"},
		{"homeassistant/select/MTEC_mode/config", true, "the one select"},

		{"homeassistant/sensor/tasmota_ABC123/config", false, "another integration in the same namespace"},
		{"homeassistant/sensor/zendure_hub_soc/config", false, "a sibling bridge of this family"},
		{"homeassistant/device/MTEC_MT1234567890/config", false, "a device bundle — step 6's shape, not this one's"},
		{"homeassistant/sensor/node/MTEC_grid_power/config", false, "the five-segment node-id form"},
		{"homeassistant/climate/MTEC_thermostat/config", false, "a platform this bridge does not emit"},
		{"homeassistant/light/MTEC_lamp/config", false, "likewise"},
		{"homeassistant/sensor/MTECH_grid_power/config", false, "a namespace that merely starts alike is not this one"},
	}
	for _, tc := range cases {
		parsed, ok := publisher.ParseConfigTopic(prefix, tc.topic)
		if !ok {
			if tc.want {
				t.Errorf("%s: ParseConfigTopic declined a topic this bridge publishes", tc.topic)
			}
			continue
		}
		if got := OwnsConfigTopic(parsed); got != tc.want {
			t.Errorf("OwnsConfigTopic(%s) = %v, want %v (%s)", tc.topic, got, tc.want, tc.why)
		}
	}
}

// TestOwnsConfigTopicCoversEveryPinnedConfigTopic is the other half: the
// predicate must be narrow, and it must still claim every one of the 100
// retained configs this daemon actually publishes. A predicate that
// declined its own fleet would leave every orphan on the broker forever,
// which is the quiet failure — nothing in the log, nothing on the wire.
func TestOwnsConfigTopicCoversEveryPinnedConfigTopic(t *testing.T) {
	d := realDiscovery(t, "en")
	n := 0
	for _, e := range d.Entries() {
		parsed, ok := publisher.ParseConfigTopic("homeassistant", e.ConfigTopic)
		if !ok {
			t.Errorf("%s does not parse as a discovery config topic", e.ConfigTopic)
			continue
		}
		if !OwnsConfigTopic(parsed) {
			t.Errorf("the sweep would not claim this daemon's own %s", e.ConfigTopic)
		}
		n++
	}
	if n != 100 {
		t.Errorf("checked %d config topics, want 100", n)
	}
}

// TestLegacyConfigTopicFormIsTheOneWired ties the constant step 3 recorded
// to the function the code now calls, so the two cannot drift: step 3
// measured the four-segment form against the pins (100 of 100; the
// five-segment default matched 0) and wrote the verdict down as a string.
// A string is not wiring. This asserts that what
// [LegacyConfigTopicForms] hands publisher.Config.LegacyEntityTopics
// renders the same topic [Discovery.configTopic] publishes to — which is
// the identity step 6's retract-then-publish ordering rests on.
func TestLegacyConfigTopicFormIsTheOneWired(t *testing.T) {
	forms := LegacyConfigTopicForms()
	if len(forms) != 1 {
		t.Fatalf("LegacyConfigTopicForms() has %d entries, want exactly one — naming a form "+
			"REPLACES the library default rather than adding to it", len(forms))
	}
	if LegacyConfigTopicForm != "publisher.LegacyTopicByUniqueID" {
		t.Errorf("the recorded form name is %q", LegacyConfigTopicForm)
	}

	d := realDiscovery(t, "en")
	n := 0
	for _, e := range d.Entries() {
		parsed, ok := publisher.ParseConfigTopic("homeassistant", e.ConfigTopic)
		if !ok {
			t.Fatalf("%s does not parse", e.ConfigTopic)
		}
		got := forms[0](publisher.LegacyEntity{
			Prefix:   "homeassistant",
			Platform: parsed.Platform,
			UniqueID: parsed.ObjectID,
		})
		if got != e.ConfigTopic {
			t.Errorf("the wired legacy form renders %q for %q", got, e.ConfigTopic)
		}
		n++
	}
	if n != 100 {
		t.Errorf("checked %d entries, want 100", n)
	}
}
