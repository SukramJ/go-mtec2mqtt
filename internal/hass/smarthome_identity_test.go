// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSmartHomeMoveKeepsEveryHomeAssistantIdentity is the proof the
// 2.0.0 move (mqtt-smarthome 2.0, openccu-loom ADR 0083) owes every Home
// Assistant installation: nothing Home Assistant keys anything on changed.
//
// It compares the document this build renders against
// testdata/bundle_pre2_{en,de}.json — the device document 1.11.0
// published, copied verbatim from the bundle golden before this release
// regenerated it, and NEVER regenerated: no flag in this package writes
// it. Held identical, per language:
//
//   - the discovery topic, and with it the node id (the serial);
//   - the device block's `identifiers` — the device-registry key;
//   - the set of component keys;
//   - per component: `platform`, `unique_id` (the entity-registry key,
//     with platform) and `default_entity_id` (the entity id seed).
//
// Home Assistant has no migration for any of them (ADR 0068); it re-points
// an entity whose unique_id is unchanged to the new state and command
// topics on its own, which is why a topic move needs none.
func TestSmartHomeMoveKeepsEveryHomeAssistantIdentity(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "bundle_pre2_"+lang+".json")) //nolint:gosec // fixed fixture
			if err != nil {
				t.Fatal(err)
			}
			var pre goldenEntry
			if err := json.Unmarshal(raw, &pre); err != nil {
				t.Fatal(err)
			}

			d := realDiscovery(t, lang)
			b := realBundle(t, lang)
			body, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			var now map[string]any
			if err := json.Unmarshal(body, &now); err != nil {
				t.Fatal(err)
			}

			if got := BundleConfigTopic("homeassistant", d); got != pre.Topic {
				t.Errorf("discovery topic moved: %s, was %s", got, pre.Topic)
			}
			ids := func(doc map[string]any) []byte {
				dev, _ := doc["device"].(map[string]any)
				return canonical(t, dev["identifiers"])
			}
			if w, g := ids(pre.Payload), ids(now); !bytes.Equal(w, g) {
				t.Errorf("device identifiers moved: %s, were %s", g, w)
			}

			type identity struct{ Platform, UniqueID, DefaultEntityID any }
			index := func(doc map[string]any) map[string]identity {
				comps, _ := doc["components"].(map[string]any)
				out := make(map[string]identity, len(comps))
				for key, c := range comps {
					m, _ := c.(map[string]any)
					out[key] = identity{m["platform"], m["unique_id"], m["default_entity_id"]}
				}
				return out
			}
			was, is := index(pre.Payload), index(now)
			if len(was) != 100 || len(is) != len(was) {
				t.Fatalf("components: %d before, %d now, want 100 both", len(was), len(is))
			}
			for key, w := range was {
				g, ok := is[key]
				if !ok {
					t.Errorf("component %s is gone; its entity and history go with it", key)
					continue
				}
				if g != w {
					t.Errorf("component %s: identity %+v, was %+v", key, g, w)
				}
			}

			// And the move did happen: the same entities read the new topics.
			comps, _ := now["components"].(map[string]any)
			grid, _ := comps["sensor.grid_power"].(map[string]any)
			if got, want := grid["state_topic"], "MTEC/status/"+goldenSerial+"/now_base/grid_power"; got != want {
				t.Errorf("sensor.grid_power state_topic %v, want %v", got, want)
			}
		})
	}
}
