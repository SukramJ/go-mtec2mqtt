// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"
	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// This file covers what ADR 0070 phase 6 steps 4 and 5 moved onto
// go-hamqtt: the state plane's de-duplication and its delivery guarantee,
// the bridge availability announcements, and the command router. The
// topic tree itself is pinned next door, in topics_golden_test.go, and
// nothing here may move a byte of it.

// --- step 4: the state plane ------------------------------------------------

// TestStateQoSIsStatedNotDefaulted is the single most expensive line of
// this whole step, and it is a constant.
//
// publisher.QoS's zero value is QoSUnset, which every runtime type in the
// library resolves to QoS 1. This bridge has published its entire state
// plane at QoS 0 since its first release. A StateConfig that simply
// omitted the field would therefore have tripled this bridge's broker
// traffic and changed the delivery guarantee of an installed base, inside
// a step whose stated purpose is de-duplication, with a broker capture as
// the only evidence. QoSAtMostOnce is 0x80 — outside the wire's 0-2 range
// — precisely so "unset" and "deliberately at most once" cannot be
// written the same way.
//
// The wire byte is asserted alongside the sentinel, because the sentinel
// is only useful if it resolves to 0.
func TestStateQoSIsStatedNotDefaulted(t *testing.T) {
	if StateQoS == publisher.QoSUnset {
		t.Fatal("StateQoS is the zero value, which the library reads as *unset* and resolves to QoS 1")
	}
	if StateQoS != publisher.QoSAtMostOnce {
		t.Fatalf("StateQoS = %v, want QoSAtMostOnce", StateQoS)
	}
	wire, ok := StateQoS.Wire()
	if !ok || wire != 0 {
		t.Fatalf("StateQoS.Wire() = (%d, %v), want (0, true)", wire, ok)
	}
	// The two neighbours, for the same reason: each is a separate answer
	// to a separate question and the library reads an omitted field as
	// neither.
	if w, ok := DiscoveryQoS.Wire(); !ok || w != 0 {
		t.Errorf("DiscoveryQoS.Wire() = (%d, %v), want (0, true)", w, ok)
	}
	if w, ok := CommandQoS.Wire(); !ok || w != 1 {
		t.Errorf("CommandQoS.Wire() = (%d, %v), want (1, true)", w, ok)
	}
}

// TestStatePublishIsDeduplicated is what step 4 buys. The poll loop
// re-read and re-published every value of every group on every cycle —
// roughly 11 136 messages an hour on the shipped cadences, nearly all of
// them byte-identical to the message before. A second cycle over
// unchanged values must now write nothing at all.
func TestStatePublishIsDeduplicated(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	c.topicBase.Store(topicParts{root: "MTEC", serial: "MTEC-TEST-001"})
	log := slog.New(slog.DiscardHandler)

	c.publishGroupOnce(context.Background(), log, registers.GroupBase)
	first := len(mqttStub.snapshotPublishes())
	if first == 0 {
		t.Fatal("the first cycle published nothing")
	}

	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	c.publishGroupOnce(context.Background(), log, registers.GroupBase)
	if n := len(mqttStub.snapshotPublishes()); n != 0 {
		t.Errorf("the second cycle re-published %d of %d unchanged values", n, first)
	}
}

// TestChangedStateValueIsPublished is the gate's other direction, and the
// one whose failure would be a bridge that stops reporting: a value that
// actually changed must go out.
func TestChangedStateValueIsPublished(t *testing.T) {
	c, reader, mqttStub, _ := buildDeps(t, false)
	c.topicBase.Store(topicParts{root: "MTEC", serial: "MTEC-TEST-001"})
	log := slog.New(slog.DiscardHandler)

	c.publishGroupOnce(context.Background(), log, registers.GroupBase)
	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	reader.mu.Lock()
	reader.groupData[registers.GroupBase]["grid_power"] = -501
	reader.mu.Unlock()

	c.publishGroupOnce(context.Background(), log, registers.GroupBase)
	wrote := map[string]bool{}
	for _, p := range mqttStub.snapshotPublishes() {
		wrote[p.topic] = true
	}
	const base = "MTEC/MTEC-TEST-001/now-base/"
	if !wrote[base+"grid_power/state"] {
		t.Error("the changed value was suppressed by the dedup gate")
	}
	// `consumption` is a pseudo-register computed from grid_power, so it
	// legitimately changes with it. Named here rather than left as an
	// unexplained count.
	if !wrote[base+"consumption/state"] {
		t.Error("the pseudo-register derived from the changed value was suppressed")
	}
	if wrote[base+"inverter/state"] {
		t.Error("an unchanged value was re-published; the dedup gate is not engaged")
	}
	if len(wrote) != 2 {
		t.Errorf("wrote %d topics, want exactly grid_power and consumption: %v", len(wrote), wrote)
	}
}

// TestPublishOnlineReopensTheDedupGate is the half of the reconnect path
// that is new rather than preserved.
//
// A (re)connect may be to a broker that came back without its retained
// store. The dedup cache would then answer "already published" for values
// the broker no longer holds, and every entity would sit blank until its
// value happened to change — which for the `static` and `total` groups is
// effectively never. Reset opens the gate without forgetting the index,
// so the next poll writes the fleet once and is deduped again afterwards.
func TestPublishOnlineReopensTheDedupGate(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	c.topicBase.Store(topicParts{root: "MTEC", serial: "MTEC-TEST-001"})
	log := slog.New(slog.DiscardHandler)

	c.publishGroupOnce(context.Background(), log, registers.GroupBase)
	first := len(mqttStub.snapshotPublishes())
	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	// The broker came back; the lifecycle fires OnConnect.
	c.PublishOnline(context.Background())

	c.publishGroupOnce(context.Background(), log, registers.GroupBase)
	states := 0
	for _, p := range mqttStub.snapshotPublishes() {
		if endsWith(p.topic, "/state") {
			states++
		}
	}
	if states != first {
		t.Errorf("after a reconnect the poll rewrote %d of %d values; a broker that lost its "+
			"retained store leaves the rest of the fleet blank forever", states, first)
	}
}

// --- step 5: birth and LWT --------------------------------------------------

// TestAnnouncementsUseTheBridgeTopic pins the three wire values of this
// daemon's own availability marker, on both edges.
//
// The topic is the SAME function the discovery builder points all 100
// entities at, and it is deliberately not under the serial: the marker is
// published at CONNECT, before the STATIC read that learns the serial
// number. An availability topic no entity references is the measured
// defect of two sibling bridges — the broker dutifully writes "offline"
// on a hard crash and every entity stays available forever, showing the
// last value it ever saw.
func TestAnnouncementsUseTheBridgeTopic(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	want := hass.BridgeStatusTopic(c.deps.Cfg.MQTTTopic)

	c.PublishOnline(context.Background())
	c.PublishOffline(context.Background())

	var got []publishCall
	for _, p := range mqttStub.snapshotPublishes() {
		if p.topic == want {
			got = append(got, p)
		}
	}
	if len(got) != 2 {
		t.Fatalf("announcements on %s = %d, want 2 (online then offline)", want, len(got))
	}
	if string(got[0].payload) != hass.PayloadAvailable {
		t.Errorf("birth payload = %q, want %q", got[0].payload, hass.PayloadAvailable)
	}
	if string(got[1].payload) != hass.PayloadNotAvailable {
		t.Errorf("death payload = %q, want %q", got[1].payload, hass.PayloadNotAvailable)
	}
	for i, p := range got {
		if !p.retain {
			t.Errorf("announcement %d is not retained — a marker a late subscriber cannot read tells nothing", i)
		}
		if p.qos != mqtt.QoS0 {
			t.Errorf("announcement %d: qos = %v, want QoS0 (unchanged)", i, p.qos)
		}
	}
	// And it must not be under the serial, which is the property that
	// makes publishing it at CONNECT possible at all.
	if startsWith(want, c.deps.Cfg.MQTTTopic+"/MTEC-TEST-001") {
		t.Error("the bridge status topic is scoped by the serial, which is not known at CONNECT")
	}
}

// TestWillAgreesWithTheAnnouncements closes the loop the composition root
// opens: the Last Will handed to CONNECT and the marker PublishOnline /
// PublishOffline write must be one topic and one pair of payloads. They
// are, because both come from the same publisher.Runtime.
func TestWillAgreesWithTheAnnouncements(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	will, err := c.deps.HARuntime.Will()
	if err != nil {
		t.Fatalf("Will() = %v", err)
	}

	c.PublishOffline(context.Background())
	var death publishCall
	for _, p := range mqttStub.snapshotPublishes() {
		if p.topic == will.Topic {
			death = p
		}
	}
	if death.topic == "" {
		t.Fatalf("nothing was published to the will's topic %q", will.Topic)
	}
	if !bytes.Equal(death.payload, will.Payload) {
		t.Errorf("the will says %q and the graceful shutdown says %q", will.Payload, death.payload)
	}
	if mqtt.QoS(will.QoS) != death.qos || will.Retain != death.retain {
		t.Errorf("will{qos:%d retain:%v} vs announcement{qos:%v retain:%v}",
			will.QoS, will.Retain, death.qos, death.retain)
	}
	if will.Topic != hass.BridgeStatusTopic(c.deps.Cfg.MQTTTopic) {
		t.Errorf("will topic = %q, want the builder's %q",
			will.Topic, hass.BridgeStatusTopic(c.deps.Cfg.MQTTTopic))
	}
}

// --- step 5: the command router ---------------------------------------------

// TestRoutedCommandReachesTheWriteQueue proves the router replaces the
// hand-rolled dispatch without changing what a /set publish does: the
// mqtt key is the route's third wildcard, and the write still goes
// through the bounded drop-oldest queue rather than to Modbus inline.
func TestRoutedCommandReachesTheWriteQueue(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)
	c.topicBase.Store(topicParts{root: "MTEC", serial: "MTEC-TEST-001"})

	c.onCommand(context.Background(), publisher.Command{
		Topic:     "MTEC/MTEC-TEST-001/config/mode/set",
		Payload:   []byte("Eco"),
		Filter:    hass.CommandFilter("MTEC"),
		Wildcards: []string{"MTEC-TEST-001", "config", "mode"},
	})

	select {
	case req := <-c.writeQueue:
		if req.mqttKey != "mode" || req.value != "Eco" {
			t.Fatalf("queued %+v, want {mode Eco}", req)
		}
	default:
		t.Fatal("nothing was queued")
	}
}

// TestCommandForAnotherInvertersSerialIsDropped: one broker can carry
// several inverters, and a command addressed to a sibling's serial is not
// this daemon's to execute. The route's wildcard is what makes that
// checkable; the previous code compared a string prefix by hand.
func TestCommandForAnotherInvertersSerialIsDropped(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)
	c.topicBase.Store(topicParts{root: "MTEC", serial: "MTEC-TEST-001"})

	c.onCommand(context.Background(), publisher.Command{
		Topic:     "MTEC/OTHER-INVERTER/config/mode/set",
		Payload:   []byte("Eco"),
		Wildcards: []string{"OTHER-INVERTER", "config", "mode"},
	})
	select {
	case req := <-c.writeQueue:
		t.Fatalf("a command for another inverter was queued: %+v", req)
	default:
	}
}

// TestCommandBeforeStaticInitIsDropped: until the STATIC read yields a
// serial there is no topic tree to validate against, and executing a
// command aimed at a tree this daemon has not claimed is worse than
// losing it.
func TestCommandBeforeStaticInitIsDropped(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)
	c.onCommand(context.Background(), publisher.Command{
		Topic:     "MTEC/MTEC-TEST-001/config/mode/set",
		Payload:   []byte("Eco"),
		Wildcards: []string{"MTEC-TEST-001", "config", "mode"},
	})
	select {
	case req := <-c.writeQueue:
		t.Fatalf("a command was queued before static init: %+v", req)
	default:
	}
}

// TestRetainedCommandsAreNotDelivered pins the policy this daemon used to
// enforce with a hand-written check and now states as configuration.
//
// A retained delivery on a command topic is the broker replaying a past
// command on (re)subscribe, not a live request. Writing it to the
// inverter on every restart and reconnect would keep overriding settings
// the user has since changed — which reads from the outside like a device
// changing its own configuration.
func TestRetainedCommandsAreNotDelivered(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	c.topicBase.Store(topicParts{root: "MTEC", serial: "MTEC-TEST-001"})
	if err := c.installInboundHandler(context.Background()); err != nil {
		t.Fatalf("installInboundHandler: %v", err)
	}
	t.Cleanup(func() { c.stopCommands(context.Background()) })

	mqttStub.deliverMsg(&mqtt.Message{
		Topic:   "MTEC/MTEC-TEST-001/config/mode/set",
		Payload: []byte("Eco"),
		Retain:  true,
	})
	c.commands.WaitIdle()

	select {
	case req := <-c.writeQueue:
		t.Fatalf("a retained command was executed: %+v", req)
	default:
	}

	// The same publish, live, must still land — otherwise this test would
	// pass on a router that delivers nothing at all.
	mqttStub.deliver("MTEC/MTEC-TEST-001/config/mode/set", []byte("Eco"))
	c.commands.WaitIdle()
	select {
	case req := <-c.writeQueue:
		if req.mqttKey != "mode" {
			t.Fatalf("queued %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("a live command was not routed")
	}
}

// TestCheckDisjointRefusesASelfEcho is the guard this daemon had no
// equivalent of. It is asserted by making it fire: a command route that
// covers a topic this daemon publishes must fail the boot, because a
// self-echo turns a state publish into a command and the least
// diagnosable shape of that is a device that appears to change its own
// settings.
func TestCheckDisjointRefusesASelfEcho(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)
	c.serialNo = "MTEC-TEST-001"

	// The real wiring is disjoint.
	if err := c.checkCommandDisjoint(); err != nil {
		t.Fatalf("the shipped wiring is not disjoint: %v", err)
	}

	// A route that swallows the whole publish root is not.
	if err := c.commands.Handle(c.deps.Cfg.MQTTTopic+"/#", func(context.Context, publisher.Command) {}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := c.checkCommandDisjoint(); err == nil {
		t.Fatal("a route covering every state topic this daemon publishes was accepted")
	}
}

// TestCommandRouteAndFilterAreOneString: the subscription, the route and
// publisher.StateConfig.CommandFilters all read hass.CommandFilter. Three
// literals is how the guard and the thing it guards drift apart.
func TestCommandRouteAndFilterAreOneString(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	if err := c.installInboundHandler(context.Background()); err != nil {
		t.Fatalf("installInboundHandler: %v", err)
	}
	t.Cleanup(func() { c.stopCommands(context.Background()) })

	want := hass.CommandFilter(c.deps.Cfg.MQTTTopic)
	filters := c.commands.Filters()
	if len(filters) != 1 || filters[0] != want {
		t.Fatalf("router filters = %v, want [%q]", filters, want)
	}
	if mqttStub.countSubscribes(want) != 1 {
		t.Errorf("the router did not subscribe %q", want)
	}
}

// TestTwoInstancesOwnTheSameConfigTopics records what the sweep does when
// this daemon runs twice, against two inverters, on one broker — which
// the brief asked to be checked and said so either way.
//
// The answer is that the ownership predicates DO overlap, completely, and
// that this is not a regression: by default a unique_id carries no serial
// (HASS_UNIQUE_ID_INCLUDE_SERIAL is off), so two instances render the
// SAME 100 config topics with the same unique_ids. Their published sets
// are therefore identical, which is why neither judges the other's
// configs orphaned — and also why the two overwrite each other's
// `state_topic` on every boot, a pre-existing condition this step does
// not introduce and does not fix.
//
// The payload check is what keeps the sweep safe in the configuration
// that actually separates two instances: with serial-scoped unique_ids
// the config topics differ AND Discovery.IsOwnConfig additionally
// requires the state topic to sit under this inverter's own serial.
func TestTwoInstancesOwnTheSameConfigTopics(t *testing.T) {
	catalog, _, err := registers.LoadFromString(testCatalogYAML)
	if err != nil {
		t.Fatal(err)
	}
	build := func(serial string, scoped bool) *hass.Discovery {
		d := hass.New("homeassistant", "MTEC", catalog, "en", nil, "")
		d.IncludeSerialInUniqueIDs(scoped)
		d.Initialize(serial, "V1", "model")
		return d
	}

	// Default configuration: identical topics, so identical claims.
	a, b := build("SN-A", false), build("SN-B", false)
	topicsOf := func(d *hass.Discovery) map[string]bool {
		out := map[string]bool{}
		for _, e := range d.Entries() {
			out[e.ConfigTopic] = true
		}
		return out
	}
	ta, tb := topicsOf(a), topicsOf(b)
	if len(ta) == 0 || len(ta) != len(tb) {
		t.Fatalf("entry counts differ: %d vs %d", len(ta), len(tb))
	}
	for topic := range ta {
		if !tb[topic] {
			t.Fatalf("%q is published by one instance only; the default is meant to be identical", topic)
		}
		// Each instance's payload check accepts the OTHER's config too,
		// because neither carries a serial anywhere. Recorded, not
		// asserted as desirable.
		for _, e := range b.Entries() {
			if e.ConfigTopic == topic && !a.IsOwnConfig(e.Payload) {
				t.Errorf("instance A declines instance B's %q; the default configuration "+
					"is expected to make the two indistinguishable", topic)
			}
		}
	}

	// Serial-scoped: different topics, and each declines the other's body.
	sa, sb := build("SN-A", true), build("SN-B", true)
	for _, e := range sb.Entries() {
		if topicsOf(sa)[e.ConfigTopic] {
			t.Errorf("serial-scoped instances still share the config topic %q", e.ConfigTopic)
		}
		if sa.IsOwnConfig(e.Payload) {
			t.Errorf("serial-scoped instance A claims instance B's config %q — "+
				"its sweep would retract a live sibling's entity", e.ConfigTopic)
		}
	}
}

// TestEmptyStatePayloadIsRefusedByBothLayers records why the poll loop's
// own empty-payload guard (F8) is now belt AND braces, and asserts the
// braces.
//
// On the retained state plane an empty payload is MQTT's retraction: it
// deletes the entity's stored value rather than blanking it. The guard in
// publishGroupOnce predates this step and still earns its place — it
// names the offending topic in a warning, which an error return does not.
// But it is no longer the only thing standing there: the library refuses
// an empty retained state publish outright, with a sentinel that says to
// use Evict if a retraction was actually meant.
//
// This is stated explicitly because removing the poll loop's guard is a
// mutation the suite does NOT catch — and that is correct rather than a
// gap, since the wire outcome is identical either way. What would not be
// identical is the diagnosis, so both layers are kept and this test pins
// the one that is new.
func TestEmptyStatePayloadIsRefusedByBothLayers(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)

	_, err := c.deps.StatePlane.Publish(t.Context(), "MTEC/MTEC-TEST-001/now-base/x/state", nil)
	if !errors.Is(err, publisher.ErrEmptyStatePayload) {
		t.Fatalf("err = %v, want ErrEmptyStatePayload — an empty retained payload would "+
			"otherwise delete the entity's last known value", err)
	}
	if n := len(mqttStub.snapshotPublishes()); n != 0 {
		t.Errorf("the refused publish still put %d messages on the wire", n)
	}
}
