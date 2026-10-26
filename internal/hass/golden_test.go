// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SukramJ/go-hamqtt/discovery"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
	"github.com/SukramJ/go-mtec2mqtt/internal/version"
)

// The golden files in testdata/ pin the one retained message this bridge
// publishes to Home Assistant's discovery tree — the device document, in
// both shipped languages — and, separately, its identity plane: the
// strings Home Assistant has no migration path for. Built by the real
// builder over the real registers.yaml, never by reading the file back.
//
// Regenerate with
//
//	go test ./internal/hass -run 'Golden' -update-golden
//
// and read the diff: a non-empty diff is a change to what an installed
// base receives, never "just a test update". An identity.json diff orphans
// entities.
var updateGolden = flag.Bool("update-golden", false,
	"rewrite internal/hass/testdata/*.json from the current builder output")

// Fixed device identity for the goldens. Every topic and the device block
// are keyed on them, so they are chosen once and never changed.
const (
	goldenSerial    = "MT1234567890"
	goldenFirmware  = "1.2.3"
	goldenEquipment = "EB-10kW"
	// goldenOriginSW stands in for the link-time build version, which would
	// otherwise move the golden on every release;
	// TestBundleOriginIsWiredFromTheBuildVersion pins the wiring.
	goldenOriginSW = "0.0.0-golden"
)

// goldenEntry is the pinned document: its topic and its payload, stored
// decoded so a reviewer reads JSON in the diff. Comparison is on a
// canonical re-encoding of both sides.
type goldenEntry struct {
	Topic   string         `json:"topic"`
	Payload map[string]any `json:"payload"`
}

// goldenIdentity is one component's identity.
type goldenIdentity struct {
	Component       string `json:"component"`
	UniqueID        string `json:"unique_id"`
	DefaultEntityID string `json:"default_entity_id"`
}

// goldenIdentities is the identity plane of the document.
type goldenIdentities struct {
	Topic       string           `json:"topic"`
	NodeID      string           `json:"node_id"`
	Identifiers []string         `json:"device_identifiers"`
	Components  []goldenIdentity `json:"components"`
}

// realCatalog loads the shipped registers.yaml.
func realCatalog(t *testing.T) *registers.Map {
	t.Helper()
	m, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return m
}

// realDiscovery builds the shipped discovery plane: the real catalog, the
// instance name "MTEC", the two built-in synthetic switches and no
// operator device name.
func realDiscovery(t *testing.T, lang string) *Discovery {
	t.Helper()
	// The on-values are the coordinator's write payloads and never reach
	// the document; any value builds the same entities.
	d := New("MTEC", realCatalog(t), lang, DefaultVirtualSwitches(50, 50), "")
	d.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	return d
}

func realBundle(t *testing.T, lang string) *discovery.Bundle {
	t.Helper()
	b, err := RenderBundle(realDiscovery(t, lang), BundleOrigin(goldenOriginSW))
	if err != nil {
		t.Fatalf("RenderBundle(%s): %v", lang, err)
	}
	return b
}

// bundleBody is the document's JSON body, decoded.
func bundleBody(t *testing.T, b *discovery.Bundle) map[string]any {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("re-read bundle: %v", err)
	}
	return body
}

// canonical re-encodes v with sorted keys and no indentation, so
// formatting can never hide a value change.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	bs, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	return bs
}

// readOrUpdateGolden returns testdata/<name>, or rewrites it from got when
// -update-golden is set.
func readOrUpdateGolden(t *testing.T, name string, got any) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	pretty, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	pretty = append(pretty, '\n')
	if *updateGolden {
		if err := os.WriteFile(path, pretty, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("wrote %s", path)
		return pretty
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test fixture path
	if err != nil {
		t.Fatalf("read %s (regenerate with -update-golden): %v", name, err)
	}
	return want
}

// TestBundleGolden pins the document's topic and its whole body. Topic and
// payload together, because they fail differently and both silently: a
// wrong topic is a valid document where Home Assistant never looks, a
// wrong payload is one it discards without a line in its log.
func TestBundleGolden(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			d := realDiscovery(t, lang)
			b := realBundle(t, lang)
			if len(b.Components) != 100 {
				t.Fatalf("components = %d, want 100 (89 annotated registers + 9 dual "+
					"emissions + 2 synthetic switches)", len(b.Components))
			}
			got := goldenEntry{Topic: BundleConfigTopic("homeassistant", d), Payload: bundleBody(t, b)}

			pin := readOrUpdateGolden(t, "bundle_"+lang+".json", got)
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

// diffBundleBodies names what moved instead of dumping 100 KB twice.
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

// TestIdentityGolden pins the identity plane on its own, so an identity
// change and a payload change are two different diffs: the document topic
// and node id, the device identifiers (the device-registry key), and every
// component's unique_id and default_entity_id. Language-independent by
// design, which is asserted by rendering both languages against one file.
func TestIdentityGolden(t *testing.T) {
	identities := func(lang string) goldenIdentities {
		d := realDiscovery(t, lang)
		b := realBundle(t, lang)
		out := goldenIdentities{
			Topic:  BundleConfigTopic("homeassistant", d),
			NodeID: b.NodeID,
		}
		doc := bundleBody(t, b)
		dev, _ := doc["device"].(map[string]any)
		for _, id := range dev["identifiers"].([]any) {
			out.Identifiers = append(out.Identifiers, id.(string))
		}
		comps, _ := doc["components"].(map[string]any)
		for _, key := range slices.Sorted(maps.Keys(b.Components)) {
			body, _ := comps[key].(map[string]any)
			eid, _ := body["default_entity_id"].(string)
			out.Components = append(out.Components, goldenIdentity{
				Component: key, UniqueID: b.Components[key].UniqueID, DefaultEntityID: eid,
			})
		}
		return out
	}
	en := identities("en")
	pin := readOrUpdateGolden(t, "identity.json", en)
	var want goldenIdentities
	if err := json.Unmarshal(pin, &want); err != nil {
		t.Fatalf("golden identity.json is not valid json: %v", err)
	}
	for _, got := range []goldenIdentities{en, identities("de")} {
		if wb, gb := canonical(t, want), canonical(t, got); !bytes.Equal(wb, gb) {
			t.Errorf("identity plane changed — every changed unique_id or entity id is an "+
				"orphaned entity in an installed base:\n golden: %s\n  built: %s", wb, gb)
		}
	}
}

// TestBundleNodeIDIsTheSerialAndNothingElse: the node id is
// discovery.NodeID over the bare serial, so the document topic is
// per-inverter, stable, and moved by no operator setting.
func TestBundleNodeIDIsTheSerialAndNothingElse(t *testing.T) {
	a := realDiscovery(t, "en")
	if got, want := BundleNodeID(a), "mt1234567890"; got != want {
		t.Fatalf("node id = %q, want %q", got, want)
	}
	if got, want := BundleConfigTopic("homeassistant", a), "homeassistant/device/mt1234567890/config"; got != want {
		t.Errorf("bundle topic = %q, want %q", got, want)
	}

	b := realDiscovery(t, "en")
	b.Initialize("MT0987654321", goldenFirmware, goldenEquipment)
	if BundleNodeID(a) == BundleNodeID(b) {
		t.Fatal("two inverters render the same node id; the two daemons would overwrite " +
			"each other's document")
	}

	named := New("MTEC", a.catalog, "en", DefaultVirtualSwitches(50, 50), "Keller")
	named.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	if got := BundleNodeID(named); got != BundleNodeID(a) {
		t.Errorf("DEVICE_NAME moved the node id to %q; the old document stays retained "+
			"and announces the same device from a second topic", got)
	}
	rooted := New("SOLAR", a.catalog, "en", DefaultVirtualSwitches(50, 50), "")
	rooted.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	if got := BundleNodeID(rooted); got != BundleNodeID(a) {
		t.Errorf("MQTT_TOPIC moved the node id to %q", got)
	}
	if !strings.EqualFold(BundleNodeID(a), goldenSerial) {
		t.Errorf("node id %q is not the serial %q under case folding", BundleNodeID(a), goldenSerial)
	}
}

// TestTheBundleValidatesWithTheNodeIDItIsPublishedUnder: discovery.Validate
// applies Home Assistant's per-platform schemas to every component and
// refuses a node id that is not a legal topic segment; buildBundle
// withholds a document that fails it.
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

// TestTwoDefaultInstancesStillShareEveryUniqueID: the documents separate by
// node id, the identities inside them do not — with
// HASS_UNIQUE_ID_INCLUDE_SERIAL off, two instances declare the same
// unique_ids and Home Assistant binds each to the first device. The
// opt-in is the documented answer.
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
		t.Fatalf("two default-configured instances share %d of 100 identities; the "+
			"README's guidance has to change", shared)
	}

	a.IncludeSerialInUniqueIDs(true)
	b.IncludeSerialInUniqueIDs(true)
	idsB = ids(b)
	for id := range ids(a) {
		if idsB[id] {
			t.Fatalf("%q is still shared with HASS_UNIQUE_ID_INCLUDE_SERIAL on", id)
		}
	}
}

// TestBundleOriginIsWiredFromTheBuildVersion pins the one value of the
// document the golden deliberately does not carry.
func TestBundleOriginIsWiredFromTheBuildVersion(t *testing.T) {
	o := BundleOrigin(version.Version)
	if o.Name != OriginName || o.URL != OriginURL {
		t.Errorf("origin = %+v, want name %q url %q", o, OriginName, OriginURL)
	}
	if o.SW != version.Version || o.SW == "" {
		t.Errorf("origin sw_version = %q, want the build version %q", o.SW, version.Version)
	}
	if d := realDiscovery(t, "en"); o.SW == d.firmware {
		t.Error("the origin carries the inverter firmware; that is the device block's sw_version")
	}
}

// TestOmittingAComponentDoesNotRemoveIt pins the Home Assistant contract
// this daemon relies on when the catalogue shrinks: Home Assistant removes
// a component only when its entry is present and carries a platform and
// nothing else. An entry that is simply absent leaves the entity in place
// — so a register deleted from registers.yaml leaves a stale entity behind
// (delete it in Home Assistant). This daemon keeps no memory of the
// previous document, so it cannot express the removal;
// discovery.Bundle.RemoveComponents is the call it would use.
func TestOmittingAComponentDoesNotRemoveIt(t *testing.T) {
	b := realBundle(t, "en")
	const key = "sensor.inverter_status"
	was, ok := b.Components[key]
	if !ok {
		t.Fatalf("the document has no %s", key)
	}
	removed := &discovery.Bundle{
		NodeID: b.NodeID, Device: b.Device, Origin: b.Origin,
		Components: map[string]discovery.Component{},
	}
	for k, c := range b.Components {
		if k != key {
			removed.Components[k] = c
		}
	}
	removed.RemoveComponents(b.Components, key)
	entry, ok := removed.Components[key]
	if !ok {
		t.Fatal("RemoveComponents left no entry; an absent key removes nothing")
	}
	body, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal tombstone: %v", err)
	}
	if got, want := string(body), `{"platform":"`+string(was.Platform)+`"}`; got != want {
		t.Errorf("tombstone body = %s, want %s", got, want)
	}
}
