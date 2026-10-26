// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"bytes"
	"errors"
	"sort"
	"testing"

	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	"github.com/SukramJ/go-hamqtt/publisher"
)

// TestRenderedBundleAcceptsTheDuplicatedUniqueIDs: nine unique_ids are
// published twice, under two platforms each, because a writable register
// also gets a read-only view. Home Assistant keys its registry on
// (domain, platform, unique_id), and so does discovery.Validate; a
// catalogue that ever emitted the SAME platform twice under one unique_id
// would be refused, correctly.
func TestRenderedBundleAcceptsTheDuplicatedUniqueIDs(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			bundle := realBundle(t, lang)
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

// TestLayoutRendersThisBridgesTopics pins the mqtt-smarthome 2.0 grammar
// `<name>/<function>/<item…>`, and that Slot.Bucket and Slot.Channel are
// inert for this bridge — the poll group rides in Slot.Path.
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
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	withBucket := s
	if l.State(withBucket) != l.State(s) {
		t.Errorf("BucketUnset changes the state topic: %q", l.State(withBucket))
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

// TestRenderedDeviceBlockIsTheBareSerial pins the device-registry key: the
// bare serial with no namespace. A namespaced identifier would re-key the
// device, leaving the old one behind with its area and name override.
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
	got := bundleBody(t, realBundle(t, "en"))["device"]
	if wb, gb := canonical(t, want), canonical(t, got); !bytes.Equal(wb, gb) {
		t.Fatalf("device block differs\n want: %s\n  got: %s", wb, gb)
	}
}

// TestLayoutStateTopicEqualsTheConfigsOwnStateTopic proves that the topic
// every config tells Home Assistant to read is the one the poll loop
// publishes to (StateTopic), for every component, in both languages.
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
				fromComponent, err := publisher.ComponentStateTopic(comp)
				if err != nil {
					t.Fatalf("%s declares no state_topic: %v", ent.Key(), err)
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
