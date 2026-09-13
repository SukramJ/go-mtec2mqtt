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

// This file ties the six hamodel.BridgeOnly() call sites in hamqtt.go —
// sensorEntity, binarySensorEntity, numberEntity, selectEntity,
// switchEntity, virtualSwitchEntity — to one guard.
//
// # What goes wrong without it
//
// The zero hamodel.Availability is not "no availability": it resolves to
// {LevelBridge, LevelDevice} under mode `all`, and LevelDevice resolves
// through Layout.Availability to "<root>/<serial>/availability", a topic
// nothing in this daemon publishes. Under `all` Home Assistant requires
// every listed source to report `online`, so an entity that reached the
// default is permanently unavailable — with nothing on the wire and
// nothing in any log to say why. All six builders reaching it is all 100
// entities.
//
// # Why the existing tests were not enough
//
// Measured, not assumed. Dropping BridgeOnly from sensorEntity before this
// file existed failed five test functions — and every one of them was a
// comparison against a recorded artifact:
//
//   - TestBundleGolden, TestLibraryReproducesThePinnedPayloads and
//     TestTheMoveChangesOnlyTheTopicTheDeviceBlockAndTheOrigin compare
//     against testdata files, which -update-discovery-golden rewrites.
//   - TestLibraryReproducesTheShippedBytesExactly and
//     TestLibraryReproducesTheDeviceNameVariants compare against
//     Discovery.Entries, the shipped hand-built builder — which is the
//     scaffold ADR 0070 phase 6 exists to DELETE. They cannot outlive it.
//
// And TestAvailabilityTopicIsOutsideTheDiscoveryTree, the one assertion
// that states the property in its own words rather than by comparison,
// stayed GREEN under that mutation: it reads Discovery.Entries too, so it
// has never guarded this path at all.
//
// So the property was held entirely by scaffolding and by regenerable
// files. What follows states it directly, of the path that is going to
// survive.

// TestSixBuildersDeclareBridgeOnly is the six sites, named one by one, so
// a seventh builder added without the line is a compile-visible omission
// here rather than a silent payload change.
func TestSixBuildersDeclareBridgeOnly(t *testing.T) {
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
		{"sensorEntity", sensorEntity(goldenSerial, sensorReg)},
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
			if !slices.Equal(levels, []hamodel.AvailabilityLevel{hamodel.LevelBridge}) {
				t.Errorf("%s: availability resolves to %v, want exactly {LevelBridge} — "+
					"anything else names a topic this daemon does not publish and greys the entity out forever",
					tc.site, levels)
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

// TestRenderedFleetNamesOnlyThePublishedAvailabilityTopic asserts the whole
// rendered fleet against the PUBLISHING side's function, which is the
// crossing that makes this more than a tautology: a bridge-level source is
// rendered by Layout.Bridge and a device-level one by Layout.Availability —
// two different renderers — while BridgeStatusTopic is what main.go's LWT
// and the coordinator's online publish actually write.
func TestRenderedFleetNamesOnlyThePublishedAvailabilityTopic(t *testing.T) {
	const root = "MTEC"
	d := realDiscovery(t, "en")

	b, err := RenderBundle(d, BundleOrigin("test"))
	if err != nil {
		t.Fatalf("render bundle: %v", err)
	}
	got := discovery.BundleAvailabilityTopics(b)
	want := []string{BridgeStatusTopic(root)}
	if !slices.Equal(got, want) {
		t.Fatalf("the fleet names availability topics %v, want exactly %v", got, want)
	}
	if len(b.Components) != 100 {
		t.Fatalf("checked %d components, want 100", len(b.Components))
	}
}

// TestRenderRefusesAnAvailabilityTopicThisDaemonDoesNotPublish drives the
// failure the guard exists for, so the guard itself is known to be able to
// fail rather than assumed to be.
//
// It renders one entity that has lost its BridgeOnly — the exact mutation
// any of the six sites would suffer — through the real Layout, and asserts
// both render entry points refuse it and name the topic.
func TestRenderRefusesAnAvailabilityTopicThisDaemonDoesNotPublish(t *testing.T) {
	const root = "MTEC"
	d := realDiscovery(t, "en")

	ents := NewEntities(d)
	if len(ents) == 0 {
		t.Fatal("no entities")
	}
	ent, ok := ents[0].(*Entity)
	if !ok {
		t.Fatalf("entity 0 is %T, want *Entity", ents[0])
	}
	// The zero value, which is what dropping the BridgeOnly() line leaves
	// behind — not an explicitly device-level availability.
	ent.Description.Availability = hamodel.Availability{}

	comp, err := discovery.RenderComponent(NewRenderContext(d), NewDevice(d), ent, discovery.Origin{})
	if err != nil {
		t.Fatalf("render component: %v", err)
	}

	// The defaulted entity must actually have acquired a second source;
	// otherwise the check below would pass for the wrong reason.
	topics := discovery.AvailabilityTopics(comp)
	deviceTopic := Layout{Root: root}.Availability(hamodel.Slot{Address: goldenSerial})
	if !slices.Contains(topics, deviceTopic) {
		t.Fatalf("the defaulted entity names %v, expected it to include the unpublished %q", topics, deviceTopic)
	}

	err = discovery.CheckAvailability(PublishesAvailabilityTopic(root), comp)
	if err == nil {
		t.Fatal("CheckAvailability accepted an entity naming a topic this daemon never publishes")
	}
	if !errors.Is(err, discovery.ErrAvailabilityUnpublished) {
		t.Fatalf("error is %v, want ErrAvailabilityUnpublished", err)
	}
	// The message has to name the topic an operator would grep the broker
	// for; a guard that fails without saying what to look at is half a
	// guard.
	if !strings.Contains(err.Error(), deviceTopic) {
		t.Errorf("error %q does not name the offending topic %q", err, deviceTopic)
	}
	if !strings.Contains(err.Error(), comp.UniqueID) {
		t.Errorf("error %q does not name the offending entity %q", err, comp.UniqueID)
	}

	// And the predicate must not be vacuous: it accepts the one topic this
	// daemon writes and nothing else.
	publishes := PublishesAvailabilityTopic(root)
	if !publishes(BridgeStatusTopic(root)) {
		t.Error("the predicate rejects the bridge status topic, which this daemon does publish")
	}
	if publishes(deviceTopic) {
		t.Error("the predicate accepts the device availability topic, which nothing publishes")
	}
	if publishes("") {
		t.Error("the predicate accepts the empty topic")
	}
}
