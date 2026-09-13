// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/SukramJ/go-mqtt"

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

// TestAnnounceAvailabilityPublishesRetained proves the birth/last-will
// companion publish is retained on the documented LWT topic, so the
// availability topic recovers to "online" after a reconnect instead
// of sticking at the will's "offline" forever.
func TestAnnounceAvailabilityPublishesRetained(t *testing.T) {
	t.Parallel()

	pub := &recordingPublisher{}
	announceAvailability(pub, "MTEC/status/lwt", "online", discardLogger())(t.Context())

	if pub.calls != 1 {
		t.Fatalf("publisher saw %d calls, want 1", pub.calls)
	}
	if pub.topic != "MTEC/status/lwt" || pub.payload != "online" || !pub.retain {
		t.Fatalf("got topic=%q payload=%q retain=%v, want MTEC/status/lwt/online/retained",
			pub.topic, pub.payload, pub.retain)
	}
}

// TestAnnounceAvailabilitySwallowsPublishError proves a failed
// availability publish is best-effort: logged, never panicking or
// propagating (it runs inside the reconnect hook and shutdown path).
func TestAnnounceAvailabilitySwallowsPublishError(t *testing.T) {
	t.Parallel()

	announceAvailability(&failingPublisher{}, "MTEC/status/lwt", "offline", discardLogger())(t.Context())
}

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
	if got != "MTEC/bridge/status" {
		t.Fatalf("BridgeStatusTopic(%q) = %q, want MTEC/bridge/status", mqttRoot, got)
	}
	if got == hassBase+"/status/lwt" {
		t.Fatal("the status topic is still in Home Assistant's own tree")
	}
	if len(got) <= len(mqttRoot) || got[:len(mqttRoot)+1] != mqttRoot+"/" {
		t.Fatalf("%q is not under the daemon's own publish root", got)
	}
}

// TestRetractLegacyAvailabilityClearsTheOldTopic proves the upgrade path:
// the retained "online" this daemon wrote to the old topic in every
// release up to 1.9.0 is cleared with an empty retained payload — MQTT's
// retraction — instead of being left to claim forever that a daemon which
// no longer publishes there is up.
func TestRetractLegacyAvailabilityClearsTheOldTopic(t *testing.T) {
	t.Parallel()

	pub := &recordingPublisher{}
	retractLegacyAvailability(pub, "homeassistant/status/lwt", discardLogger())(t.Context())

	if pub.calls != 1 {
		t.Fatalf("publisher saw %d calls, want 1", pub.calls)
	}
	if pub.topic != "homeassistant/status/lwt" {
		t.Errorf("retracted %q, want homeassistant/status/lwt", pub.topic)
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

	retractLegacyAvailability(&failingPublisher{}, "homeassistant/status/lwt", discardLogger())(t.Context())
}
