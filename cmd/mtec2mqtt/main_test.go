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

// recordingSubscriber captures Subscribe/Unsubscribe filters so the
// test can prove the session delegates them to the raw client.
type recordingSubscriber struct {
	subscribed   []string
	unsubscribed []string
}

func (s *recordingSubscriber) Subscribe(_ context.Context, filter string, _ mqtt.QoS, _ mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	s.subscribed = append(s.subscribed, filter)
	return mqtt.SubscribeResult{}, nil
}

func (s *recordingSubscriber) Unsubscribe(_ context.Context, filter string) error {
	s.unsubscribed = append(s.unsubscribed, filter)
	return nil
}

// TestMQTTSessionPublishIsCircuitGated proves the coordinator-facing
// session routes Publish through the breaker: once the failure
// threshold is reached, publishes fail fast with ErrCircuitOpen and no
// longer hit the underlying client.
func TestMQTTSessionPublishIsCircuitGated(t *testing.T) {
	t.Parallel()

	pub := &failingPublisher{}
	session := &mqttSession{
		Breaker: mqtt.NewBreaker(pub, mqtt.BreakerConfig{
			FailureThreshold: 1,
		}),
		MQTTSubscriber: &recordingSubscriber{},
	}

	err := session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)
	if !errors.Is(err, mqtt.ErrNotConnected) {
		t.Fatalf("first publish: got %v, want ErrNotConnected", err)
	}
	err = session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)
	if !errors.Is(err, mqtt.ErrCircuitOpen) {
		t.Fatalf("second publish: got %v, want ErrCircuitOpen", err)
	}
	if pub.calls != 1 {
		t.Fatalf("underlying publisher saw %d calls, want 1 (open circuit must fail fast)", pub.calls)
	}
}

// TestMQTTSessionSubscribeBypassesBreaker proves subscriptions are not
// affected by the publish-side circuit state.
func TestMQTTSessionSubscribeBypassesBreaker(t *testing.T) {
	t.Parallel()

	sub := &recordingSubscriber{}
	session := &mqttSession{
		Breaker:        mqtt.NewBreaker(&failingPublisher{}, mqtt.BreakerConfig{FailureThreshold: 1}),
		MQTTSubscriber: sub,
	}

	// Trip the circuit open on the publish side.
	_ = session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)
	_ = session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)

	if _, err := session.Subscribe(t.Context(), "cmd/#", mqtt.QoS1, func(*mqtt.Message) {}); err != nil {
		t.Fatalf("subscribe with open circuit: %v", err)
	}
	if err := session.Unsubscribe(t.Context(), "cmd/#"); err != nil {
		t.Fatalf("unsubscribe with open circuit: %v", err)
	}
	if len(sub.subscribed) != 1 || sub.subscribed[0] != "cmd/#" {
		t.Fatalf("subscriber saw %v, want [cmd/#]", sub.subscribed)
	}
	if len(sub.unsubscribed) != 1 || sub.unsubscribed[0] != "cmd/#" {
		t.Fatalf("unsubscriber saw %v, want [cmd/#]", sub.unsubscribed)
	}
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
