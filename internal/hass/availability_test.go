// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// This file ties the six bridgeAndDevice() call sites in hamqtt.go —
// sensorEntity, binarySensorEntity, numberEntity, selectEntity,
// switchEntity, virtualSwitchEntity — to one guard.
//
// # What goes wrong without it
//
// Under `availability_mode: all` Home Assistant requires every listed
// source to report available. Since 2.0.0 every entity names two:
// `<name>/connected` (available at 2) and the inverter's
// `<name>/status/<serial>/online`. An entity that names a third source
// nothing publishes, or loses one of the two, is either permanently
// unavailable or available while the inverter is gone — with nothing on
// the wire and nothing in any log to say why. All six builders drifting is
// all 100 entities.
//
// Up to 1.11 the same file pinned the opposite, bridge-only availability,
// because nothing published a device-level topic then.

// TestSixBuildersDeclareBridgeAndDevice is the six sites, named one by
// one, so a seventh builder added without the line is a compile-visible
// omission here rather than a silent payload change.
func TestSixBuildersDeclareBridgeAndDevice(t *testing.T) {
	d := realDiscovery(t, "en")

	// One real register per platform, taken from the catalog rather than
	// invented, so each builder is called the way NewEntities calls it.
	pick := func(want Platform) *registers.Register {
		t.Helper()
		for _, r := range d.catalog.All {
			if r.Group == "" || !r.HasHassHints() {
				continue
			}
			got := PlatformSensor
			if r.HassComponentType != "" {
				got = Platform(r.HassComponentType)
			}
			if got == want {
				return r
			}
		}
		t.Fatalf("no register in the catalog builds a %q entity", want)
		return nil
	}

	sensorReg := pick(PlatformSensor)
	numberReg := pick(PlatformNumber)
	selectReg := pick(PlatformSelect)
	switchReg := pick(PlatformSwitch)

	cases := []struct {
		site   string
		entity *Entity
	}{
		{"sensorEntity", sensorEntity(goldenSerial, sensorReg, "en")},
		// binarySensorEntity is built from a switch register, exactly as
		// NewEntities pairs them.
		{"binarySensorEntity", binarySensorEntity(goldenSerial, switchReg)},
		{"numberEntity", numberEntity(goldenSerial, numberReg)},
		{"selectEntity", selectEntity(goldenSerial, selectReg)},
		{"switchEntity", switchEntity(goldenSerial, switchReg)},
		{"virtualSwitchEntity", virtualSwitchEntity(goldenSerial, DefaultVirtualSwitches(50, 50)[0])},
	}
	if len(cases) != 6 {
		t.Fatalf("this test claims to cover six builders, covers %d", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.site, func(t *testing.T) {
			levels, mode := tc.entity.Description.Availability.Resolved()
			want := []hamodel.AvailabilityLevel{hamodel.LevelBridge, hamodel.LevelDevice}
			if !slices.Equal(levels, want) {
				t.Errorf("%s: availability resolves to %v, want exactly %v — "+
					"`connected` and the inverter's `online`, the two topics this daemon publishes",
					tc.site, levels, want)
			}
			// Asserted so the test cannot pass because the mode changed
			// underneath it: `all` is what makes an unwritten source fatal
			// rather than ignored.
			if mode != hamodel.AvailabilityAll {
				t.Errorf("%s: availability mode is %q, want %q", tc.site, mode, hamodel.AvailabilityAll)
			}
		})
	}
}

// TestRenderedFleetNamesOnlyThePublishedAvailabilityTopics asserts the
// whole rendered fleet against the PUBLISHING side's functions, which is
// the crossing that makes this more than a tautology: the sources are
// rendered by Layout.Bridge and Layout.Availability, while ConnectedTopic
// and OnlineTopic are what the runtime's will and the coordinator's online
// item actually write.
func TestRenderedFleetNamesOnlyThePublishedAvailabilityTopics(t *testing.T) {
	const root = "MTEC"
	d := realDiscovery(t, "en")

	b, err := RenderBundle(d, BundleOrigin("test"))
	if err != nil {
		t.Fatalf("render bundle: %v", err)
	}
	got := discovery.BundleAvailabilityTopics(b)
	want := []string{ConnectedTopic(root), OnlineTopic(root, goldenSerial)}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the fleet names availability topics %v, want exactly %v", got, want)
	}
	if want[0] != "MTEC/connected" || want[1] != "MTEC/status/"+goldenSerial+"/online" {
		t.Fatalf("availability topics %v are not the mqtt-smarthome 2.0 ones", want)
	}
	if len(b.Components) != 100 {
		t.Fatalf("checked %d components, want 100", len(b.Components))
	}
}

// TestRenderRefusesAnAvailabilityTopicThisDaemonDoesNotPublish drives the
// failure the guard exists for, so the guard itself is known to be able to
// fail rather than assumed to be: an entity whose device-level source names
// an inverter this daemon does not publish `online` for.
func TestRenderRefusesAnAvailabilityTopicThisDaemonDoesNotPublish(t *testing.T) {
	const root = "MTEC"
	d := realDiscovery(t, "en")

	ents := NewEntities(d)
	if len(ents) == 0 {
		t.Fatal("no entities")
	}
	comp, err := discovery.RenderComponent(NewRenderContext(d), NewDevice(d), ents[0], discovery.Origin{})
	if err != nil {
		t.Fatalf("render component: %v", err)
	}
	onlineTopic := OnlineTopic(root, goldenSerial)
	if !slices.Contains(discovery.AvailabilityTopics(comp), onlineTopic) {
		t.Fatalf("the entity names %v, expected it to include %q", discovery.AvailabilityTopics(comp), onlineTopic)
	}

	// The predicate of a daemon that knows another serial: the entity's
	// device source is a topic it does not publish.
	err = discovery.CheckAvailability(PublishesAvailabilityTopic(root, "OTHER0000"), comp)
	if err == nil {
		t.Fatal("CheckAvailability accepted an entity naming a topic this daemon never publishes")
	}
	if !errors.Is(err, discovery.ErrAvailabilityUnpublished) {
		t.Fatalf("error is %v, want ErrAvailabilityUnpublished", err)
	}
	// The message has to name the topic an operator would grep the broker
	// for; a guard that fails without saying what to look at is half a
	// guard.
	if !strings.Contains(err.Error(), onlineTopic) {
		t.Errorf("error %q does not name the offending topic %q", err, onlineTopic)
	}
	if !strings.Contains(err.Error(), comp.UniqueID) {
		t.Errorf("error %q does not name the offending entity %q", err, comp.UniqueID)
	}

	// And the predicate must not be vacuous: it accepts the two topics this
	// daemon writes and nothing else — before the STATIC read only the
	// first.
	publishes := PublishesAvailabilityTopic(root, goldenSerial)
	if !publishes(ConnectedTopic(root)) || !publishes(onlineTopic) {
		t.Error("the predicate rejects a topic this daemon does publish")
	}
	for _, other := range []string{"", LegacyBridgeStatusTopic(root), "MTEC/" + goldenSerial + "/availability"} {
		if publishes(other) {
			t.Errorf("the predicate accepts %q, which nothing publishes", other)
		}
	}
	if PublishesAvailabilityTopic(root, "")(OnlineTopic(root, "")) {
		t.Error("before the serial is known the predicate claims an online topic")
	}
}
