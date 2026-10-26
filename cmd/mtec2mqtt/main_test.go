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
		Layout: hass.NewLayout("MTEC"),
		QoS:    coordinator.DiscoveryQoS,
		Logger: discardLogger(),
	})
	will, err := rt.Will()
	if err != nil {
		t.Fatalf("Will() = %v", err)
	}
	if will.Topic != hass.ConnectedTopic("MTEC") || will.Topic != "MTEC/connected" {
		t.Errorf("will topic = %q, want %q — the will and the 100 entities' "+
			"availability_topic must be one string", will.Topic, hass.ConnectedTopic("MTEC"))
	}
	// mqtt-smarthome 2.0 §3.1: the will sets `connected` to 0.
	if string(will.Payload) != publisher.ConnectedPayloadDown {
		t.Errorf("will payload = %q, want %q", will.Payload, "0")
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
	layout := hass.NewLayout("MTEC")
	if rt.BridgeTopic() != layout.Bridge() {
		t.Errorf("BridgeTopic() = %q, Layout.Bridge() = %q", rt.BridgeTopic(), layout.Bridge())
	}
}

// nopTransport is a publisher.Transport that reaches no broker. The test
// above reads configuration back out of a Runtime; it publishes nothing.
type nopTransport struct{}

func (nopTransport) Publish(context.Context, string, []byte, byte, bool) error { return nil }
func (nopTransport) Subscribe(context.Context, string, byte, publisher.Handler) error {
	return nil
}
func (nopTransport) Unsubscribe(context.Context, string) error { return nil }

// TestConnectedTopicIsNotInTheDiscoveryTree pins the string this daemon
// wills, announces and buries itself on: `<name>/connected` since 2.0.0
// (mqtt-smarthome 2.0), `<root>/bridge/status` in 1.10 and 1.11, and
// "<hass_base>/status/lwt" before that — inside Home Assistant's own birth
// tree, one level under the topic this same daemon subscribes to. It is in
// the daemon's own tree, and it is read from the same function the
// discovery builder points 100 entities at, so the publisher and the
// declaration cannot drift.
func TestConnectedTopicIsNotInTheDiscoveryTree(t *testing.T) {
	t.Parallel()

	const mqttRoot, hassBase = "MTEC", "homeassistant"
	got := hass.ConnectedTopic(mqttRoot)
	if got != "MTEC/connected" {
		t.Errorf("ConnectedTopic(%q) = %q, want MTEC/connected", mqttRoot, got)
	}
	if got == hassBase+"/status/lwt" || got == mqttRoot+"/bridge/status" {
		t.Error("the status topic is still one of the pre-2.0 ones")
	}
	if strings.HasPrefix(got, hassBase+"/") {
		t.Errorf("%q is under the Home Assistant discovery prefix", got)
	}
	if !strings.HasPrefix(got, mqttRoot+"/") {
		t.Errorf("%q is not under the daemon's own publish root", got)
	}
}

// TestInstanceInfoCarriesTheProjectFields pins the project's own
// `<name>/info` fields beside the ones the library answers.
func TestInstanceInfoCarriesTheProjectFields(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{ModbusIP: "192.168.1.5", ModbusPort: 5743, HASSEnable: true}
	got := instanceInfo(cfg)
	if got["modbus"] != "192.168.1.5:5743" || got["ha_discovery"] != true || len(got) != 2 {
		t.Errorf("instanceInfo = %v", got)
	}
}
