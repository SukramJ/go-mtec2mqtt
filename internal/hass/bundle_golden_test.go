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

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mtec2mqtt/internal/version"
)

// The device bundle is a NEW pinned artefact, alongside the 200 per-entity
// payloads in testdata/discovery_{en,de}.json rather than instead of them.
//
// Those 200 are the frozen record of what every installed broker holds
// right now, and this release's whole claim is a statement about the
// relationship between the two: the document carries the same entities,
// byte for byte, minus the `device` block each of them repeated and plus a
// `platform` key and one `origin`. That claim is only checkable while both
// sides exist, so discovery_{en,de}.json is never regenerated here — this
// file's flag is its own (-update-bundle-golden) precisely so
// -update-discovery-golden cannot be reached from a bundle test.
//
// Same discipline as the per-entity pins: topic and payload pinned
// TOGETHER, payload stored decoded so a reviewer reads JSON in the diff,
// compared on a canonical re-encoding of both sides, and driven by the real
// builder over the real registers.yaml — never by reading the file back.
var updateBundleGolden = flag.Bool("update-bundle-golden", false,
	"rewrite internal/hass/testdata/bundle_*.json from the current builder output")

// goldenOrigin is the `origin` block the pinned document carries.
//
// The version string is fixed rather than internal/version.Version, and
// that is the only difference between the pin and what a release build
// writes: Version is a link-time variable, so folding it into the frozen
// artefact would make the artefact stale the moment a tag is cut and the
// regeneration that followed would be a diff nobody reads.
// TestBundleOriginIsWiredFromTheBuildVersion pins the wiring instead.
const goldenOriginSW = "0.0.0-golden"

func realBundle(t *testing.T, lang string) *discovery.Bundle {
	t.Helper()
	d := realDiscovery(t, lang)
	b, err := RenderBundle(d, BundleOrigin(goldenOriginSW))
	if err != nil {
		t.Fatalf("RenderBundle(%s): %v", lang, err)
	}
	return b
}

// readOrUpdateBundleGolden is readOrUpdateGolden with its own flag.
func readOrUpdateBundleGolden(t *testing.T, name string, got any) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	pretty, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	pretty = append(pretty, '\n')
	if *updateBundleGolden {
		if err := os.WriteFile(path, pretty, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("wrote %s", path)
		return pretty
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test fixture path
	if err != nil {
		t.Fatalf("read %s (regenerate with -update-bundle-golden): %v", name, err)
	}
	return want
}

// TestBundleGolden pins the one retained message this daemon publishes to
// Home Assistant's discovery tree: its topic and its whole body, in both
// shipped languages.
//
// Topic and payload together, because they fail differently and both
// silently. A wrong topic publishes a perfectly valid document where Home
// Assistant never looks; a wrong payload publishes to the right place a
// document Home Assistant discards without a line in its log.
func TestBundleGolden(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			d := realDiscovery(t, lang)
			b := realBundle(t, lang)

			if len(b.Components) != 100 {
				t.Fatalf("components = %d, want 100 (89 annotated registers + 9 dual "+
					"emissions + 2 synthetic switches)", len(b.Components))
			}

			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("marshal bundle: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("re-read bundle: %v", err)
			}
			got := goldenEntry{
				Topic:   BundleConfigTopic("homeassistant", d),
				Payload: body,
			}

			pin := readOrUpdateBundleGolden(t, "bundle_"+lang+".json", got)
			var want goldenEntry
			if err := json.Unmarshal(pin, &want); err != nil {
				t.Fatalf("golden bundle_%s.json is not valid json: %v", lang, err)
			}
			if want.Topic != got.Topic {
				t.Errorf("bundle topic moved:\n golden: %s\n  built: %s", want.Topic, got.Topic)
			}
			if wb, gb := canonical(t, want.Payload), canonical(t, got.Payload); !bytes.Equal(wb, gb) {
				diffBundleBodies(t, want.Payload, got.Payload)
				t.Errorf("bundle payload changed (%d golden bytes vs %d built)", len(wb), len(gb))
			}
		})
	}
}

// diffBundleBodies names what moved instead of dumping 50 KB twice.
func diffBundleBodies(t *testing.T, want, got map[string]any) {
	t.Helper()
	for _, k := range []string{"device", "origin", "qos"} {
		if wb, gb := canonical(t, want[k]), canonical(t, got[k]); !bytes.Equal(wb, gb) {
			t.Errorf("bundle %q changed:\n golden: %s\n  built: %s", k, wb, gb)
		}
	}
	wc, _ := want["components"].(map[string]any)
	gc, _ := got["components"].(map[string]any)
	for key, w := range wc {
		g, ok := gc[key]
		if !ok {
			t.Errorf("component no longer in the document: %s", key)
			continue
		}
		if wb, gb := canonical(t, w), canonical(t, g); !bytes.Equal(wb, gb) {
			t.Errorf("component %s changed:\n golden: %s\n  built: %s", key, wb, gb)
		}
	}
	for key := range gc {
		if _, ok := wc[key]; !ok {
			t.Errorf("new component not in the golden: %s", key)
		}
	}
}

// TestTheMoveChangesOnlyTheTopicTheDeviceBlockAndTheOrigin checks this
// release's central claim field by field rather than leaving it to a
// reviewer's eye, against the FROZEN pre-migration pins rather than against
// anything this build renders a second time.
//
// Every one of the 100 entities must arrive in the document carrying
// exactly the body its per-entity config carried, with two differences and
// no others:
//
//   - `platform` is ADDED. The per-entity form put it in the topic and
//     [discovery.Component.EntityJSON] therefore dropped it from the body;
//     a document has no per-entity topic, so it has to be in the entry.
//   - `device` is REMOVED from every entity and appears ONCE at the top of
//     the document, byte-identical to the block all 100 repeated.
//
// `unique_id` above all must not move: it is what Home Assistant keys its
// entity registry on, and every rename, area assignment, icon override and
// automation reference a user has made is keyed on it. Nothing is re-keyed
// by this release, which is why history survives the move.
func TestTheMoveChangesOnlyTheTopicTheDeviceBlockAndTheOrigin(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			frozen := frozenPerEntityConfigs(t, lang)
			if len(frozen) != 100 {
				t.Fatalf("the frozen pin holds %d configs, want 100", len(frozen))
			}
			b := realBundle(t, lang)

			var deviceBlock any
			raw, err := json.Marshal(b.Device)
			if err != nil {
				t.Fatalf("marshal device: %v", err)
			}
			if err := json.Unmarshal(raw, &deviceBlock); err != nil {
				t.Fatalf("re-read device: %v", err)
			}

			// Index the document by the identity the frozen topic carries.
			byIdentity := map[string]map[string]any{}
			for key, comp := range b.Components {
				cb, err := json.Marshal(comp)
				if err != nil {
					t.Fatalf("marshal component %s: %v", key, err)
				}
				var m map[string]any
				if err := json.Unmarshal(cb, &m); err != nil {
					t.Fatalf("re-read component %s: %v", key, err)
				}
				id := string(comp.Platform) + "/" + comp.UniqueID
				if _, dup := byIdentity[id]; dup {
					t.Fatalf("two components share (platform, unique_id) %q; Home Assistant "+
						"keys its registry on that pair and would refuse the device", id)
				}
				byIdentity[id] = m
			}

			for _, e := range frozen {
				platform, uniqueID := splitLegacyConfigTopic(t, e.Topic)
				comp, ok := byIdentity[platform+"/"+uniqueID]
				if !ok {
					t.Errorf("%s is in no component of the document; its entity disappears "+
						"and its history with it", e.Topic)
					continue
				}
				// `platform` added, and it must equal the segment the old
				// topic carried — otherwise the entity changes domain.
				if got := comp["platform"]; got != platform {
					t.Errorf("%s: component platform = %v, want %q", e.Topic, got, platform)
				}
				// `device` removed from the entity, and identical to the
				// document's own block.
				if _, still := comp["device"]; still {
					t.Errorf("%s: the component still repeats the `device` block", e.Topic)
				}
				was := map[string]any{}
				for k, v := range e.Payload {
					was[k] = v
				}
				if wb, gb := canonical(t, was["device"]), canonical(t, deviceBlock); !bytes.Equal(wb, gb) {
					t.Errorf("%s: the device block moved rather than being hoisted:\n"+
						" frozen: %s\n    now: %s", e.Topic, wb, gb)
				}
				delete(was, "device")
				now := map[string]any{}
				for k, v := range comp {
					now[k] = v
				}
				delete(now, "platform")

				if wb, gb := canonical(t, was), canonical(t, now); !bytes.Equal(wb, gb) {
					t.Errorf("%s: the body changed beyond `platform` and `device`:\n"+
						" frozen: %s\n    now: %s", e.Topic, wb, gb)
				}
				if got, _ := now["unique_id"].(string); got != uniqueID {
					t.Errorf("%s: unique_id = %q, want %q — re-keying orphans every "+
						"rename, area and automation a user has made", e.Topic, got, uniqueID)
				}
			}

			// The origin is the one thing that is genuinely new.
			if b.Origin.Name != OriginName || b.Origin.URL != OriginURL {
				t.Errorf("origin = %+v, want name %q and url %q", b.Origin, OriginName, OriginURL)
			}
		})
	}
}

// frozenPerEntityConfigs reads the pre-migration pin. It reads the FILE,
// deliberately: it is the record of what installed brokers hold, and the
// whole point of comparing against it is that it is not produced by the
// code under test.
func frozenPerEntityConfigs(t *testing.T, lang string) []goldenEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "discovery_"+lang+".json")) //nolint:gosec // fixed fixture
	if err != nil {
		t.Fatalf("read the frozen per-entity pin: %v", err)
	}
	var out []goldenEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the frozen per-entity pin is not valid json: %v", err)
	}
	return out
}

func splitLegacyConfigTopic(t *testing.T, topic string) (platform, uniqueID string) {
	t.Helper()
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "homeassistant" || parts[3] != "config" {
		t.Fatalf("%q is not the four-segment per-entity form this fleet is on", topic)
	}
	return parts[1], parts[2]
}

// TestSupersededTopicsAreTheWholeFrozenFleet is the assertion the migration
// lives or dies by.
//
// Publishing the document while ONE per-entity config of a component's
// `unique_id` is still retained makes Home Assistant refuse the whole
// device, in silence, with a single WARNING in its own log and no entities
// on screen. So the retraction list must be the frozen fleet exactly: no
// topic missing, and none invented that reaches into a discovery tree this
// daemon shares with every other integration.
//
// It is DERIVED, not listed. publisher.SupersededTopics renders it from the
// document's own components, which is the property that makes it complete
// by construction rather than by maintenance: the set of unique_ids that
// can conflict with the document is exactly the set of unique_ids IN the
// document, and that is the set the list is built from. The comparison
// against the frozen 100 is what proves the FORM is right.
func TestSupersededTopicsAreTheWholeFrozenFleet(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			frozen := frozenPerEntityConfigs(t, lang)
			want := make([]string, 0, len(frozen))
			for _, e := range frozen {
				want = append(want, e.Topic)
			}
			sort.Strings(want)

			got := SupersededConfigTopics("homeassistant", realBundle(t, lang))
			if len(got) != 100 {
				t.Errorf("the retraction covers %d topics, want the frozen fleet's 100", len(got))
			}
			if wb, gb := canonical(t, want), canonical(t, got); !bytes.Equal(wb, gb) {
				missing, extra := diffTopicLists(want, got)
				t.Errorf("the retraction is not the frozen fleet\n missing (would leave a "+
					"conflict Home Assistant refuses the whole device over): %v\n"+
					" extra (reaches into a shared discovery tree): %v", missing, extra)
			}
		})
	}
}

// TestTheRuledOutLegacyFormsRetractNothing is the same verdict from the
// other side, and it is why hass.LegacyConfigTopicForms exists at all.
//
// publisher.SupersededTopics renders the FIVE-segment form when a consumer
// says nothing. For this fleet that form matches 0 of 100, so a release
// that forgot the one line at the composition root would retract nothing
// whatsoever, publish the document into a tree still holding all 100
// configs, and produce exactly the silent refusal above. Stating a form
// REPLACES the default rather than adding to it, which is what makes that
// one line sufficient.
func TestTheRuledOutLegacyFormsRetractNothing(t *testing.T) {
	frozen := map[string]bool{}
	for _, e := range frozenPerEntityConfigs(t, "en") {
		frozen[e.Topic] = true
	}
	b := realBundle(t, "en")

	for name, form := range map[string]publisher.LegacyTopicFunc{
		"publisher.LegacyTopicWithNodeID (the library default)": publisher.LegacyTopicWithNodeID,
		"publisher.LegacyTopicByObjectID":                       publisher.LegacyTopicByObjectID,
	} {
		hits := 0
		for _, topic := range publisher.SupersededTopics("homeassistant", b, form) {
			if frozen[topic] {
				hits++
			}
		}
		if hits != 0 {
			t.Errorf("%s renders %d of this fleet's retained topics; the measurement "+
				"recorded 0 and the choice of form was made on that", name, hits)
		}
	}
	// And the one that is wired renders all of them.
	hits := 0
	for _, topic := range SupersededConfigTopics("homeassistant", b) {
		if frozen[topic] {
			hits++
		}
	}
	if hits != 100 {
		t.Errorf("the wired form renders %d of 100 retained topics", hits)
	}
}

func diffTopicLists(want, got []string) (missing, extra []string) {
	has := map[string]bool{}
	for _, g := range got {
		has[g] = true
	}
	for _, w := range want {
		if !has[w] {
			missing = append(missing, w)
		}
	}
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w] = true
	}
	for _, g := range got {
		if !wanted[g] {
			extra = append(extra, g)
		}
	}
	return missing, extra
}

// TestBundleNodeIDIsTheSerialAndNothingElse is F16's answer, and it is the
// question the bundle form makes urgent.
//
// The per-entity form this release replaces carries NO serial under the
// shipped defaults: two daemons against two inverters render the same 100
// config topics with the same unique_ids and overwrite each other on every
// boot (recorded as F16 in notes/adr0070-phase6-steps45-results.md §4.3).
// A document is ONE topic per device, so if two instances derived the same
// node id they would not merely overwrite — each would retract and
// republish the other's document, forever.
//
// They do not. The node id is discovery.NodeID over the device's primary
// identifier, and this bridge's primary identifier is the bare serial the
// STATIC read supplies. So the document topic is per-inverter by
// construction, which is strictly BETTER than the form it replaces, and
// HASS_UNIQUE_ID_INCLUDE_SERIAL is not required to make it safe.
//
// What the flag still governs is the entity identities INSIDE the two
// documents, which is a different question and is asserted separately by
// TestTwoDefaultInstancesStillShareEveryUniqueID.
func TestBundleNodeIDIsTheSerialAndNothingElse(t *testing.T) {
	a := realDiscovery(t, "en")
	if got, want := BundleNodeID(a), "mt1234567890"; got != want {
		t.Fatalf("node id = %q, want %q", got, want)
	}
	if got, want := BundleConfigTopic("homeassistant", a), "homeassistant/device/mt1234567890/config"; got != want {
		t.Errorf("bundle topic = %q, want %q", got, want)
	}

	// Per-inverter: a second serial is a second topic.
	b := realDiscovery(t, "en")
	b.Initialize("MT0987654321", goldenFirmware, goldenEquipment)
	if BundleNodeID(a) == BundleNodeID(b) {
		t.Fatal("two inverters render the same node id; the two daemons would retract " +
			"and republish each other's document forever")
	}

	// Not the device name, and not any other operator setting: a renamed
	// device must keep publishing to the same topic, or its old document is
	// orphaned and Home Assistant shows the device twice.
	named := New("homeassistant", "MTEC", a.catalog, "en", DefaultVirtualSwitches(50, 50), "Keller")
	named.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	if got := BundleNodeID(named); got != BundleNodeID(a) {
		t.Errorf("DEVICE_NAME moved the node id to %q; the old document stays retained "+
			"and announces the same device from a second topic", got)
	}
	// Nor the MQTT root, which an operator may change at any time.
	rooted := New("homeassistant", "SOLAR", a.catalog, "en", DefaultVirtualSwitches(50, 50), "")
	rooted.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	if got := BundleNodeID(rooted); got != BundleNodeID(a) {
		t.Errorf("MQTT_TOPIC moved the node id to %q", got)
	}

	// It is case-folded by topic.Slug, which is the one residual collision:
	// two inverters whose serials differ ONLY in case would share a
	// document. Recorded here rather than left to be discovered, and
	// accepted because discovery.Validate requires a node id that is
	// already a legal, sanitised topic segment — see
	// TestTheBundleValidatesWithTheNodeIDItIsPublishedUnder.
	if !strings.EqualFold(BundleNodeID(a), goldenSerial) {
		t.Errorf("node id %q is not the serial %q under case folding; it is derived "+
			"from something else and F16's answer does not hold", BundleNodeID(a), goldenSerial)
	}
}

// TestTheBundleValidatesWithTheNodeIDItIsPublishedUnder closes the loop on
// the slug: discovery.Validate refuses a node id that is not already a
// legal topic segment, and buildBundle withholds the whole migration on a
// blocking validation issue. So an exotic serial cannot produce a document
// published to a malformed topic — it produces one that is slugged, or no
// document at all.
func TestTheBundleValidatesWithTheNodeIDItIsPublishedUnder(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		b := realBundle(t, lang)
		if err := discovery.Validate(b); err != nil {
			t.Errorf("Validate(%s) = %v", lang, err)
		}
		if b.NodeID == "" {
			t.Errorf("%s: the document has no node id", lang)
		}
		if want := "homeassistant/device/" + b.NodeID + "/config"; b.Topic("homeassistant") != want {
			t.Errorf("%s: the document topic and the node id disagree", lang)
		}
	}
}

// TestTwoDefaultInstancesStillShareEveryUniqueID keeps F16 recorded as the
// open defect it is, rather than letting the node id's per-inverter
// property read as a fix for the whole of it.
//
// The topics separate. The identities do not: with
// HASS_UNIQUE_ID_INCLUDE_SERIAL off — the shipped default — two instances
// against two inverters publish two DIFFERENT documents declaring the SAME
// 91 unique_ids, and Home Assistant keys its entity registry on
// (domain, platform, unique_id). It binds each identity to whichever device
// declared it first and rejects the second. That is a different failure
// from the one this release inherits (two writers overwriting one topic)
// and it is louder, but it is still broken, and the documented answer is
// still the flag.
func TestTwoDefaultInstancesStillShareEveryUniqueID(t *testing.T) {
	a := realDiscovery(t, "en")
	b := realDiscovery(t, "en")
	b.Initialize("MT0987654321", goldenFirmware, goldenEquipment)

	ids := func(d *Discovery) map[string]bool {
		bundle, err := RenderBundle(d, BundleOrigin(goldenOriginSW))
		if err != nil {
			t.Fatalf("RenderBundle: %v", err)
		}
		out := map[string]bool{}
		for _, c := range bundle.Components {
			out[string(c.Platform)+"/"+c.UniqueID] = true
		}
		return out
	}
	shared := 0
	idsB := ids(b)
	for id := range ids(a) {
		if idsB[id] {
			shared++
		}
	}
	if shared != 100 {
		t.Fatalf("two default-configured instances share %d of 100 identities; F16 has "+
			"changed shape and the README's guidance has to change with it", shared)
	}

	// With the opt-in the identities separate completely.
	a.IncludeSerialInUniqueIDs(true)
	b.IncludeSerialInUniqueIDs(true)
	idsB = ids(b)
	for id := range ids(a) {
		if idsB[id] {
			t.Fatalf("%q is still shared with HASS_UNIQUE_ID_INCLUDE_SERIAL on; the "+
				"documented answer to F16 does not work", id)
		}
	}
}

// TestBundleOriginIsWiredFromTheBuildVersion pins the one byte of the
// document the golden deliberately does not carry.
func TestBundleOriginIsWiredFromTheBuildVersion(t *testing.T) {
	o := BundleOrigin(version.Version)
	if o.Name != OriginName || o.URL != OriginURL {
		t.Errorf("origin = %+v, want name %q url %q", o, OriginName, OriginURL)
	}
	if o.SW != version.Version || o.SW == "" {
		t.Errorf("origin sw_version = %q, want the build version %q", o.SW, version.Version)
	}
	// It is the BRIDGE's version, not the inverter's. The device block
	// already answers the other question and the two must not be swapped.
	d := realDiscovery(t, "en")
	if o.SW == d.firmware {
		t.Error("the origin carries the inverter firmware; that is the device block's " +
			"sw_version and origin answers `which program wrote this document`")
	}
}

// TestOmittingAComponentDoesNotRemoveIt pins the library and Home Assistant
// contract this daemon relies on when the catalogue shrinks, because it is
// the one place where a bundle is WEAKER than the form it replaces.
//
// Home Assistant removes a component from a device only when its entry is
// present and carries a platform and nothing else. An entry that is simply
// absent leaves the entity in place — so a release that drops a register
// publishes a document that says nothing about it and Home Assistant keeps
// showing it.
//
// It shows it as AVAILABLE, not unavailable, and the earlier wording here
// (and in changelog.md) had that wrong in the flattering direction. The
// stranded entity's `availability` list still names MTEC/bridge/status,
// which this daemon keeps publishing "online" to, so nothing about the
// phantom looks dead — it simply stops updating, or goes on updating if
// the register is still polled, because the poll loop is group-driven and
// consults no discovery hint. There is no signal anywhere that the entity
// belongs to nothing.
//
// It is reachable by ordinary operator action, because registers.yaml ships
// next to the binary and is meant to be edited: deleting a register,
// clearing its `group`, renaming its `mqtt:` key, downgrading
// `hass_component_type` from `number`/`select` to `sensor` (one stranded
// entity) or from `switch` to `sensor` (two), or a typo that makes the
// entry fail validateEntry. It does NOT depend on the language — both
// bundles carry identical component keys.
//
// discovery.Bundle.RemoveComponents is the call that expresses the removal,
// and [discovery.Bundle.Tombstones] is what makes the removal reach the
// entity's OLD per-entity config as well: a tombstone's payload entry is a
// platform and nothing else, by Home Assistant's rule, so the `unique_id`
// publisher.LegacyTopicByUniqueID needs is not in the payload any more and
// has to be remembered outside it.
//
// This daemon does NOT call it. It has no memory of the previous document
// — the catalogue is compiled in and nothing is persisted across a restart
// — so it cannot know which key left. Recorded as F17 in the release notes
// with the operator workaround, and pinned here so the day this daemon
// grows that memory, the contract it has to satisfy is already written down.
func TestOmittingAComponentDoesNotRemoveIt(t *testing.T) {
	b := realBundle(t, "en")
	var key string
	var was discovery.Component
	for k, c := range b.Components {
		if c.UniqueID == "MTEC_inverter_status" || key == "" {
			key, was = k, c
		}
	}
	if key == "" {
		t.Fatal("the document has no components")
	}

	// Omission: the entry is simply gone, and so is any retraction for it.
	shrunk := &discovery.Bundle{
		NodeID: b.NodeID, Device: b.Device, Origin: b.Origin,
		Components: map[string]discovery.Component{},
	}
	for k, c := range b.Components {
		if k != key {
			shrunk.Components[k] = c
		}
	}
	legacy := LegacyConfigTopic("homeassistant", Platform(was.Platform), was.UniqueID)
	for _, topic := range SupersededConfigTopics("homeassistant", shrunk) {
		if topic == legacy {
			t.Fatalf("omitting %s still retracts %s; the library's contract changed and "+
				"F17's workaround can be dropped", key, legacy)
		}
	}
	if _, still := shrunk.Components[key]; still {
		t.Fatal("the fixture did not actually omit the component")
	}

	// The expressed removal: a platform-only entry, and the identity kept
	// outside the payload so the legacy retraction can still be rendered.
	removed := &discovery.Bundle{
		NodeID: b.NodeID, Device: b.Device, Origin: b.Origin,
		Components: map[string]discovery.Component{},
	}
	for k, c := range shrunk.Components {
		removed.Components[k] = c
	}
	removed.RemoveComponents(b.Components, key)

	entry, ok := removed.Components[key]
	if !ok {
		t.Fatal("RemoveComponents left no entry; an absent key removes nothing")
	}
	if entry.Platform != was.Platform {
		t.Errorf("the tombstone's platform = %q, want %q", entry.Platform, was.Platform)
	}
	if entry.UniqueID != "" {
		t.Error("the tombstone carries a unique_id; that un-removes the entity it exists " +
			"to finish removing")
	}
	body, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal tombstone: %v", err)
	}
	if got, want := string(body), `{"platform":"`+string(was.Platform)+`"}`; got != want {
		t.Errorf("tombstone body = %s, want %s", got, want)
	}
	if got := removed.Tombstones[key].UniqueID; got != was.UniqueID {
		t.Errorf("Tombstones[%s].unique_id = %q, want %q — without it the legacy "+
			"retraction has nothing to key on", key, got, was.UniqueID)
	}
	var found bool
	for _, topic := range SupersededConfigTopics("homeassistant", removed) {
		if topic == legacy {
			found = true
		}
	}
	if !found {
		t.Errorf("the expressed removal does not retract %s; the deleted entity's stale "+
			"retained config re-creates it on every MQTT-integration restart", legacy)
	}
}
