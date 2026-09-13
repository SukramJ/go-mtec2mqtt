// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sort"
	"testing"
	"time"
)

// TestPublishDiscoveryReturnsPublishedSet checks that publishDiscovery
// reports the full set of config topics it advertised, so the caller can
// hand it to reconcileOrphans. Every returned topic must also have been
// published to the broker with retain=true.
func TestPublishDiscoveryReturnsPublishedSet(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")

	published := c.publishDiscovery(context.Background())

	entries := c.deps.HASS.Entries()
	if len(entries) == 0 {
		t.Fatal("no discovery entries built")
	}
	if len(published) != len(entries) {
		t.Fatalf("published set size = %d, want %d (one per entry)", len(published), len(entries))
	}
	for _, e := range entries {
		if !published[e.ConfigTopic] {
			t.Errorf("published set missing %q", e.ConfigTopic)
		}
	}
	// A representative topic must also have gone out retained.
	const want = "homeassistant/select/MTEC_mode/config"
	if !published[want] {
		t.Errorf("published set missing %q", want)
	}
	retained := false
	for _, p := range mqttStub.snapshotPublishes() {
		if p.topic == want && p.retain {
			retained = true
			break
		}
	}
	if !retained {
		t.Errorf("%q was not published with retain=true", want)
	}
}

// TestDiscoveryRepublishIsDeduplicated is the gate publishing through
// publisher.Runtime buys, on the plane where it is least obvious.
//
// A Home Assistant birth triggers a full republish of every config. On a
// steady-state fleet every one of those payloads is byte-identical to the
// retained copy the broker already holds, and Home Assistant re-reads and
// re-validates each. The second pass must therefore write nothing.
func TestDiscoveryRepublishIsDeduplicated(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")

	c.publishDiscovery(context.Background())
	first := len(mqttStub.snapshotPublishes())
	if first == 0 {
		t.Fatal("the first pass published nothing")
	}

	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	published := c.publishDiscovery(context.Background())
	if n := len(mqttStub.snapshotPublishes()); n != 0 {
		t.Errorf("the republish wrote %d of %d configs again; the dedup gate is not engaged", n, first)
	}
	// The reported set is still the whole fleet: a deduped config is one
	// this process still claims, and dropping it from the set would make
	// the sweep judge it an orphan and delete it.
	if len(published) != first {
		t.Errorf("published set shrank to %d, want %d — the sweep would clear the difference",
			len(published), first)
	}
}

// sweepFixture drives one sweep pass end to end against the stub broker:
// it publishes discovery (which is what mints this process's claims), runs
// sweepOrphans, replays retained configs into the snapshot window, and
// returns the topics the pass retracted.
func sweepFixture(t *testing.T, retained map[string][]byte) []string {
	t.Helper()
	old := reconcileCollectWindow
	reconcileCollectWindow = 150 * time.Millisecond
	t.Cleanup(func() { reconcileCollectWindow = old })

	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")
	published := c.publishDiscovery(context.Background())

	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.sweepOrphans(context.Background(), published)
	}()

	// The window only sees what arrives after the subscribe lands.
	const snapshotFilter = "homeassistant/#"
	deadline := time.Now().Add(2 * time.Second)
	for {
		mqttStub.mu.Lock()
		_, up := mqttStub.handlers[snapshotFilter]
		mqttStub.mu.Unlock()
		if up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep never subscribed to %s", snapshotFilter)
		}
		time.Sleep(time.Millisecond)
	}
	for topic, body := range retained {
		mqttStub.deliver(topic, body)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweepOrphans did not finish")
	}

	var got []string
	for _, p := range mqttStub.snapshotPublishes() {
		if len(p.payload) == 0 && p.retain {
			got = append(got, p.topic)
		}
	}
	sort.Strings(got)
	return got
}

// TestSweepRetractsOnlyOurOwnOrphans is the safety property of the whole
// step, and every row is a population that exists on somebody's broker.
//
// A sweep that owns too much deletes another daemon's entities.
// openccu-loom's PR #817 is repairing exactly that: its retraction
// prefixes turned out to own 100 % of a sibling daemon's configs. So this
// asserts both directions at once — the one stale config of ours is
// cleared, and nothing else is touched.
func TestSweepRetractsOnlyOurOwnOrphans(t *testing.T) {
	const (
		current    = "homeassistant/select/MTEC_mode/config"
		orphan     = "homeassistant/sensor/MTEC_old_sensor/config"
		foreign    = "homeassistant/sensor/zigbee2mqtt_0x1/config"
		sibling    = "homeassistant/sensor/MTEC_grid_power/config"
		bundle     = "homeassistant/device/MTEC_MT1234567890/config"
		nodeID     = "homeassistant/sensor/node/MTEC_via_node/config"
		otherPlat  = "homeassistant/climate/MTEC_thermostat/config"
		alreadyOut = "homeassistant/sensor/MTEC_already_gone/config"
	)
	retracted := sweepFixture(t, map[string][]byte{
		// Ours and still published — keep.
		current: []byte(`{"unique_id":"MTEC_mode","state_topic":"MTEC/MTEC-TEST-001/config/mode/state"}`),
		// Ours, in our namespace, under our root, and no longer published.
		// The one topic this pass may clear.
		orphan: []byte(`{"unique_id":"MTEC_old_sensor","state_topic":"MTEC/MTEC-TEST-001/now-base/old_sensor/state"}`),
		// Another integration in the same shared discovery prefix.
		foreign: []byte(`{"unique_id":"zigbee2mqtt_0x1","state_topic":"zigbee2mqtt/0x1"}`),
		// The "MTEC_" namespace, but a state topic under a DIFFERENT MQTT
		// root: a second daemon whose operator changed MQTT_TOPIC. Declined
		// by the payload check, not by the topic check.
		sibling: []byte(`{"unique_id":"MTEC_grid_power","state_topic":"SOLAR/OTHER/now-base/grid_power/state"}`),
		// A device document — step 6's shape, and every Tasmota-style
		// writer's. Declined by the topic check.
		bundle: []byte(`{"dev":{"ids":["x"]},"cmps":{}}`),
		// The five-segment node-id form: not this fleet's.
		nodeID: []byte(`{"unique_id":"MTEC_via_node","state_topic":"MTEC/MTEC-TEST-001/now-base/x/state"}`),
		// A platform this daemon does not emit.
		otherPlat: []byte(`{"unique_id":"MTEC_thermostat","state_topic":"MTEC/MTEC-TEST-001/config/t/state"}`),
		// Already cleared: an empty retained payload is a topic the broker
		// is clearing already, and retracting it again is a message for
		// nothing.
		alreadyOut: {},
	})

	if len(retracted) != 1 || retracted[0] != orphan {
		t.Fatalf("the sweep retracted %v, want exactly [%q]", retracted, orphan)
	}
}

// TestSweepSparesAConfigThisProcessStillClaims is the failure mode the
// hand-rolled reconcile could not see and the library can.
//
// SweepResult.Owned lists EVERY owned topic the window delivered, the ones
// this process is publishing right now included. `Retract(res.Owned...)`
// is the composition that cleared 29 live configs in a sibling repo. The
// claim subtraction is what stands between this daemon and that.
func TestSweepSparesAConfigThisProcessStillClaims(t *testing.T) {
	const live = "homeassistant/select/MTEC_mode/config"
	retracted := sweepFixture(t, map[string][]byte{
		live: []byte(`{"unique_id":"MTEC_mode","state_topic":"MTEC/MTEC-TEST-001/config/mode/state"}`),
	})
	if len(retracted) != 0 {
		t.Fatalf("the sweep retracted %v — these are live configs this daemon publishes", retracted)
	}
}

// TestSweepSnapshotIsTakenDownAgain: the window is one subscription over
// the whole discovery prefix, and a client that replays its subscriptions
// on reconnect would carry a leaked one across every later broker restart,
// feeding a collector nobody reads for the process lifetime. The
// hand-rolled version retried the unsubscribe three times and then gave up
// loudly; the library takes it down on every exit path.
func TestSweepSnapshotIsTakenDownAgain(t *testing.T) {
	old := reconcileCollectWindow
	reconcileCollectWindow = 50 * time.Millisecond
	t.Cleanup(func() { reconcileCollectWindow = old })

	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")
	c.sweepOrphans(context.Background(), c.publishDiscovery(context.Background()))

	mqttStub.mu.Lock()
	_, still := mqttStub.handlers["homeassistant/#"]
	mqttStub.mu.Unlock()
	if mqttStub.countSubscribes("homeassistant/#") == 0 {
		t.Fatal("the sweep never opened a snapshot window")
	}
	if still {
		t.Error("the snapshot subscription survived the window; it would replay on every reconnect")
	}
}

// TestReportOnlySweepOverTheRealFleet is the report the brief asked for
// before the sweep was armed, kept as a test so it stays true.
//
// It drives the pass over the REAL registers.yaml — all 100 retained
// configs this daemon publishes — against a broker that also holds a
// realistic shared discovery tree: another integration, a sibling mtec
// instance under a different MQTT root, a device document, a five-segment
// node-id config, a platform this daemon does not emit, and one genuine
// leftover of its own.
//
// The two numbers that matter are printed, because "0 retracted" and "0
// inspected" look identical in a log line that reports only the second.
func TestReportOnlySweepOverTheRealFleet(t *testing.T) {
	old := reconcileCollectWindow
	reconcileCollectWindow = 200 * time.Millisecond
	t.Cleanup(func() { reconcileCollectWindow = old })

	c, discovery, mqttStub, _ := realTopicCoordinator(t)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	published := c.publishDiscovery(context.Background())
	if len(published) != 100 {
		t.Fatalf("published %d configs, want the real fleet's 100", len(published))
	}

	// The broker's retained discovery tree: this fleet, plus the company
	// it keeps.
	retained := map[string][]byte{}
	for _, e := range discovery.Entries() {
		retained[e.ConfigTopic] = e.Payload
	}
	const leftover = "homeassistant/sensor/MTEC_retired_sensor/config"
	extras := map[string][]byte{
		leftover: []byte(`{"unique_id":"MTEC_retired_sensor","state_topic":"MTEC/MT1234567890/now-base/retired_sensor/state"}`),
		"homeassistant/sensor/zigbee2mqtt_0x00124b/config":         []byte(`{"unique_id":"zigbee2mqtt_0x00124b","state_topic":"zigbee2mqtt/x"}`),
		"homeassistant/binary_sensor/tasmota_ABC123_status/config": []byte(`{"unique_id":"tasmota_ABC123_status","state_topic":"tasmota/x"}`),
		"homeassistant/sensor/MTEC_grid_power/config.other":        []byte(`{}`),
		"homeassistant/device/MTEC_OTHERSERIAL/config":             []byte(`{"dev":{"ids":["x"]},"cmps":{}}`),
		"homeassistant/sensor/mtecnode/MTEC_via_node/config":       []byte(`{"unique_id":"MTEC_via_node","state_topic":"MTEC/MT1234567890/now-base/x/state"}`),
		"homeassistant/climate/MTEC_thermostat/config":             []byte(`{"unique_id":"MTEC_thermostat","state_topic":"MTEC/MT1234567890/config/t/state"}`),
		// A sibling mtec instance whose operator changed MQTT_TOPIC: same
		// unique_id namespace, different publish root. Declined on the body.
		"homeassistant/sensor/MTEC_solar_power/config": []byte(`{"unique_id":"MTEC_solar_power","state_topic":"SOLAR/OTHERSERIAL/now-base/solar_power/state"}`),
	}
	for k, v := range extras {
		retained[k] = v
	}

	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.sweepOrphans(context.Background(), published)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mqttStub.mu.Lock()
		_, up := mqttStub.handlers["homeassistant/#"]
		mqttStub.mu.Unlock()
		if up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep never opened its window")
		}
		time.Sleep(time.Millisecond)
	}
	for topic, body := range retained {
		mqttStub.deliver(topic, body)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not finish")
	}

	var retracted []string
	for _, p := range mqttStub.snapshotPublishes() {
		if len(p.payload) == 0 && p.retain {
			retracted = append(retracted, p.topic)
		}
	}
	sort.Strings(retracted)
	t.Logf("report: %d retained configs offered, %d claimed by this daemon, %d would be retracted: %v",
		len(retained), len(published), len(retracted), retracted)

	if len(retracted) != 1 || retracted[0] != leftover {
		t.Fatalf("the sweep would retract %v, want exactly [%q] — every other row is "+
			"somebody else's entity", retracted, leftover)
	}
}

// TestSweepSparesAConfigWhoseOwnPublishFailed is one half of the claim
// subtraction, isolated so that removing either half is a failing test.
//
// The two claim sets are not the same set, and the difference is exactly
// this case: `published` names every config this boot's batch MINTED,
// including one whose publish the broker refused — an open circuit
// breaker, a momentary outage. Such a topic is NOT in the runtime's
// declared set, because the runtime records only what the broker
// accepted. Judging on the declared set alone would therefore retract an
// entity this daemon fully intends to publish, on the next poll, because
// one earlier write failed.
func TestSweepSparesAConfigWhoseOwnPublishFailed(t *testing.T) {
	old := reconcileCollectWindow
	reconcileCollectWindow = 150 * time.Millisecond
	t.Cleanup(func() { reconcileCollectWindow = old })

	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")

	// Every config publish of this boot fails, so `published` names all of
	// them and `Declared()` names none.
	mqttStub.mu.Lock()
	mqttStub.publishErr = errInjected{}
	mqttStub.mu.Unlock()
	published := c.publishDiscovery(context.Background())
	if len(published) == 0 {
		t.Fatal("no configs were advertised")
	}
	if n := len(c.deps.HARuntime.Declared()); n != 0 {
		t.Fatalf("the runtime declared %d configs despite every publish failing", n)
	}
	mqttStub.mu.Lock()
	mqttStub.publishErr = nil
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	retained := map[string][]byte{}
	for _, e := range c.deps.HASS.Entries() {
		retained[e.ConfigTopic] = e.Payload
	}
	if retracted := runSweep(t, c, mqttStub, published, retained); len(retracted) != 0 {
		t.Fatalf("the sweep retracted %v — a transient broker error must never clear an "+
			"entity this daemon still intends to publish", retracted)
	}
}

// TestSweepSparesADeclaredConfigOutsideThisBatch is the other half.
//
// `Declared()` is what the runtime has written for the whole PROCESS, and
// `published` is one batch. A batch that is transiently short — a report
// that shrank, a second pass that has not run yet — must not make the
// sweep delete everything the process still holds. Passing an empty batch
// is that case at its most extreme.
func TestSweepSparesADeclaredConfigOutsideThisBatch(t *testing.T) {
	old := reconcileCollectWindow
	reconcileCollectWindow = 150 * time.Millisecond
	t.Cleanup(func() { reconcileCollectWindow = old })

	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")
	c.publishDiscovery(context.Background())
	if len(c.deps.HARuntime.Declared()) == 0 {
		t.Fatal("the runtime declared nothing")
	}
	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	retained := map[string][]byte{}
	for _, e := range c.deps.HASS.Entries() {
		retained[e.ConfigTopic] = e.Payload
	}
	// An EMPTY batch, against a broker holding every live config.
	if retracted := runSweep(t, c, mqttStub, map[string]bool{}, retained); len(retracted) != 0 {
		t.Fatalf("an empty batch made the sweep retract %v — these are live configs "+
			"this process declared", retracted)
	}
}

// runSweep opens one window, replays retained and returns what was
// retracted. The shared body of the fixtures above.
func runSweep(t *testing.T, c *Coordinator, mqttStub *stubMQTT, published map[string]bool, retained map[string][]byte) []string {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.sweepOrphans(context.Background(), published)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mqttStub.mu.Lock()
		_, up := mqttStub.handlers["homeassistant/#"]
		mqttStub.mu.Unlock()
		if up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep never opened its window")
		}
		time.Sleep(time.Millisecond)
	}
	for topic, body := range retained {
		mqttStub.deliver(topic, body)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not finish")
	}
	var got []string
	for _, p := range mqttStub.snapshotPublishes() {
		if len(p.payload) == 0 && p.retain {
			got = append(got, p.topic)
		}
	}
	sort.Strings(got)
	return got
}
