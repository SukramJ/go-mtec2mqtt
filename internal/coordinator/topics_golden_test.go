// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// This file pins the wire this daemon actually writes to: the state and
// command topic tree, the delivery guarantee of every publish, and — the
// point of the exercise — that the *two independent state-topic builders*
// agree. See notes/adr0070-phase6-measurement.md, finding F5: the config
// payload's `state_topic` is composed in internal/hass/discovery.go, the
// value is published from a second expression in poll.go, and until now
// nothing compared them. A divergence leaves every entity pointing at a
// topic nobody publishes, permanently `unknown`, with nothing in the log
// and nothing in Home Assistant's registry to notice it.
//
// The golden is driven through the *real* builders over the real
// registers.yaml: publishGroupOnce for the publisher side, hass.Discovery
// for the config side. Regenerate with:
//
//	go test ./internal/coordinator -run TestTopicGolden -update-topics-golden
//
// # Pinned defects
//
//   - F3 — state is published non-retained, so a Home Assistant restart
//     blanks every entity until the next poll (up to an hour for the
//     `static` cadence). Pinned by TestPublishQoSAndRetain, which is also
//     the first test in this repository to assert a QoS at all.
//   - F10 — the state-topic tree is wider than the discovery tree: the
//     `static` group and two unannotated `now-base` registers publish
//     topics no entity reads. Pinned as `state_topics_without_entity` in
//     the golden, so the later filter step shows up as that list emptying.
var updateTopicsGolden = flag.Bool("update-topics-golden", false,
	"rewrite internal/coordinator/testdata/topics.json from the current builders")

const (
	goldenSerial    = "MT1234567890"
	goldenFirmware  = "1.2.3"
	goldenEquipment = "EB-10kW"
)

// topicGolden is the pinned topic tree. Every list is sorted.
type topicGolden struct {
	// StateTopics is every topic publishGroupOnce writes a value to,
	// across all catalog groups.
	StateTopics []string `json:"state_topics"`
	// DiscoveryConfigTopics is every retained config topic.
	DiscoveryConfigTopics []string `json:"discovery_config_topics"`
	// CommandTopics is every topic an entity tells Home Assistant to
	// write back to.
	CommandTopics []string `json:"command_topics"`
	// SubscribeFilters is what the daemon asks the broker for.
	SubscribeFilters []string `json:"subscribe_filters"`
	// StateTopicsWithoutEntity is F10: published, but no entity reads it.
	StateTopicsWithoutEntity []string `json:"state_topics_without_entity"`
}

// realTopicCoordinator wires a Coordinator over the real catalog and the
// real discovery builder, with a reader that answers every group with
// every register in it. Nothing connects: the test drives the publish
// path directly.
func realTopicCoordinator(t *testing.T) (*Coordinator, *hass.Discovery, *stubMQTT, *registers.Map) {
	t.Helper()
	catalog, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	// The shipped defaults an untouched deployment runs with: topic root
	// "MTEC", discovery root "homeassistant", language "en".
	cfg, err := config.Load(strings.NewReader(`
MODBUS_IP: 127.0.0.1
MODBUS_PORT: 502
MODBUS_SLAVE: 247
MODBUS_TIMEOUT: 5
MQTT_SERVER: localhost
MQTT_PORT: 1883
MQTT_TOPIC: MTEC
HASS_ENABLE: true
`), nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	reader := newStubReader()
	for _, g := range catalog.Groups {
		data := map[string]any{}
		for _, r := range catalog.ByGroup(g) {
			// The value is irrelevant to a topic; 0 flows through every
			// formatter without being skipped.
			data[r.MQTT] = 0
		}
		reader.groupData[g] = data
	}

	mqttStub := newStubMQTT()
	virtual := hass.DefaultVirtualSwitches(cfg.ChargeActiveValue, cfg.DischargeActiveValue)
	discovery := hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, catalog, cfg.Language, virtual, cfg.DeviceName)
	discovery.Initialize(goldenSerial, goldenFirmware, goldenEquipment)

	c := New(Deps{
		Cfg:     cfg,
		Catalog: catalog,
		Modbus:  &stubModbus{},
		Reader:  reader,
		MQTT:    mqttStub,
		HASS:    discovery,
		Virtual: virtual,
		Logger:  slog.New(slog.DiscardHandler),
		Now:     func() time.Time { return time.Date(2026, 5, 25, 14, 30, 45, 0, time.UTC) },
	})
	// What runStatic would have stored after the STATIC read. Every state
	// topic is keyed on it, exactly as in production.
	c.topicBase.Store(cfg.MQTTTopic + "/" + goldenSerial)
	return c, discovery, mqttStub, catalog
}

// publishEveryGroup runs one poll cycle per catalog group through the
// production publish path and returns what was published.
func publishEveryGroup(t *testing.T, c *Coordinator, catalog *registers.Map, stub *stubMQTT) []publishCall {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	for _, g := range catalog.Groups {
		c.publishGroupOnce(context.Background(), log, g)
	}
	pubs := stub.snapshotPublishes()
	if len(pubs) == 0 {
		t.Fatal("no state publishes — the poll path did not run")
	}
	return pubs
}

// discoveryStateTopics reads the state_topic out of each built config
// payload: the *config* side of F5.
func discoveryStateTopics(t *testing.T, d *hass.Discovery) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range d.Entries() {
		var payload struct {
			StateTopic string `json:"state_topic"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("%s: %v", e.ConfigTopic, err)
		}
		if payload.StateTopic == "" {
			t.Errorf("%s has no state_topic", e.ConfigTopic)
			continue
		}
		out[e.ConfigTopic] = payload.StateTopic
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// subscribeFilters asks the production code what it subscribes to —
// installInboundHandler for the two startup filters and
// Discovery.ConfigFilter for the orphan sweep's — rather than
// re-deriving the strings in the test, which would pin nothing.
func subscribeFilters(t *testing.T, c *Coordinator, d *hass.Discovery, stub *stubMQTT) []string {
	t.Helper()
	if err := c.installInboundHandler(context.Background()); err != nil {
		t.Fatalf("installInboundHandler: %v", err)
	}
	stub.mu.Lock()
	got := append([]string(nil), stub.subscribes...)
	stub.subscribes, stub.subscribeQoS = nil, nil
	stub.mu.Unlock()
	got = append(got, d.ConfigFilter())
	sort.Strings(got)
	return got
}

// TestTopicGolden pins the whole topic tree — state, config, command and
// the filters the daemon subscribes to — as produced by the real builders.
func TestTopicGolden(t *testing.T) {
	c, discovery, stub, catalog := realTopicCoordinator(t)

	stateSet := map[string]bool{}
	for _, p := range publishEveryGroup(t, c, catalog, stub) {
		stateSet[p.topic] = true
	}

	configSet := map[string]bool{}
	commandSet := map[string]bool{}
	for _, e := range discovery.Entries() {
		configSet[e.ConfigTopic] = true
		if e.CommandTopic != "" {
			commandSet[e.CommandTopic] = true
		}
	}

	entityStateTopics := map[string]bool{}
	for _, st := range discoveryStateTopics(t, discovery) {
		entityStateTopics[st] = true
	}
	orphanState := map[string]bool{}
	for st := range stateSet {
		if !entityStateTopics[st] {
			orphanState[st] = true
		}
	}

	got := topicGolden{
		StateTopics:              sortedKeys(stateSet),
		DiscoveryConfigTopics:    sortedKeys(configSet),
		CommandTopics:            sortedKeys(commandSet),
		SubscribeFilters:         subscribeFilters(t, c, discovery, stub),
		StateTopicsWithoutEntity: sortedKeys(orphanState),
	}

	path := filepath.Join("testdata", "topics.json")
	pretty, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pretty = append(pretty, '\n')
	if *updateTopicsGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	raw, err := os.ReadFile(path) //nolint:gosec // fixed test fixture path
	if err != nil {
		t.Fatalf("read %s (regenerate with -update-topics-golden): %v", path, err)
	}
	var want topicGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("golden topics.json is not valid json: %v", err)
	}
	diffTopicList(t, "state_topics", want.StateTopics, got.StateTopics)
	diffTopicList(t, "discovery_config_topics", want.DiscoveryConfigTopics, got.DiscoveryConfigTopics)
	diffTopicList(t, "command_topics", want.CommandTopics, got.CommandTopics)
	diffTopicList(t, "subscribe_filters", want.SubscribeFilters, got.SubscribeFilters)
	diffTopicList(t, "state_topics_without_entity", want.StateTopicsWithoutEntity, got.StateTopicsWithoutEntity)
}

// diffTopicList reports added and removed topics by name rather than
// dumping both lists.
func diffTopicList(t *testing.T, name string, want, got []string) {
	t.Helper()
	inWant := map[string]bool{}
	for _, s := range want {
		inWant[s] = true
	}
	inGot := map[string]bool{}
	for _, s := range got {
		inGot[s] = true
	}
	for _, s := range want {
		if !inGot[s] {
			t.Errorf("%s: topic no longer produced: %s", name, s)
		}
	}
	for _, s := range got {
		if !inWant[s] {
			t.Errorf("%s: new topic not in golden: %s", name, s)
		}
	}
}

// TestStateTopicBuildersAgree is F5's pin. Every entity's advertised
// state_topic must be one the poll loop actually publishes to — checked
// against the real publish path, not against the golden file, so a change
// to either builder alone fails here even after a regeneration.
func TestStateTopicBuildersAgree(t *testing.T) {
	c, discovery, stub, catalog := realTopicCoordinator(t)

	published := map[string]bool{}
	for _, p := range publishEveryGroup(t, c, catalog, stub) {
		published[p.topic] = true
	}

	advertised := discoveryStateTopics(t, discovery)
	if len(advertised) != 100 {
		t.Errorf("advertised state topics = %d, want 100", len(advertised))
	}
	distinct := map[string]bool{}
	for configTopic, stateTopic := range advertised {
		distinct[stateTopic] = true
		if !published[stateTopic] {
			t.Errorf("%s advertises %s, which the poll loop never publishes to "+
				"(hass/discovery.go and coordinator/poll.go have diverged)", configTopic, stateTopic)
		}
	}
	if len(distinct) != 91 {
		t.Errorf("distinct advertised state topics = %d, want 91 (nine pairs share one)", len(distinct))
	}
}

// TestCommandTopicsAreSubscribed proves the wildcard the daemon
// subscribes to covers every command topic it advertises — the same
// class of divergence as F5, on the inbound half.
func TestCommandTopicsAreSubscribed(t *testing.T) {
	_, discovery, _, _ := realTopicCoordinator(t)
	filter := "MTEC/+/+/+/set"
	n := 0
	for _, e := range discovery.Entries() {
		if e.CommandTopic == "" {
			continue
		}
		n++
		if !matchTopicFilter(filter, e.CommandTopic) {
			t.Errorf("%s is advertised but not covered by %s", e.CommandTopic, filter)
		}
	}
	if n != 11 {
		t.Errorf("command topics = %d, want 11", n)
	}
}

// TestPublishQoSAndRetain pins the delivery guarantee of every publish
// this daemon makes. Before this test, every MQTT stub in the repository
// discarded the QoS argument, so the guarantee was asserted nowhere.
//
// The state row pins F3: QoS 0 and retain=false, so a Home Assistant
// restart finds no value for any entity. When step 2 flips it, this test
// fails, and that failure is the whole point.
func TestPublishQoSAndRetain(t *testing.T) {
	c, _, stub, catalog := realTopicCoordinator(t)

	for _, p := range publishEveryGroup(t, c, catalog, stub) {
		if p.qos != mqtt.QoS0 {
			t.Errorf("state publish %s: qos = %v, want QoS0", p.topic, p.qos)
		}
		if p.retain {
			t.Errorf("state publish %s: retain = true — F3 is fixed; update this test", p.topic)
		}
	}

	// Discovery: retained, QoS 0.
	stub.mu.Lock()
	stub.publishes = nil
	stub.mu.Unlock()
	c.publishDiscovery(context.Background())
	discoveryPubs := stub.snapshotPublishes()
	if len(discoveryPubs) != 100 {
		t.Fatalf("discovery publishes = %d, want 100", len(discoveryPubs))
	}
	for _, p := range discoveryPubs {
		if p.qos != mqtt.QoS0 {
			t.Errorf("discovery publish %s: qos = %v, want QoS0", p.topic, p.qos)
		}
		if !p.retain {
			t.Errorf("discovery publish %s: retain = false, want true", p.topic)
		}
	}
}

// TestSubscribeQoS pins the QoS of the two startup subscriptions and of
// the orphan-reconcile one, for the same reason as above: nothing
// asserted them.
func TestSubscribeQoS(t *testing.T) {
	c, _, stub, _ := realTopicCoordinator(t)
	if err := c.installInboundHandler(context.Background()); err != nil {
		t.Fatalf("installInboundHandler: %v", err)
	}
	stub.mu.Lock()
	filters, qoss := append([]string(nil), stub.subscribes...), append([]mqtt.QoS(nil), stub.subscribeQoS...)
	stub.mu.Unlock()

	want := map[string]mqtt.QoS{
		"homeassistant/status": mqtt.QoS1,
		"MTEC/+/+/+/set":       mqtt.QoS1,
	}
	if len(filters) != len(want) {
		t.Fatalf("subscribed to %v, want %d filters", filters, len(want))
	}
	for i, f := range filters {
		q, ok := want[f]
		if !ok {
			t.Errorf("unexpected subscription %s", f)
			continue
		}
		if qoss[i] != q {
			t.Errorf("subscribe %s: qos = %v, want %v", f, qoss[i], q)
		}
	}
}

// TestStatePayloadsAreCanonical pins that no state publish carries an
// empty payload today. Under F3's fix an empty *retained* payload is
// MQTT's retraction — it would delete the value rather than blank it —
// which is F8, and is why F3 must not be fixed by flipping a boolean.
func TestStatePayloadsAreCanonical(t *testing.T) {
	c, _, stub, catalog := realTopicCoordinator(t)
	for _, p := range publishEveryGroup(t, c, catalog, stub) {
		if len(bytes.TrimSpace(p.payload)) == 0 {
			t.Errorf("%s published an empty payload — under a retained state plane this is a retraction (F8)", p.topic)
		}
	}
}
