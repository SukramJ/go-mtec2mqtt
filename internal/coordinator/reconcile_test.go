// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"testing"
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

// TestOrphanTopics is the pure orphan-decision: from the retained configs the
// broker replays, only our own (IsOwnConfig) topics that are absent from the
// just-published set are flagged for clearing. Foreign configs, already-cleared
// (empty) topics and still-current entities are left untouched.
func TestOrphanTopics(t *testing.T) {
	c, _, _, _ := buildDeps(t, true)

	const (
		current = "homeassistant/sensor/MTEC_grid_power/config"
		orphan  = "homeassistant/sensor/MTEC_old_sensor/config"
		foreign = "homeassistant/sensor/zigbee2mqtt_0x1/config"
		cleared = "homeassistant/sensor/MTEC_already_gone/config"
	)
	retained := map[string][]byte{
		// Ours and still published — keep.
		current: []byte(`{"unique_id":"MTEC_grid_power","state_topic":"MTEC/SN/now-base/grid_power/state"}`),
		// Ours but no longer published — orphan to clear.
		orphan: []byte(`{"unique_id":"MTEC_old_sensor","state_topic":"MTEC/SN/now-base/old_sensor/state"}`),
		// Another integration — never touch.
		foreign: []byte(`{"unique_id":"zigbee2mqtt_0x1","state_topic":"zigbee2mqtt/0x1"}`),
		// Already cleared (empty retained payload) — skip.
		cleared: {},
	}
	published := map[string]bool{current: true}

	got := c.orphanTopics(retained, published)
	if len(got) != 1 || got[0] != orphan {
		t.Fatalf("orphanTopics = %v, want [%q]", got, orphan)
	}
}
