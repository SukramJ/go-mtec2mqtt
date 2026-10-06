// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"
	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
)

// This file covers what 2.0.0 adds for mqtt-smarthome 2.0 (openccu-loom
// ADR 0083): `<name>/connected` and the inverter's `online` item following
// the Modbus link, the `set` conversions of §5.3, the maintenance routes,
// `<name>/info`, and the start-up sweep of the pre-2.0 topic layout.

const testSerial = "MTEC-TEST-001"

// publishedOn returns the payloads published to topic, in order.
func publishedOn(stub *stubMQTT, topic string) []string {
	var out []string
	for _, p := range stub.snapshotPublishes() {
		if p.topic == topic {
			out = append(out, string(p.payload))
		}
	}
	return out
}

func clearPublishes(stub *stubMQTT) {
	stub.mu.Lock()
	stub.publishes = nil
	stub.mu.Unlock()
}

// statusVals decodes the `val` of every status object published to topic.
func statusVals(t *testing.T, stub *stubMQTT, topic string) []any {
	t.Helper()
	raws := publishedOn(stub, topic)
	out := make([]any, 0, len(raws))
	for _, raw := range raws {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			t.Fatalf("%s: %q is not a status object: %v", topic, raw, err)
		}
		out = append(out, obj["val"])
	}
	return out
}

// TestConnectedFollowsTheInverter: `<name>/connected` is 2 while the
// Modbus link is up and 1 while it is not, published on every transition
// and only then; the device's `online` item says the same as a boolean.
func TestConnectedFollowsTheInverter(t *testing.T) {
	c, _, stub, mb := buildDeps(t, false)
	c.topicBase.Store(topicParts{root: "MTEC", serial: testSerial})
	ctx := t.Context()

	for _, up := range []bool{true, true, false, false, true} {
		mb.connected.Store(up)
		c.updateUpstream(ctx)
	}

	if got, want := publishedOn(stub, "MTEC/connected"), []string{"2", "1", "2"}; !slices.Equal(got, want) {
		t.Errorf("connected = %v, want %v (transitions only)", got, want)
	}
	if got, want := statusVals(t, stub, "MTEC/status/"+testSerial+"/online"), []any{true, false, true}; !slices.Equal(got, want) {
		t.Errorf("online = %v, want %v (changes only)", got, want)
	}
	for _, p := range stub.snapshotPublishes() {
		if !p.retain {
			t.Errorf("%s published without retain", p.topic)
		}
	}
}

// TestReconnectAnnouncesTheCurrentLevelOnce: a reconnect writes the level
// the inverter is at, as ONE message — a 1 that a 2 then overwrites would
// flicker every entity unavailable on each reconnect.
func TestReconnectAnnouncesTheCurrentLevelOnce(t *testing.T) {
	for _, tc := range []struct {
		up   bool
		want string
	}{{true, "2"}, {false, "1"}} {
		c, _, stub, mb := buildDeps(t, false)
		mb.connected.Store(tc.up)
		c.updateUpstream(t.Context())
		clearPublishes(stub)

		c.PublishOnline(t.Context())
		if got := publishedOn(stub, "MTEC/connected"); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("inverter up=%v: reconnect published connected %v, want [%s]", tc.up, got, tc.want)
		}
	}
}

// TestPublishOnlineAnnouncesInfo: `<name>/info` on every broker connect,
// retained, naming the Go project rather than an npm package.
func TestPublishOnlineAnnouncesInfo(t *testing.T) {
	c, _, stub, _ := buildDeps(t, false)
	c.PublishOnline(t.Context())

	infos := publishedOn(stub, "MTEC/info")
	if len(infos) != 1 {
		t.Fatalf("info published %d times, want 1", len(infos))
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(infos[0]), &info); err != nil {
		t.Fatal(err)
	}
	if info["name"] != "go-mtec2mqtt" || info["spec"] != "2.0" || info["maintenance"] != true {
		t.Errorf("info = %v", info)
	}
}

const setCatalogYAML = testCatalogYAML + `
"25100":
  name: Grid injection limit switch
  length: 1
  type: U16
  writable: true
  mqtt: grid_inject_switch
  group: config
  hass_component_type: switch
  hass_payload_on: "1"
  hass_payload_off: "0"

"50000":
  name: Inverter operation mode
  length: 1
  type: U16
  writable: true
  mqtt: inverter_mode
  group: now_base
  hass_component_type: select
  hass_device_class: enum
  hass_value_items:
    257: "General mode"
    258: "Economic mode"
  hass_value_items_de:
    257: "Allgemeiner Modus"
    258: "Sparmodus"

"52601":
  name: Charge limit
  length: 1
  type: U16
  writable: true
  mqtt: charge_limit
  group: config
  hass_component_type: number
`

// TestSetAppliesTheSmartHomeConversions: `set` accepts what it accepted
// before plus the §5.3 spellings — `{"val": …}`, booleans as
// true/false/1/0/on/off/yes/no in any case, enum tokens in any case — and
// hands the write path a value it understands.
func TestSetAppliesTheSmartHomeConversions(t *testing.T) {
	virtual := hass.DefaultVirtualSwitches(50, 50)
	c, _, _, _ := buildDepsWith(t, false, setCatalogYAML, virtual)
	c.topicBase.Store(topicParts{root: "MTEC", serial: testSerial})

	cases := []struct {
		group, key, payload, want string
	}{
		{"config", "grid_inject_switch", "true", "1"},
		{"config", "grid_inject_switch", "OFF", "0"},
		{"config", "grid_inject_switch", "yes", "1"},
		{"config", "grid_inject_switch", `{"val": false}`, "0"},
		{"config", "grid_inject_switch", "1", "1"},
		{"config", "charge_active", "On", "1"},
		{"config", "charge_active", "no", "0"},
		// What the write path accepted before still reaches it unchanged.
		{"config", "charge_active", "5", "5"},
		{"now_base", "inverter_mode", "general MODE", "General mode"},
		{"now_base", "inverter_mode", `{"val":"Economic mode"}`, "Economic mode"},
		{"now_base", "inverter_mode", "Sparmodus", "Economic mode"},
		{"now_base", "inverter_mode", "258", "258"},
		{"config", "charge_limit", "42.5", "42.5"},
		{"config", "charge_limit", `{"val": 40}`, "40"},
	}
	for _, tc := range cases {
		routeSet(t, c, publisher.Command{
			Topic:     hass.CommandTopic("MTEC", testSerial, tc.group, tc.key),
			Payload:   []byte(tc.payload),
			Filter:    hass.CommandFilter("MTEC"),
			Wildcards: []string{testSerial, tc.group, tc.key},
		})
		select {
		case req := <-c.writeQueue:
			if req.mqttKey != tc.key || req.value != tc.want {
				t.Errorf("%s %q: queued %s=%q, want %q", tc.key, tc.payload, req.mqttKey, req.value, tc.want)
			}
			if req.topic != hass.CommandTopic("MTEC", testSerial, tc.group, tc.key) || req.payload != tc.payload {
				t.Errorf("%s %q: the request lost its topic or payload: %+v", tc.key, tc.payload, req)
			}
		default:
			t.Errorf("%s %q: nothing queued", tc.key, tc.payload)
		}
	}
}

// syncBuffer is a log sink safe for the router's worker goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRejectedAndFailedSetsAreLoggedWithTopicAndPayload is spec §3.3's
// MUST: a request this daemon will not execute is logged at warn with its
// topic and payload, whether it is refused before the write or the write
// itself fails.
func TestRejectedAndFailedSetsAreLoggedWithTopicAndPayload(t *testing.T) {
	c, reader, _, _ := buildDepsWith(t, false, setCatalogYAML, nil)
	var logs syncBuffer
	c.deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	c.topicBase.Store(topicParts{root: "MTEC", serial: testSerial})

	topic := hass.CommandTopic("MTEC", testSerial, "config", "charge_limit")
	routeSet(t, c, publisher.Command{
		Topic: topic, Payload: []byte(`{"limit": 3}`),
		Wildcards: []string{testSerial, "config", "charge_limit"},
	})
	select {
	case req := <-c.writeQueue:
		t.Fatalf("a structured payload was queued: %+v", req)
	default:
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN msg=coordinator.set_rejected") ||
		!strings.Contains(out, "topic="+topic) || !strings.Contains(out, `payload="{\"limit\": 3}"`) {
		t.Errorf("rejection not logged at warn with topic and payload:\n%s", out)
	}

	reader.mu.Lock()
	reader.writeFailures, reader.writeErr = 1, modbus.ErrValueParse
	reader.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = c.writeWorker(ctx); close(done) }()
	routeSet(t, c, publisher.Command{
		Topic: topic, Payload: []byte("abc"),
		Wildcards: []string{testSerial, "config", "charge_limit"},
	})
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "coordinator.write_failed") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	out := logs.String()
	if !strings.Contains(out, "level=WARN msg=coordinator.write_failed") ||
		!strings.Contains(out, "topic="+topic) || !strings.Contains(out, "payload=abc") {
		t.Errorf("failed write not logged at warn with topic and payload:\n%s", out)
	}
}

// TestMaintenanceIsRoutedByTheCommandRouter: `<name>/maintenance/set/…`
// reaches the instance through this daemon's router — the log level onto
// the daemon's real level variable, the restart onto its real shutdown
// path when a supervisor is known — and a retained restart never fires.
func TestMaintenanceIsRoutedByTheCommandRouter(t *testing.T) {
	c, _, stub, _ := buildDeps(t, false)
	var level slog.LevelVar
	restarted := make(chan struct{}, 2)
	c.deps.Instance = publisher.NewInstance(hassTransport(stub), publisher.InstanceConfig{
		Layout:      hass.NewLayout("MTEC"),
		Name:        hass.OriginName,
		Version:     "0.0.0-test",
		SetLogLevel: publisher.LevelVarSetter(&level),
		Supervised:  func() bool { return true },
		Shutdown:    func() { restarted <- struct{}{} },
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err := c.installInboundHandler(t.Context()); err != nil {
		t.Fatalf("installInboundHandler: %v", err)
	}
	t.Cleanup(func() { c.stopCommands(context.Background()) })

	stub.deliver("MTEC/maintenance/set/loglevel", []byte("debug"))
	stub.deliverMsg(&mqtt.Message{Topic: "MTEC/maintenance/set/restart", Payload: []byte("1"), Retain: true})
	c.commands.WaitIdle()
	if level.Level() != slog.LevelDebug {
		t.Errorf("log level = %v, want debug", level.Level())
	}
	select {
	case <-restarted:
		t.Fatal("a retained restart fired")
	case <-time.After(50 * time.Millisecond):
	}

	stub.deliver("MTEC/maintenance/set/restart", nil)
	c.commands.WaitIdle()
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("the restart did not reach the shutdown path")
	}
}

// --- the start-up sweep of the pre-2.0 layout --------------------------------

// TestLegacySweepClearsOnlyThisInstancesOldTopics drives the sweep against
// a broker holding, beside this instance's own pre-2.0 topics, a sibling
// inverter's on the same root, another root's, this release's own new
// layout, and an old-shaped topic under this serial for a key this
// instance does not publish. Only the exact old topics this instance owns
// may go; everything else must survive. A second start finds nothing left.
func TestLegacySweepClearsOnlyThisInstancesOldTopics(t *testing.T) {
	old := legacySweepWindow
	legacySweepWindow = 10 * time.Millisecond
	t.Cleanup(func() { legacySweepWindow = old })

	virtual := hass.DefaultVirtualSwitches(50, 50)
	c, _, stub, _ := buildDepsWith(t, false, testCatalogYAML, virtual)
	own := []string{
		"MTEC/" + testSerial + "/now-base/grid_power/state",
		"MTEC/" + testSerial + "/now-base/consumption/state",
		"MTEC/" + testSerial + "/config/mode/state",
		"MTEC/" + testSerial + "/config/mode/set", // somebody's retained command
		"MTEC/" + testSerial + "/config/charge_active/state",
		"MTEC/" + testSerial + "/static/serial_no/state",
		"MTEC/bridge/status",
	}
	survivors := []string{
		// A sibling inverter on the same root.
		"MTEC/OTHER-INVERTER/now-base/grid_power/state",
		"MTEC/OTHER-INVERTER/config/mode/state",
		// Another instance's root.
		"SOLAR/" + testSerial + "/now-base/grid_power/state",
		"SOLAR/bridge/status",
		// This release's own layout: a function name at the second level.
		"MTEC/status/" + testSerial + "/now_base/grid_power",
		"MTEC/connected",
		"MTEC/info",
		"MTEC/maintenance/stats",
		// Old-shaped, our serial, but a key this instance does not publish:
		// never a prefix match.
		"MTEC/" + testSerial + "/now-base/not_in_this_catalog/state",
		"MTEC/" + testSerial + "/availability",
		// Old shape with the NEW group spelling was never published.
		"MTEC/" + testSerial + "/now_base/grid_power/state",
	}
	stub.retained = map[string][]byte{}
	for _, topic := range append(slices.Clone(own), survivors...) {
		stub.retained[topic] = []byte("x")
	}

	tp := topicParts{root: "MTEC", serial: testSerial}
	c.runLegacySweep(t.Context(), tp)

	var cleared []string
	for _, p := range stub.snapshotPublishes() {
		if len(p.payload) != 0 {
			t.Errorf("the sweep published a non-empty payload to %s", p.topic)
			continue
		}
		if !p.retain {
			t.Errorf("%s cleared without retain, which clears nothing", p.topic)
		}
		cleared = append(cleared, p.topic)
	}
	slices.Sort(cleared)
	want := slices.Clone(own)
	slices.Sort(want)
	if !slices.Equal(cleared, want) {
		t.Errorf("cleared %v\nwant    %v", cleared, want)
	}
	// The window opens on the old layout only — never a filter that
	// overlaps the command routes, which would deliver a `set` twice.
	routes := []string{hass.CommandFilter("MTEC"), "MTEC/maintenance/set/#"}
	for _, f := range legacySweepFilters(tp) {
		if n := stub.countSubscribes(f); n != 1 {
			t.Errorf("subscribed %s %d times, want 1", f, n)
		}
		for _, r := range routes {
			for _, probe := range []string{"MTEC/set/" + testSerial + "/config/mode", "MTEC/maintenance/set/restart"} {
				if matchTopicFilter(f, probe) && matchTopicFilter(r, probe) {
					t.Errorf("sweep filter %s overlaps command route %s on %s", f, r, probe)
				}
			}
		}
		stub.mu.Lock()
		unsubscribed := slices.Contains(stub.unsubscribes, f)
		stub.mu.Unlock()
		if !unsubscribed {
			t.Errorf("the sweep's window %s was not unsubscribed", f)
		}
	}

	// The broker applied the clears; a second start clears nothing.
	stub.mu.Lock()
	for _, topic := range cleared {
		delete(stub.retained, topic)
	}
	stub.publishes = nil
	stub.mu.Unlock()
	c.runLegacySweep(t.Context(), tp)
	if pubs := stub.snapshotPublishes(); len(pubs) != 0 {
		t.Errorf("a second sweep published %d messages, want none", len(pubs))
	}
}

func TestLegacySweepTargetsNeverTouchAFunctionLevel(t *testing.T) {
	owned := map[string]bool{
		"mtec/status/x":           true, // an owned set that wrongly named a new topic
		"mtec/SN1/day/pv/state":   true,
		"home/mtec/bridge/status": true,
	}
	got := legacySweepTargets("mtec", []string{"mtec/status/x", "mtec/SN1/day/pv/state", "mtec/SN1/day/pv/state", "other/SN1/day/pv/state"}, owned)
	if !slices.Equal(got, []string{"mtec/SN1/day/pv/state"}) {
		t.Errorf("targets = %v", got)
	}
	// A multi-level root is cut at the root, not at the first level.
	got = legacySweepTargets("home/mtec", []string{"home/mtec/bridge/status"}, owned)
	if !slices.Equal(got, []string{"home/mtec/bridge/status"}) {
		t.Errorf("multi-level targets = %v", got)
	}
}

// TestLegacySweepRunsOncePerProcess: idempotent across starts, but not
// re-run on a reconnect within one process.
func TestLegacySweepRunsOncePerProcess(t *testing.T) {
	old := legacySweepWindow
	legacySweepWindow = 10 * time.Millisecond
	t.Cleanup(func() { legacySweepWindow = old })

	c, _, stub, _ := buildDeps(t, false)
	c.sweepLegacyTopics(t.Context()) // before the serial: nothing
	c.topicBase.Store(topicParts{root: "MTEC", serial: testSerial})
	c.sweepLegacyTopics(t.Context())
	c.sweepLegacyTopics(t.Context())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stub.mu.Lock()
		done := slices.Contains(stub.unsubscribes, "MTEC/bridge/status")
		stub.mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := stub.countSubscribes("MTEC/" + testSerial + "/#"); n != 1 {
		t.Errorf("the sweep ran %d times, want once", n)
	}
}

// hassTransport adapts the stub the way the composition root adapts the
// real client.
func hassTransport(stub *stubMQTT) publisher.Transport { return hagomqtt.Split(stub, stub) }
