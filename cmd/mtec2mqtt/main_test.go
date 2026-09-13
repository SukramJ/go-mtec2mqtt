// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"
	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/coordinator"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// failingPublisher always reports a broker-side failure so the breaker
// counts every publish against its threshold.
type failingPublisher struct{ calls int }

func (p *failingPublisher) Publish(context.Context, string, []byte, mqtt.QoS, bool, ...mqtt.PublishOption) error {
	p.calls++
	return mqtt.ErrNotConnected
}

// flakyStarter fails the first `failures` Start attempts, then
// succeeds — a broker that is still booting when the daemon starts.
type flakyStarter struct {
	failures int
	calls    int
}

func (s *flakyStarter) Start(context.Context) error {
	s.calls++
	if s.calls <= s.failures {
		return errors.New("injected: connection refused")
	}
	return nil
}

// TestStartMQTTRetriesUntilBrokerAccepts proves a transiently
// unavailable broker at boot (power-outage recovery) no longer kills
// the daemon: startMQTT keeps retrying until Start succeeds.
func TestStartMQTTRetriesUntilBrokerAccepts(t *testing.T) {
	t.Parallel()

	st := &flakyStarter{failures: 2}
	err := startMQTT(t.Context(), st, time.Millisecond, 4*time.Millisecond, discardLogger())
	if err != nil {
		t.Fatalf("startMQTT: %v, want success after retries", err)
	}
	if st.calls != 3 {
		t.Fatalf("starter saw %d attempts, want 3 (2 failures + 1 success)", st.calls)
	}
}

// TestStartMQTTStopsCleanlyOnCancel proves a shutdown signal during
// the connect-retry phase exits the loop with context.Canceled — and
// that main treats that as a clean stop, not a fatal error.
func TestStartMQTTStopsCleanlyOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // signal arrives while the broker is still unreachable
	st := &flakyStarter{failures: int(^uint(0) >> 1)}

	err := startMQTT(ctx, st, time.Millisecond, time.Millisecond, discardLogger())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startMQTT: %v, want context.Canceled", err)
	}
	if fatalErr(err) {
		t.Fatal("cancellation during startup must not be treated as fatal")
	}
}

// TestFatalErr pins the exit-code contract: only genuine failures are
// fatal; nil and (wrapped) context cancellation are clean stops.
func TestFatalErr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"wrapped canceled", fmt.Errorf("mtec2mqtt: mqtt start: %w", context.Canceled), false},
		{"real failure", errors.New("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		if got := fatalErr(tc.err); got != tc.want {
			t.Errorf("%s: fatalErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// recordingPublisher captures the last publish for assertion.
type recordingPublisher struct {
	calls   int
	topic   string
	payload string
	retain  bool
}

func (p *recordingPublisher) Publish(_ context.Context, topic string, payload []byte, _ mqtt.QoS, retain bool, _ ...mqtt.PublishOption) error {
	p.calls++
	p.topic, p.payload, p.retain = topic, string(payload), retain
	return nil
}

// TestWillIsTheRuntimesOwnStatement is what replaces the two
// announceAvailability tests this daemon used to carry.
//
// The birth and the will used to be two literals in run(): a topic
// string handed to mqtt.Will and the same string handed to a publish
// helper. They agreed, and "they agree" is precisely the property two
// sibling bridges failed to keep — their will's topic is referenced by no
// published entity, so a hard crash writes "offline" where nothing reads
// it and every entity stays available forever, showing the last value it
// ever saw.
//
// There is now one statement. publisher.Config takes the topic.Layout the
// discovery side renders from and derives the status topic from its
// Bridge(); Will() hands back the topic, the payload, the QoS and the
// retain flag; run() copies all four into mqtt.TCPConfig.Will without
// writing any of them down. This asserts the values that reach CONNECT.
//
// What it cannot assert is that run() uses them rather than a literal of
// its own — that is a property of eleven lines of wiring in a function
// that dials a broker. It is stated here so the gap is on the record.
func TestWillIsTheRuntimesOwnStatement(t *testing.T) {
	t.Parallel()

	rt := publisher.New(nopTransport{}, publisher.Config{
		Prefix: "homeassistant",
		Layout: hass.Layout{Root: "MTEC"},
		QoS:    coordinator.DiscoveryQoS,
		Logger: discardLogger(),
	})
	will, err := rt.Will()
	if err != nil {
		t.Fatalf("Will() = %v", err)
	}
	if will.Topic != hass.BridgeStatusTopic("MTEC") {
		t.Errorf("will topic = %q, want %q — the will and the 100 entities' "+
			"availability_topic must be one string", will.Topic, hass.BridgeStatusTopic("MTEC"))
	}
	if string(will.Payload) != hass.PayloadNotAvailable {
		t.Errorf("will payload = %q, want %q", will.Payload, hass.PayloadNotAvailable)
	}
	if !will.Retain {
		t.Error("will retain = false — an unretained marker tells nothing to a " +
			"Home Assistant that subscribes after the crash, which is exactly when it needs telling")
	}
	if mqtt.QoS(will.QoS) != mqtt.QoS0 {
		t.Errorf("will qos = %v, want QoS0 — unchanged from every previous release", will.QoS)
	}
	// The runtime derives the status topic from the Layout rather than
	// taking a literal, and refuses a disagreement. Asserted because it is
	// the property that makes the rest of this test more than a tautology.
	layout := hass.Layout{Root: "MTEC"}
	if rt.BridgeTopic() != layout.Bridge() {
		t.Errorf("BridgeTopic() = %q, Layout.Bridge() = %q", rt.BridgeTopic(), layout.Bridge())
	}
}

// TestLegacyFormsNamesTheMeasuredForm proves the composition root states
// the per-entity topic form this fleet is actually on.
//
// publisher.Config.LegacyEntityTopics REPLACES the library's five-segment
// default rather than adding to it, so saying nothing is a statement too —
// and the wrong one here. Step 3 measured the four-segment
// LegacyTopicByUniqueID reproducing 100 of 100 pinned config topics while
// the five-segment default reproduced 0. Getting it wrong in step 6 is
// silent and total: the bundle publishes, this module logs nothing, and
// Home Assistant refuses the document with one WARNING.
func TestLegacyFormsNamesTheMeasuredForm(t *testing.T) {
	t.Parallel()

	rt := publisher.New(nopTransport{}, publisher.Config{
		Prefix:             "homeassistant",
		Layout:             hass.Layout{Root: "MTEC"},
		QoS:                coordinator.DiscoveryQoS,
		LegacyEntityTopics: hass.LegacyConfigTopicForms(),
		Logger:             discardLogger(),
	})
	forms := rt.LegacyForms()
	if len(forms) != 1 || forms[0] != hass.LegacyConfigTopicForm {
		t.Fatalf("LegacyForms() = %v, want exactly [%s]", forms, hass.LegacyConfigTopicForm)
	}
}

// nopTransport is a publisher.Transport that reaches no broker. The two
// tests above read configuration back out of a Runtime; neither publishes.
type nopTransport struct{}

func (nopTransport) Publish(context.Context, string, []byte, byte, bool) error { return nil }
func (nopTransport) Subscribe(context.Context, string, byte, publisher.Handler) error {
	return nil
}
func (nopTransport) Unsubscribe(context.Context, string) error { return nil }

// TestBridgeStatusTopicIsNotInTheDiscoveryTree pins the string this daemon
// wills, births and buries itself on. Until this release it was
// "<hass_base>/status/lwt" — homeassistant/status/lwt by default, inside
// Home Assistant's own birth tree, one level under the topic this same
// daemon subscribes to. It is now the daemon's own tree, and it is read
// from the same function the discovery builder points 100 entities at, so
// the publisher and the declaration cannot drift.
func TestBridgeStatusTopicIsNotInTheDiscoveryTree(t *testing.T) {
	t.Parallel()

	const mqttRoot, hassBase = "MTEC", "homeassistant"
	got := hass.BridgeStatusTopic(mqttRoot)
	// t.Errorf, not t.Fatalf: the three assertions below used to sit after
	// a t.Fatalf and were therefore unreachable — the first one passing was
	// the only reason the rest ever ran, and a mutation that moved the
	// topic would have stopped at line one with the interesting assertions
	// never evaluated.
	if got != "MTEC/bridge/status" {
		t.Errorf("BridgeStatusTopic(%q) = %q, want MTEC/bridge/status", mqttRoot, got)
	}
	if got == hass.LegacyAvailabilityTopic(hassBase) {
		t.Error("the status topic is still in Home Assistant's own tree")
	}
	if strings.HasPrefix(got, hassBase+"/") {
		t.Errorf("%q is under the Home Assistant discovery prefix", got)
	}
	if !strings.HasPrefix(got, mqttRoot+"/") {
		t.Errorf("%q is not under the daemon's own publish root", got)
	}
}

// TestRetractLegacyAvailabilityClearsTheOldTopic proves the upgrade path:
// the retained "online" this daemon wrote to the old topic in every
// release up to 1.9.0 is cleared with an empty retained payload — MQTT's
// retraction — instead of being left to claim forever that a daemon which
// no longer publishes there is up.
func TestRetractLegacyAvailabilityClearsTheOldTopic(t *testing.T) {
	t.Parallel()

	// The topic comes from PRODUCTION, not from this test. It used to be
	// passed in as a literal and asserted against the same literal, which
	// tested argument forwarding and nothing else: mutating cmd's spelling
	// to "/status/LWT" left this suite green, and the retraction would have
	// cleared a topic no release ever wrote while the real stale "online"
	// sat there forever.
	// The PREFIX goes in and the topic is derived by production code. The
	// literal below is the one every release up to 1.9.0 actually wrote,
	// and it is the only literal this test states.
	cfg := &config.Config{HASSBaseTopic: "homeassistant"}
	const topic = "homeassistant/status/lwt"

	pub := &recordingPublisher{}
	retractLegacyAvailability(pub, cfg.HASSBaseTopic, discardLogger())(t.Context())

	if pub.calls != 1 {
		t.Fatalf("publisher saw %d calls, want 1", pub.calls)
	}
	if pub.topic != topic {
		t.Errorf("retracted %q, want %q", pub.topic, topic)
	}
	if pub.payload != "" {
		t.Errorf("payload = %q, want empty — only an empty payload retracts", pub.payload)
	}
	if !pub.retain {
		t.Error("retain = false — a non-retained empty payload leaves the stored message in place")
	}
}

// TestRetractLegacyAvailabilitySwallowsPublishError: like the birth
// publish, the retraction is best-effort and must never take the daemon
// down or stall a reconnect hook.
func TestRetractLegacyAvailabilitySwallowsPublishError(t *testing.T) {
	t.Parallel()

	retractLegacyAvailability(&failingPublisher{}, "homeassistant", discardLogger())(t.Context())
}

// TestTheLegacyAvailabilityTopicIsTrimmedLikeHomeAssistantTrimsIt is the
// same trailing-slash defect as the birth subscription's, on the other
// topic the discovery prefix is appended to. A HASS_BASE_TOPIC written
// "homeassistant/" must clear the topic the old release actually wrote,
// not a double-slashed sibling of it that holds nothing.
func TestTheLegacyAvailabilityTopicIsTrimmedLikeHomeAssistantTrimsIt(t *testing.T) {
	t.Parallel()

	for _, base := range []string{"homeassistant", "homeassistant/", "ha/disc/"} {
		want := strings.TrimRight(base, "/") + "/status/lwt"
		if got := hass.LegacyAvailabilityTopic(base); got != want {
			t.Errorf("LegacyAvailabilityTopic(%q) = %q, want %q", base, got, want)
		}
	}
}
