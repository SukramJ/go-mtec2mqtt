// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"testing"
	"time"
)

// TestPublishDiscoveryReturnsPublishedSet checks that publishDiscovery
// reports the set of config topics it claims, so the caller can hand it to
// reconcileOrphans.
//
// Since the move to the device bundle that set is exactly ONE topic —
// homeassistant/device/<serial>/config — where it used to be one per
// entity. The count is asserted rather than merely the membership, because
// a set that still carried the 100 per-entity topics would make the sweep
// SPARE the very configs this release exists to retract.
func TestPublishDiscoveryReturnsPublishedSet(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	initDiscovery(t, c)

	published := c.publishDiscovery(context.Background())

	want := "homeassistant/device/mtec-test-001/config"
	if len(published) != 1 || !published[want] {
		t.Fatalf("published set = %v, want exactly {%q}", published, want)
	}
	// The document itself went out retained, and the per-entity topics it
	// replaces were cleared with a retained EMPTY payload — MQTT's
	// retraction — rather than simply stopped being published.
	var doc, retractions int
	for _, p := range mqttStub.snapshotPublishes() {
		if !p.retain {
			t.Errorf("%q was published non-retained; a discovery config the broker "+
				"does not keep is gone the moment Home Assistant restarts", p.topic)
		}
		if p.topic == want {
			doc++
			if len(p.payload) == 0 {
				t.Error("the device document was published empty — that is a retraction")
			}
			continue
		}
		if len(p.payload) != 0 {
			t.Errorf("%q carried a payload; the only non-document write of a discovery "+
				"pass is a retraction", p.topic)
		}
		retractions++
	}
	if doc != 1 {
		t.Errorf("the device document was published %d times, want 1", doc)
	}
	if retractions != len(c.deps.HASS.Entries()) {
		t.Errorf("retracted %d per-entity topics, want %d (one per pre-migration entry)",
			retractions, len(c.deps.HASS.Entries()))
	}
}

// TestRetractionsPrecedeTheBundle is the ordering the whole release turns
// on, asserted on the wire rather than inferred from the library's
// documentation.
//
// Home Assistant refuses a device document while a per-entity config for
// one of its components' unique_ids is still retained, symmetrically, in
// silence, with one WARNING in its own log and no entities. So every
// retraction must be on the socket before the document is. At
// [DiscoveryQoS] — QoS 0 — there is no acknowledgement to wait for and
// this daemon does not pretend there is one: what it relies on is that the
// retractions are written first, on the same connection, and MQTT orders
// equal-QoS publishes from one client. This test pins the "written first"
// half; TestAFailedRetractionWithholdsTheBundle pins what happens when one
// of them does not go out at all.
func TestRetractionsPrecedeTheBundle(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	initDiscovery(t, c)
	c.publishDiscovery(context.Background())

	const doc = "homeassistant/device/mtec-test-001/config"
	pubs := mqttStub.snapshotPublishes()
	docAt := -1
	for i, p := range pubs {
		if p.topic == doc {
			docAt = i
			break
		}
	}
	if docAt < 0 {
		t.Fatal("the device document was never published")
	}
	// Every per-entity config the document supersedes must appear BEFORE
	// it, and the set must be complete — a single survivor is enough for
	// Home Assistant to refuse the whole device.
	want := map[string]bool{}
	for _, e := range c.deps.HASS.Entries() {
		want[e.ConfigTopic] = true
	}
	for _, p := range pubs[:docAt] {
		delete(want, p.topic)
	}
	if len(want) != 0 {
		t.Errorf("%d per-entity configs were not retracted before the document: %v",
			len(want), sortedKeys(want))
	}
	for _, p := range pubs[docAt+1:] {
		t.Errorf("%q was written after the document; a retraction that lands late "+
			"retracts nothing and leaves the conflict in place", p.topic)
	}
}

// TestAFailedRetractionWithholdsTheBundle is the dangerous window's guard.
//
// Between the retractions and the document the entities are ABSENT, not
// merely unavailable. Having cleared the old configs and then failed to
// write the new one is the one outcome worse than not having started, so a
// retraction the broker refuses must abort before the document rather than
// press on.
func TestAFailedRetractionWithholdsTheBundle(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	initDiscovery(t, c)

	mqttStub.setPublishErr(errInjected{})
	published := c.publishDiscovery(context.Background())

	const doc = "homeassistant/device/mtec-test-001/config"
	for _, p := range mqttStub.snapshotPublishes() {
		if p.topic == doc {
			t.Fatal("the document was published although a retraction failed")
		}
	}
	if c.discoverySent.Load() {
		t.Error("discovery was marked sent although nothing was published; " +
			"the republisher would never retry")
	}
	// The claim set still names the document, so the sweep that runs next
	// does not treat it as an orphan.
	if !published[doc] {
		t.Errorf("published set = %v, want it to still claim %q", published, doc)
	}
}

// TestTheCrashWindowHealsOnTheNextBoot answers the question a migration
// has to answer out loud: what if the daemon dies with the retractions out
// and the document not.
//
// The broker then holds no discovery config for this device at all and
// Home Assistant shows no entities. Nothing in this process can repair
// that, because the process is gone. What makes it survivable is that a
// fresh process starts with an empty superseded set and an empty declared
// set, so it re-runs the retraction (a no-op against topics the broker has
// already cleared) and then publishes the document. Asserted by building a
// second runtime over the SAME broker, which is what a restart is.
func TestTheCrashWindowHealsOnTheNextBoot(t *testing.T) {
	crashed, _, mqttStub, _ := buildDeps(t, true)
	crashed.deps.Logger = slog.New(slog.DiscardHandler)
	initDiscovery(t, crashed)

	// The retractions land; the document does not.
	mqttStub.setFailPublishTo("homeassistant/device/mtec-test-001/config", errInjected{})
	crashed.publishDiscovery(context.Background())
	const doc = "homeassistant/device/mtec-test-001/config"
	legacy := map[string]bool{}
	for _, e := range crashed.deps.HASS.Entries() {
		legacy[e.ConfigTopic] = true
	}
	retracted := map[string]bool{}
	for _, p := range mqttStub.snapshotPublishes() {
		if p.topic == doc {
			t.Fatal("the document was published although its write was refused")
		}
		if legacy[p.topic] && len(p.payload) == 0 {
			retracted[p.topic] = true
		}
	}
	if len(retracted) != len(legacy) {
		t.Fatalf("retracted %d topics, want the full %d — the fixture is not in the "+
			"crash window", len(retracted), len(legacy))
	}

	// The restart: a new process, so a new publisher.Runtime with an empty
	// superseded set and an empty declared set. Both halves matter — a
	// runtime that remembered the retractions would skip them, and one
	// that remembered the document would dedup it away.
	fresh, _, freshStub, _ := buildDeps(t, true)
	initDiscovery(t, fresh)
	fresh.publishDiscovery(context.Background())

	var document, redone int
	for _, p := range freshStub.snapshotPublishes() {
		if p.topic == doc && len(p.payload) > 0 {
			document++
			continue
		}
		if retracted[p.topic] && len(p.payload) == 0 {
			redone++
		}
	}
	if document != 1 {
		t.Error("the next boot did not publish the document; the crash window would " +
			"leave the device with no discovery config at all, forever")
	}
	if redone != len(retracted) {
		t.Errorf("the next boot re-retracted %d of %d topics; a boot that trusts a "+
			"previous process's retraction is a boot that publishes into a conflict",
			redone, len(retracted))
	}
	if !fresh.discoverySent.Load() {
		t.Error("the next boot did not mark discovery sent")
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
	initDiscovery(t, c)

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
	// The reported set still claims the document: a deduped config is one
	// this process still claims, and dropping it from the set would make
	// the sweep judge it an orphan and delete it.
	if !published["homeassistant/device/mtec-test-001/config"] {
		t.Errorf("published set = %v, want it to still claim the document", published)
	}
	// And the retractions are NOT repeated. After the first pass the broker
	// holds nothing at those topics, so a second round is a message for
	// nothing — and a boot that rewrote the document forty times would
	// otherwise send forty rounds of them.
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
	initDiscovery(t, c)
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
		bundle     = "homeassistant/device/mtec_mt1234567890/config"
		nodeID     = "homeassistant/sensor/node/MTEC_via_node/config"
		otherPlat  = "homeassistant/climate/MTEC_thermostat/config"
		alreadyOut = "homeassistant/sensor/MTEC_already_gone/config"
	)
	retracted := sweepFixture(t, map[string][]byte{
		// A per-entity config of the release being upgraded FROM. Since
		// the move to the device bundle this daemon publishes no
		// four-segment config at all, so this one is not "current" any
		// more: it is exactly what the migration must clear, and leaving
		// it retained is what makes Home Assistant refuse the document.
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

	want := []string{orphan, current}
	sort.Strings(want)
	if !slices.Equal(retracted, want) {
		t.Fatalf("the sweep retracted %v, want exactly %v", retracted, want)
	}
	// Said separately because it is the whole safety property: the six
	// declined rows are somebody else's entities, and the one that matters
	// most is the device document under ANOTHER serial. Two daemons against
	// two inverters each own a bundle topic keyed on their own serial, and a
	// sweep that claimed the bundle form would delete the other's fleet on
	// every boot. hass.OwnsConfigTopic declines the bundle form outright.
	for _, got := range retracted {
		if got == bundle {
			t.Fatal("the sweep retracted a device document; with the node id keyed on " +
				"the serial that is a sibling instance's whole fleet")
		}
	}
}

// TestSweepSparesAConfigThisProcessStillClaims is the failure mode the
// hand-rolled reconcile could not see and the library can.
//
// SweepResult.Owned lists EVERY owned topic the window delivered, the ones
// this process is publishing right now included. `Retract(res.Owned...)`
// is the composition that cleared 29 live configs in a sibling repo. The
// claim subtraction is what stands between this daemon and that.
//
// # Why the claim is minted by hand
//
// Since the move to the device bundle this daemon publishes exactly one
// config topic and it is of the bundle form, which hass.OwnsConfigTopic
// declines — so no config the window offers is ever claimed, and the
// subtraction is INERT in production today. That is a fact about this
// release, not a reason to delete the guard: it is the library-level net
// under publisher.Runtime.PublishComponent (the documented rollback
// direction) and under any future per-entity publish, and a guard that is
// deleted while it is inert is a guard nobody puts back. So the claim is
// minted directly on the runtime, which is what makes this test a test of
// the mechanism rather than of the current call graph.
func TestSweepSparesAConfigThisProcessStillClaims(t *testing.T) {
	old := reconcileCollectWindow
	reconcileCollectWindow = 150 * time.Millisecond
	t.Cleanup(func() { reconcileCollectWindow = old })

	const live = "homeassistant/select/MTEC_mode/config"
	body := []byte(`{"unique_id":"MTEC_mode","state_topic":"MTEC/MTEC-TEST-001/config/mode/state"}`)

	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	initDiscovery(t, c)
	if _, err := c.deps.HARuntime.Publish(context.Background(), live, body); err != nil {
		t.Fatalf("mint the claim: %v", err)
	}
	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	retracted := runSweep(t, c, mqttStub, map[string]bool{live: true}, map[string][]byte{live: body})
	if len(retracted) != 0 {
		t.Fatalf("the sweep retracted %v — these are live configs this daemon publishes", retracted)
	}
}

// TestSweepClearsTheLegacyConfigsTheBundleReplaces is the other side of
// the same coin and the reason the claim set had to shrink to one topic.
//
// publisher.Runtime.PublishBundle retracts the per-entity configs of the
// components the document carries; it cannot reach a config whose entity
// left the catalogue in an EARLIER release, because that entity is in no
// document to derive a topic from. Only the sweep can, and it can only do
// so because the published set no longer names the per-entity form.
func TestSweepClearsTheLegacyConfigsTheBundleReplaces(t *testing.T) {
	const (
		fromAnOlderRelease = "homeassistant/sensor/MTEC_withdrawn_in_1_8/config"
		document           = "homeassistant/device/mtec-test-001/config"
	)
	retracted := sweepFixture(t, map[string][]byte{
		fromAnOlderRelease: []byte(`{"unique_id":"MTEC_withdrawn_in_1_8","state_topic":"MTEC/MTEC-TEST-001/now-base/withdrawn/state"}`),
		document:           []byte(`{"dev":{"ids":["MTEC-TEST-001"]},"cmps":{}}`),
	})
	if !slices.Contains(retracted, fromAnOlderRelease) {
		t.Errorf("the sweep left %q retained; it re-creates a permanently unavailable "+
			"phantom entity on every MQTT-integration restart and nothing else can reach it",
			fromAnOlderRelease)
	}
	if slices.Contains(retracted, document) {
		t.Error("the sweep retracted this daemon's own device document")
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
	initDiscovery(t, c)
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
	const document = "homeassistant/device/mt1234567890/config"
	if len(published) != 1 || !published[document] {
		t.Fatalf("published set = %v, want exactly {%q}", published, document)
	}

	// The broker's retained discovery tree: this fleet, plus the company
	// it keeps.
	retained := map[string][]byte{}
	legacy := map[string]bool{}
	for _, e := range discovery.Entries() {
		retained[e.ConfigTopic] = e.Payload
		legacy[e.ConfigTopic] = true
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

	// Post-migration the answer is the whole legacy fleet plus the one
	// leftover: this daemon publishes no four-segment config any more, so
	// every one of them is superseded. In production PublishBundle has
	// already cleared the 100 by the time this window opens, and what is
	// left for the sweep is the residue — a config whose entity left the
	// catalogue in an earlier release, which no document can name.
	want := append(sortedKeys(legacy), leftover)
	sort.Strings(want)
	if !slices.Equal(retracted, want) {
		missing, extra := diffStrings(want, retracted)
		t.Fatalf("the sweep would retract %d topics, want %d\n missing: %v\n   extra: %v",
			len(retracted), len(want), missing, extra)
	}
	// The eight rows that are NOT this daemon's business must all survive,
	// and each is a population that exists on somebody's broker.
	for topic := range extras {
		if topic == leftover {
			continue
		}
		if slices.Contains(retracted, topic) {
			t.Errorf("the sweep would retract %q — that is somebody else's entity", topic)
		}
	}
}

// diffStrings reports what want has that got does not, and the reverse.
func diffStrings(want, got []string) (missing, extra []string) {
	in := map[string]bool{}
	for _, g := range got {
		in[g] = true
	}
	for _, w := range want {
		if !in[w] {
			missing = append(missing, w)
		}
	}
	has := map[string]bool{}
	for _, w := range want {
		has[w] = true
	}
	for _, g := range got {
		if !has[g] {
			extra = append(extra, g)
		}
	}
	return missing, extra
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
//
// Minted by hand for the reason given on
// TestSweepSparesAConfigThisProcessStillClaims: the production claim set
// is one bundle topic, which the sweep's ownership rule declines, so the
// only way to keep this half of the subtraction non-vacuous is to hand it
// a config of the form it actually judges.
func TestSweepSparesAConfigWhoseOwnPublishFailed(t *testing.T) {
	old := reconcileCollectWindow
	reconcileCollectWindow = 150 * time.Millisecond
	t.Cleanup(func() { reconcileCollectWindow = old })

	const refused = "homeassistant/sensor/MTEC_inverter/config"
	body := []byte(`{"unique_id":"MTEC_inverter","state_topic":"MTEC/MTEC-TEST-001/now-base/inverter/state"}`)

	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	initDiscovery(t, c)

	// The publish fails, so `published` names it and `Declared()` does not.
	mqttStub.setPublishErr(errInjected{})
	if _, err := c.deps.HARuntime.Publish(context.Background(), refused, body); err == nil {
		t.Fatal("the injected broker error did not reach the runtime")
	}
	mqttStub.setPublishErr(nil)
	if slices.Contains(c.deps.HARuntime.Declared(), refused) {
		t.Fatalf("the runtime declared %q despite the publish failing", refused)
	}
	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	if retracted := runSweep(t, c, mqttStub, map[string]bool{refused: true},
		map[string][]byte{refused: body}); len(retracted) != 0 {
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

	const declared = "homeassistant/sensor/MTEC_inverter/config"
	body := []byte(`{"unique_id":"MTEC_inverter","state_topic":"MTEC/MTEC-TEST-001/now-base/inverter/state"}`)

	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	initDiscovery(t, c)
	if _, err := c.deps.HARuntime.Publish(context.Background(), declared, body); err != nil {
		t.Fatalf("mint the claim: %v", err)
	}
	if !slices.Contains(c.deps.HARuntime.Declared(), declared) {
		t.Fatal("the runtime declared nothing")
	}
	mqttStub.mu.Lock()
	mqttStub.publishes = nil
	mqttStub.mu.Unlock()

	// An EMPTY batch, against a broker holding a config the process holds.
	if retracted := runSweep(t, c, mqttStub, map[string]bool{},
		map[string][]byte{declared: body}); len(retracted) != 0 {
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
