// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
)

// --- stubs -----------------------------------------------------------------

// stubReader simulates modbus.Reader with canned per-group responses.
type stubReader struct {
	mu        sync.Mutex
	groupData map[registers.Group]map[string]any
	calls     map[registers.Group]int
	writes    []writeCall
	failReads map[registers.Group]error
	// writeFailures counts down: while positive, WriteRegisterByMQTT
	// returns writeErr instead of recording the write. writeAttempts
	// counts every call, failed or not.
	writeFailures int
	writeErr      error
	writeAttempts int
}

type writeCall struct{ mqttKey, value string }

func newStubReader() *stubReader {
	return &stubReader{
		groupData: map[registers.Group]map[string]any{},
		calls:     map[registers.Group]int{},
		failReads: map[registers.Group]error{},
	}
}

func (s *stubReader) ReadGroup(_ context.Context, g registers.Group) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[g]++
	if err, ok := s.failReads[g]; ok && err != nil {
		return nil, err
	}
	// Return a fresh copy so the coordinator doesn't accidentally
	// alias our test data.
	out := make(map[string]any, len(s.groupData[g]))
	for k, v := range s.groupData[g] {
		out[k] = v
	}
	return out, nil
}

func (s *stubReader) ReadRegister(_ context.Context, _ string) (any, error) {
	return nil, nil
}

func (s *stubReader) WriteRegisterByMQTT(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeAttempts++
	if s.writeFailures > 0 {
		s.writeFailures--
		return s.writeErr
	}
	s.writes = append(s.writes, writeCall{key, value})
	return nil
}

// failWrites makes the next n writes return err.
func (s *stubReader) failWrites(n int, err error) {
	s.mu.Lock()
	s.writeFailures, s.writeErr = n, err
	s.mu.Unlock()
}

func (s *stubReader) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeAttempts
}

func (s *stubReader) snapshotWrites() []writeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]writeCall, len(s.writes))
	copy(out, s.writes)
	return out
}

// stubModbus tracks Connect/Close calls.
type stubModbus struct {
	connectCalls atomic.Int32
	closeCalls   atomic.Int32
	connected    atomic.Bool
	connectErr   error
}

func (s *stubModbus) Connect(context.Context) error {
	s.connectCalls.Add(1)
	if s.connectErr != nil {
		return s.connectErr
	}
	s.connected.Store(true)
	return nil
}

func (s *stubModbus) Close() error      { s.closeCalls.Add(1); s.connected.Store(false); return nil }
func (s *stubModbus) IsConnected() bool { return s.connected.Load() }

// stubMQTT captures publishes and subscribe handlers.
type stubMQTT struct {
	mu           sync.Mutex
	publishes    []publishCall
	handlers     map[string]mqtt.MessageHandler
	subscribes   []string
	unsubscribes []string
	// publishErr, when non-nil, fails every Publish (models an open
	// circuit breaker). subscribeFailures / unsubscribeFailures count
	// down the number of calls that fail before the first success.
	publishErr          error
	subscribeFailures   int
	unsubscribeFailures int
	// beforePublish, when set, runs before each Publish is recorded —
	// the hook tests use to inject an event mid-batch. Called without
	// the stub's lock held.
	beforePublish func(topic string)
}

type publishCall struct {
	topic   string
	payload []byte
	retain  bool
}

func newStubMQTT() *stubMQTT {
	return &stubMQTT{handlers: map[string]mqtt.MessageHandler{}}
}

func (s *stubMQTT) Publish(_ context.Context, topic string, payload []byte, _ mqtt.QoS, retain bool, _ ...mqtt.PublishOption) error {
	s.mu.Lock()
	hook, err := s.beforePublish, s.publishErr
	s.mu.Unlock()
	if hook != nil {
		hook(topic)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(payload))
	copy(cp, payload)
	s.publishes = append(s.publishes, publishCall{topic, cp, retain})
	return nil
}

func (s *stubMQTT) Subscribe(_ context.Context, filter string, _ mqtt.QoS, h mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribes = append(s.subscribes, filter)
	if s.subscribeFailures > 0 {
		s.subscribeFailures--
		return mqtt.SubscribeResult{}, errInjected{}
	}
	s.handlers[filter] = h
	return mqtt.SubscribeResult{}, nil
}

func (s *stubMQTT) Unsubscribe(_ context.Context, filter string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribes = append(s.unsubscribes, filter)
	if s.unsubscribeFailures > 0 {
		s.unsubscribeFailures--
		return errInjected{}
	}
	delete(s.handlers, filter)
	return nil
}

// setPublishErr makes every subsequent Publish fail with err (nil clears).
func (s *stubMQTT) setPublishErr(err error) {
	s.mu.Lock()
	s.publishErr = err
	s.mu.Unlock()
}

func (s *stubMQTT) setBeforePublish(f func(topic string)) {
	s.mu.Lock()
	s.beforePublish = f
	s.mu.Unlock()
}

func (s *stubMQTT) countSubscribes(filter string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.subscribes {
		if f == filter {
			n++
		}
	}
	return n
}

func (s *stubMQTT) countUnsubscribes(filter string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.unsubscribes {
		if f == filter {
			n++
		}
	}
	return n
}

// deliver invokes every handler whose filter matches topic. The TCPClient
// adapter does proper wildcard matching internally; for tests we use a
// substring rule that is good enough for our specific filters.
func (s *stubMQTT) deliver(topic string, payload []byte) {
	s.deliverMsg(&mqtt.Message{Topic: topic, Payload: payload})
}

// deliverMsg is deliver for a fully-formed message, preserving flags
// such as Retain.
func (s *stubMQTT) deliverMsg(msg *mqtt.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for filter, h := range s.handlers {
		if matchTopicFilter(filter, msg.Topic) {
			h(msg)
		}
	}
}

// matchTopicFilter is a minimal MQTT wildcard matcher: '+' matches one
// level, '#' tail-matches. Mirrors the real client's behaviour for the
// two filter shapes the coordinator subscribes to.
func matchTopicFilter(filter, topic string) bool {
	if filter == topic {
		return true
	}
	fparts := splitPath(filter)
	tparts := splitPath(topic)
	for i, f := range fparts {
		if f == "#" {
			return true
		}
		if i >= len(tparts) {
			return false
		}
		if f != "+" && f != tparts[i] {
			return false
		}
	}
	return len(fparts) == len(tparts)
}

func (s *stubMQTT) snapshotPublishes() []publishCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]publishCall, len(s.publishes))
	copy(out, s.publishes)
	return out
}

// --- catalog helpers -------------------------------------------------------

// testCatalogYAML defines a small but representative catalog covering
// every code path the coordinator needs: STATIC (init), BASE (regular
// poll), CONFIG (writable + select with value_items), and a pseudo-
// register entry in BASE so PseudoRegisters has something to compute.
const testCatalogYAML = `
"10000":
  name: Inverter serial number
  length: 8
  type: STR
  mqtt: serial_no
  group: static

"10008":
  name: Equipment info
  length: 1
  type: BYTE
  mqtt: equipment_info
  group: static

"10011":
  name: Firmware version
  length: 4
  type: BYTE
  mqtt: firmware_version
  group: static

"11000":
  name: Grid power
  length: 2
  type: I32
  unit: W
  mqtt: grid_power
  group: now-base
  hass_device_class: power

"11016":
  name: Inverter AC power
  length: 2
  type: I32
  unit: W
  mqtt: inverter
  group: now-base
  hass_device_class: power

"52000":
  name: Operation mode
  length: 1
  type: U16
  writable: true
  mqtt: mode
  group: config
  hass_component_type: select
  hass_device_class: enum
  hass_value_items:
    0: "General"
    1: "Eco"

"consumption":
  name: Household consumption
  unit: W
  mqtt: consumption
  group: now-base
`

func buildConfig(t *testing.T, hassEnable bool) *config.Config {
	t.Helper()
	yaml := `
MODBUS_IP: 127.0.0.1
MODBUS_PORT: 502
MODBUS_SLAVE: 247
MODBUS_TIMEOUT: 5
MQTT_SERVER: localhost
MQTT_PORT: 1883
MQTT_TOPIC: MTEC
REFRESH_NOW: 1
REFRESH_CONFIG: 1
REFRESH_DAY: 1
REFRESH_TOTAL: 1
REFRESH_STATIC: 1
HASS_BIRTH_GRACETIME: 0
`
	if hassEnable {
		yaml += "HASS_ENABLE: true\n"
	}
	c, err := config.Load(strings.NewReader(yaml), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func buildDeps(t *testing.T, hassEnable bool) (*Coordinator, *stubReader, *stubMQTT, *stubModbus) {
	t.Helper()
	catalog, _, err := registers.LoadFromString(testCatalogYAML)
	if err != nil {
		t.Fatal(err)
	}
	cfg := buildConfig(t, hassEnable)
	reader := newStubReader()
	// STATIC must yield the three init fields the coordinator needs.
	reader.groupData[registers.GroupStatic] = map[string]any{
		"serial_no":        "MTEC-TEST-001",
		"equipment_info":   "30 03", // → 8.0K-25A-3P after lookup
		"firmware_version": "01 27 52 20  03 04 05 06",
	}
	reader.groupData[registers.GroupBase] = map[string]any{
		"inverter":   3000,
		"grid_power": -500, // exporting
	}
	reader.groupData[registers.GroupConfig] = map[string]any{
		"mode": 1, // Eco
	}
	// Empty maps so polls don't fail; the values from these groups
	// flow through the same publish path even when empty.
	for _, g := range []registers.Group{
		registers.GroupDay, registers.GroupTotal,
		registers.GroupGrid, registers.GroupInverter,
		registers.GroupBackup, registers.GroupBattery, registers.GroupPV,
	} {
		reader.groupData[g] = map[string]any{}
	}

	mqttStub := newStubMQTT()
	modbusStub := &stubModbus{}

	deps := Deps{
		Cfg:     cfg,
		Catalog: catalog,
		Modbus:  modbusStub,
		Reader:  reader,
		MQTT:    mqttStub,
		Now:     func() time.Time { return time.Date(2026, 5, 25, 14, 30, 45, 0, time.UTC) },
	}
	if hassEnable {
		deps.HASS = hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, catalog, cfg.Language, nil, cfg.DeviceName)
	}
	return New(deps), reader, mqttStub, modbusStub
}

// runFor starts the coordinator in a goroutine, lets it tick for d,
// then cancels and waits for it to finish.
func runFor(t *testing.T, c *Coordinator, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	time.Sleep(d)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

// --- tests -----------------------------------------------------------------

func TestRunInitialisesFromStaticAndPublishesBase(t *testing.T) {
	c, reader, mqttStub, modbusStub := buildDeps(t, false)
	runFor(t, c, 200*time.Millisecond)

	if modbusStub.connectCalls.Load() < 1 {
		t.Fatal("Modbus Connect was never called")
	}
	if reader.calls[registers.GroupStatic] < 1 {
		t.Fatal("STATIC group was never read")
	}

	// Topic base must reflect the serial from the stubbed STATIC data.
	wantPrefix := "MTEC/MTEC-TEST-001/"
	found := false
	for _, p := range mqttStub.snapshotPublishes() {
		if startsWith(p.topic, wantPrefix) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no publish under %s prefix; publishes=%v",
			wantPrefix, summariseTopics(mqttStub.snapshotPublishes()))
	}
}

func TestRunPublishesPseudoConsumptionAndAPIDate(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	runFor(t, c, 200*time.Millisecond)

	pubs := mqttStub.snapshotPublishes()
	// consumption is a float (3500.0) — formatValue applies the
	// default MQTT_FLOAT_FORMAT ".3f" so we get three decimals.
	want := map[string]string{
		"MTEC/MTEC-TEST-001/now-base/consumption/state": "3500.000",
		"MTEC/MTEC-TEST-001/now-base/api_date/state":    "2026-05-25 14:30:45",
	}
	for topic, val := range want {
		if !hasPublish(pubs, topic, val) {
			t.Errorf("missing publish %s = %q", topic, val)
		}
	}
}

func TestRunProcessesEnumThroughInitFlow(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	runFor(t, c, 200*time.Millisecond)

	// mode=1 → "Eco" via value_items
	pubs := mqttStub.snapshotPublishes()
	if !hasPublish(pubs, "MTEC/MTEC-TEST-001/config/mode/state", "Eco") {
		t.Fatalf("mode register did not flow through enum conversion; pubs: %s",
			summariseTopics(pubs))
	}
}

func TestRunWithHASSPublishesDiscoveryRetained(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	runFor(t, c, 200*time.Millisecond)

	var retained []string
	for _, p := range mqttStub.snapshotPublishes() {
		if p.retain && startsWith(p.topic, "homeassistant/") {
			retained = append(retained, p.topic)
		}
	}
	if len(retained) == 0 {
		t.Fatal("no retained HA discovery publishes")
	}
}

func TestRunHandlesIncomingSetCommand(t *testing.T) {
	c, reader, mqttStub, _ := buildDeps(t, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	// Wait until subscriptions are installed.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mqttStub.mu.Lock()
		ready := len(mqttStub.handlers) > 0
		mqttStub.mu.Unlock()
		if ready && c.loadTopicBase() != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Deliver a synthetic /set publish — should land as a write
	// against mqtt key "mode" with payload "Eco".
	mqttStub.deliver("MTEC/MTEC-TEST-001/config/mode/set", []byte("Eco"))

	// Give the write-worker a chance to drain.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	writes := reader.snapshotWrites()
	if len(writes) != 1 || writes[0].mqttKey != "mode" || writes[0].value != "Eco" {
		t.Fatalf("expected one write (mode=Eco), got %+v", writes)
	}
}

func TestRunIgnoresRetainedSetCommand(t *testing.T) {
	c, reader, mqttStub, _ := buildDeps(t, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	// Wait until subscriptions are installed and init completed.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mqttStub.mu.Lock()
		ready := len(mqttStub.handlers) > 0
		mqttStub.mu.Unlock()
		if ready && c.loadTopicBase() != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A retained delivery is the broker replaying an old command on
	// (re)subscribe — it must never reach the inverter.
	mqttStub.deliverMsg(&mqtt.Message{
		Topic:   "MTEC/MTEC-TEST-001/config/mode/set",
		Payload: []byte("Eco"),
		Retain:  true,
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if writes := reader.snapshotWrites(); len(writes) != 0 {
		t.Fatalf("retained command was written to the inverter: %+v", writes)
	}
}

// TestOnMessageDuringStaticInitIsRaceFree pins the fix for the data
// race between the MQTT dispatch goroutine reading topicBase in
// onMessage and the Run goroutine writing it in tryInitFromStatic.
// The race detector (`make test` runs with -race) flags the old
// plain-string field under this workload.
func TestOnMessageDuringStaticInitIsRaceFree(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)
	// Silence the write-queue-full and static-init log chatter this
	// tight loop produces.
	c.deps.Logger = slog.New(slog.DiscardHandler)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			c.onMessage(&mqtt.Message{
				Topic:   "MTEC/MTEC-TEST-001/config/mode/set",
				Payload: []byte("Eco"),
			})
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			if err := c.tryInitFromStatic(ctx); err != nil {
				t.Errorf("tryInitFromStatic: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestTryInitFromStaticRejectsTopicUnsafeSerial(t *testing.T) {
	for _, serial := range []string{
		"BAD/SERIAL", "BAD+SERIAL", "BAD#SERIAL", "BAD\x00SERIAL", "BAD\nSERIAL",
	} {
		c, reader, _, _ := buildDeps(t, false)
		reader.groupData[registers.GroupStatic]["serial_no"] = serial
		if err := c.tryInitFromStatic(context.Background()); err == nil {
			t.Errorf("serial %q was accepted", serial)
		}
		if tb := c.loadTopicBase(); tb != "" {
			t.Errorf("topicBase %q was set despite unsafe serial %q", tb, serial)
		}
	}
}

// TestHASSBirthTriggersDiscoveryRepublish covers the full loop: HA
// announces itself ("online" birth), onMessage clears discoverySent,
// and the discoveryRepublisher goroutine re-sends every retained
// discovery config. The republisher ticks every 5s, so this test —
// like TestModbusWatchdogReconnects — polls for several seconds.
func TestHASSBirthTriggersDiscoveryRepublish(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	// The config loader maps 0 to the 15s default; skip the birth wait
	// entirely so the initial discovery burst happens immediately.
	c.deps.Cfg.HASSBirthGracetime = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	countConfigs := func() int {
		n := 0
		for _, p := range mqttStub.snapshotPublishes() {
			if p.retain && startsWith(p.topic, "homeassistant/") && endsWith(p.topic, "/config") {
				n++
			}
		}
		return n
	}

	// Wait for the initial discovery burst.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countConfigs() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	initial := countConfigs()
	if initial == 0 {
		t.Fatal("no initial discovery publishes")
	}

	// Home Assistant restarts and publishes its birth message.
	mqttStub.deliver("homeassistant/status", []byte("online"))

	deadline = time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) && countConfigs() <= initial {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	if got := countConfigs(); got <= initial {
		t.Fatalf("discovery was not republished after HA birth: %d configs before, %d after",
			initial, got)
	}
}

func TestModbusWatchdogReconnects(t *testing.T) {
	c, _, _, modbusStub := buildDeps(t, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Wait for first connect, then simulate the transport poisoning
	// itself by toggling IsConnected → false.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if modbusStub.connectCalls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := modbusStub.connectCalls.Load()
	modbusStub.connected.Store(false)

	// Watchdog ticks every 5 s in production; for tests we just wait
	// long enough that it has at least one shot. We tolerate the
	// timing by polling rather than asserting an exact count.
	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if modbusStub.connectCalls.Load() > first {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	if modbusStub.connectCalls.Load() <= first {
		t.Fatalf("watchdog did not reconnect; connect calls = %d (start %d)",
			modbusStub.connectCalls.Load(), first)
	}
}

// TestRunRetriesInitialModbusConnect proves the boot-time connect is
// no longer fatal: an unreachable inverter (e.g. its gateway still
// rejoining the network after a power outage) is retried with backoff
// until ctx is cancelled, and cancellation surfaces as
// context.Canceled — a normal stop, not a hard failure.
func TestRunRetriesInitialModbusConnect(t *testing.T) {
	c, _, _, modbusStub := buildDeps(t, false)
	modbusStub.connectErr = errInjected{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for modbusStub.connectCalls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("Run did not retry the initial connect; calls = %d",
				modbusStub.connectCalls.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	err := <-done
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup returned %v, want nil or context.Canceled", err)
	}
}

// --- discovery completeness ------------------------------------------------

// TestPublishDiscoveryNotMarkedSentWhenPublishFails pins the fix for a
// silent, restart-only failure mode: with the broker refusing publishes
// at startup (an open circuit breaker publishes nothing at all),
// discoverySent used to be raised anyway, so the republisher never
// retried and Home Assistant stayed without entities until the daemon
// was restarted.
func TestPublishDiscoveryNotMarkedSentWhenPublishFails(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")

	mqttStub.setPublishErr(errInjected{})
	published := c.publishDiscovery(context.Background())
	if c.discoverySent.Load() {
		t.Fatal("discovery marked as sent although every publish failed")
	}
	if len(published) == 0 {
		t.Fatal("the advertised set must still list the topics, so reconcile keeps them")
	}

	// Broker recovers → the next pass completes and latches.
	mqttStub.setPublishErr(nil)
	c.publishDiscovery(context.Background())
	if !c.discoverySent.Load() {
		t.Fatal("a fully successful batch must mark discovery as sent")
	}
}

// TestPublishDiscoveryKeepsBirthArrivingMidPublish covers the lost
// update between onMessage and publishDiscovery: a Home Assistant birth
// that lands while we are publishing used to be erased by the final
// Store(true), so the entities HA asked for were never re-sent.
func TestPublishDiscoveryKeepsBirthArrivingMidPublish(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	c.deps.HASS.Initialize("MTEC-TEST-001", "V1", "model")

	var once sync.Once
	mqttStub.setBeforePublish(func(string) {
		once.Do(func() {
			c.onMessage(&mqtt.Message{
				Topic:   c.hassStatusTopic,
				Payload: []byte("online"),
			})
		})
	})

	c.publishDiscovery(context.Background())
	if c.discoverySent.Load() {
		t.Fatal("birth seen during publishDiscovery was swallowed; republisher will not run")
	}
}

// --- write queue -----------------------------------------------------------

// TestEnqueueWriteDropsOldestCommand pins drop-oldest: dragging an HA
// slider emits a burst of commands and the one that must reach the
// inverter is the value the user released it at. Dropping the newest
// left the inverter on an intermediate value.
func TestEnqueueWriteDropsOldestCommand(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)

	depth := cap(c.writeQueue)
	for i := range depth {
		if err := c.enqueueWrite(writeReq{mqttKey: "charge_limit", value: strconv.Itoa(i)}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := c.enqueueWrite(writeReq{mqttKey: "charge_limit", value: "newest"}); err != nil {
		t.Fatalf("enqueue into a full queue: %v", err)
	}

	drained := make([]string, 0, depth)
	for len(c.writeQueue) > 0 {
		drained = append(drained, (<-c.writeQueue).value)
	}
	if len(drained) != depth {
		t.Fatalf("queue holds %d entries, want %d", len(drained), depth)
	}
	if drained[len(drained)-1] != "newest" {
		t.Errorf("newest command was dropped; queue tail = %q", drained[len(drained)-1])
	}
	if drained[0] != "1" {
		t.Errorf("oldest command was not the one dropped; queue head = %q", drained[0])
	}
}

// TestEnqueueWriteRepliesToDroppedCaller makes sure a synchronous caller
// whose command is dropped is told, instead of waiting for a reply that
// will never come.
func TestEnqueueWriteRepliesToDroppedCaller(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)

	reply := make(chan error, 1)
	if err := c.enqueueWrite(writeReq{mqttKey: "mode", value: "oldest", reply: reply}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < cap(c.writeQueue); i++ {
		if err := c.enqueueWrite(writeReq{mqttKey: "mode", value: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.enqueueWrite(writeReq{mqttKey: "mode", value: "newest"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-reply:
		if !errors.Is(err, ErrWriteSuperseded) {
			t.Fatalf("dropped caller got %v, want ErrWriteSuperseded", err)
		}
	default:
		t.Fatal("dropped caller was left waiting for a reply")
	}
}

// TestWebWriteIsSerialisedThroughTheQueue proves the web path shares the
// HA command queue: a command queued earlier runs first, and the web
// call still returns the real outcome synchronously. Bypassing the queue
// let a web write and an HA write for the same register reach the
// inverter in either order.
func TestWebWriteIsSerialisedThroughTheQueue(t *testing.T) {
	c, reader, _, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A Home Assistant command is already waiting.
	if err := c.enqueueWrite(writeReq{mqttKey: "mode", value: "from-hass"}); err != nil {
		t.Fatal(err)
	}
	// Worker not started yet, but the queued path is what Write must take.
	c.writeWorkerUp.Store(true)

	errCh := make(chan error, 1)
	go func() { errCh <- c.Write(ctx, "mode", "from-web") }()

	time.Sleep(50 * time.Millisecond)
	if n := len(reader.snapshotWrites()); n != 0 {
		t.Fatalf("web write went straight to the transport: %d writes before the worker ran", n)
	}

	workerDone := make(chan error, 1)
	go func() { workerDone <- c.writeWorker(ctx) }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("web write returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("web write never received its reply")
	}
	cancel()
	<-workerDone

	writes := reader.snapshotWrites()
	if len(writes) != 2 || writes[0].value != "from-hass" || writes[1].value != "from-web" {
		t.Fatalf("writes = %+v, want from-hass then from-web", writes)
	}
}

// TestWebWriteDispatchesInlineWithoutWorker keeps the web API answerable
// when no writeWorker is draining the queue (server up before Run, or
// Run already returned) instead of blocking until the client gives up.
func TestWebWriteDispatchesInlineWithoutWorker(t *testing.T) {
	c, reader, _, _ := buildDeps(t, false)
	if err := c.Write(context.Background(), "mode", "Eco"); err != nil {
		t.Fatal(err)
	}
	writes := reader.snapshotWrites()
	if len(writes) != 1 || writes[0] != (writeCall{"mode", "Eco"}) {
		t.Fatalf("writes = %+v, want one inline mode=Eco", writes)
	}
}

// --- write retry -----------------------------------------------------------

// shortenWriteRetry makes the writeWorker's retry delay test-fast.
func shortenWriteRetry(t *testing.T) {
	t.Helper()
	old := writeRetryDelay
	writeRetryDelay = time.Millisecond
	t.Cleanup(func() { writeRetryDelay = old })
}

// TestWriteRetriesTransientModbusError covers commands issued inside the
// watchdog's reconnect window: the transport is momentarily poisoned,
// and the command used to be logged and dropped, leaving HA showing a
// value the inverter never received.
func TestWriteRetriesTransientModbusError(t *testing.T) {
	c, reader, _, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	shortenWriteRetry(t)

	reader.failWrites(2, modbus.ErrNotConnected)
	err := c.dispatchWriteRetrying(context.Background(),
		writeReq{mqttKey: "mode", value: "Eco"})
	if err != nil {
		t.Fatalf("transient failure was not retried to success: %v", err)
	}
	if got := reader.attempts(); got != 3 {
		t.Errorf("attempts = %d, want 3 (two failures + the success)", got)
	}
	writes := reader.snapshotWrites()
	if len(writes) != 1 || writes[0] != (writeCall{"mode", "Eco"}) {
		t.Errorf("writes = %+v, want a single mode=Eco", writes)
	}
}

// TestWriteDoesNotRetryPermanentRejection keeps the retry from burning
// queue time on a command the reader has already judged impossible.
func TestWriteDoesNotRetryPermanentRejection(t *testing.T) {
	c, reader, _, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	shortenWriteRetry(t)

	reader.failWrites(5, fmt.Errorf("%w: mqtt=%q", modbus.ErrNotWritable, "mode"))
	err := c.dispatchWriteRetrying(context.Background(),
		writeReq{mqttKey: "mode", value: "Eco"})
	if !errors.Is(err, modbus.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	if got := reader.attempts(); got != 1 {
		t.Errorf("attempts = %d, want 1 (a read-only register never becomes writable)", got)
	}
}

// TestWriteGivesUpAfterRetries checks the final failure is surfaced to a
// synchronous caller rather than swallowed.
func TestWriteGivesUpAfterRetries(t *testing.T) {
	c, reader, _, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	shortenWriteRetry(t)

	reader.failWrites(writeRetries+5, modbus.ErrNotConnected)
	err := c.dispatchWriteRetrying(context.Background(),
		writeReq{mqttKey: "mode", value: "Eco"})
	if !errors.Is(err, modbus.ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if got := reader.attempts(); got != writeRetries {
		t.Errorf("attempts = %d, want %d", got, writeRetries)
	}
}

// --- startup subscribe -----------------------------------------------------

// shortenStartupBackoff makes the startup retry loop test-fast.
func shortenStartupBackoff(t *testing.T) {
	t.Helper()
	oldStart, oldMax := startupBackoff, startupMaxBackoff
	startupBackoff, startupMaxBackoff = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { startupBackoff, startupMaxBackoff = oldStart, oldMax })
}

// TestInstallInboundHandlerRetriesSubscribe: a broker that is still
// coming up alongside the daemon used to be fatal here, killing the
// whole command path while every other startup step retried.
func TestInstallInboundHandlerRetriesSubscribe(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	shortenStartupBackoff(t)

	mqttStub.mu.Lock()
	mqttStub.subscribeFailures = 2
	mqttStub.mu.Unlock()

	if err := c.installInboundHandler(context.Background()); err != nil {
		t.Fatalf("installInboundHandler = %v, want nil after retries", err)
	}
	if got := mqttStub.countSubscribes(c.hassStatusTopic); got != 3 {
		t.Errorf("status filter subscribed %d times, want 3 (two failures + success)", got)
	}
	// The second filter succeeded first try — retries must not re-subscribe
	// a filter that is already installed.
	setFilter := c.deps.Cfg.MQTTTopic + "/+/+/+/set"
	if got := mqttStub.countSubscribes(setFilter); got != 1 {
		t.Errorf("set filter subscribed %d times, want 1", got)
	}
}

func TestInstallInboundHandlerStopsOnCancelledContext(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	shortenStartupBackoff(t)

	mqttStub.mu.Lock()
	mqttStub.subscribeFailures = 1 << 20 // never succeeds
	mqttStub.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.installInboundHandler(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// --- reconcile unsubscribe -------------------------------------------------

func TestUnsubscribeWithRetrySucceedsAfterFailures(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	oldDelay := reconcileUnsubscribeDelay
	reconcileUnsubscribeDelay = time.Millisecond
	t.Cleanup(func() { reconcileUnsubscribeDelay = oldDelay })

	const filter = "homeassistant/+/+/config"
	mqttStub.mu.Lock()
	mqttStub.unsubscribeFailures = 2
	mqttStub.mu.Unlock()

	c.unsubscribeWithRetry(context.Background(), filter)
	if got := mqttStub.countUnsubscribes(filter); got != 3 {
		t.Fatalf("unsubscribe attempts = %d, want 3", got)
	}
	mqttStub.mu.Lock()
	_, still := mqttStub.handlers[filter]
	mqttStub.mu.Unlock()
	if still {
		t.Error("collector subscription survived; it would replay on every reconnect")
	}
}

func TestUnsubscribeWithRetryGivesUpBounded(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, true)
	c.deps.Logger = slog.New(slog.DiscardHandler)
	oldDelay := reconcileUnsubscribeDelay
	reconcileUnsubscribeDelay = time.Millisecond
	t.Cleanup(func() { reconcileUnsubscribeDelay = oldDelay })

	const filter = "homeassistant/+/+/config"
	mqttStub.mu.Lock()
	mqttStub.unsubscribeFailures = 100
	mqttStub.mu.Unlock()

	c.unsubscribeWithRetry(context.Background(), filter)
	if got := mqttStub.countUnsubscribes(filter); got != reconcileUnsubscribeTries {
		t.Fatalf("unsubscribe attempts = %d, want %d", got, reconcileUnsubscribeTries)
	}
}

// --- secondary round-robin -------------------------------------------------

// TestSecondaryIndexSurvivesCounterOverflow: the tick counter wraps to
// negative after 2^31 ticks, and `int(v) % len` would then hand the
// slice a negative index and panic.
func TestSecondaryIndexSurvivesCounterOverflow(t *testing.T) {
	for _, v := range []int32{
		0, 1, 4, 5,
		math.MaxInt32 - 1, math.MaxInt32,
		math.MinInt32, math.MinInt32 + 1, -1,
	} {
		idx := secondaryIndex(v)
		if idx < 0 || idx >= len(secondaryGroups) {
			t.Fatalf("secondaryIndex(%d) = %d, out of range [0,%d)", v, idx, len(secondaryGroups))
		}
	}
	// The rotation stays contiguous across the wrap.
	if got, want := secondaryIndex(math.MinInt32), (secondaryIndex(math.MaxInt32)+1)%len(secondaryGroups); got != want {
		t.Errorf("index after wrap = %d, want %d", got, want)
	}
}

// --- web catalog projection ------------------------------------------------

// TestRegistersCopiesValueItems: the catalog hands out its own enum map
// for the English case, so returning it unguarded let any consumer of
// the web API mutate the shared, process-lifetime catalog.
func TestRegistersCopiesValueItems(t *testing.T) {
	c, _, _, _ := buildDeps(t, false)

	items := valueItemsOf(t, c.Registers(), "mode")
	if items[1] != "Eco" {
		t.Fatalf("unexpected catalog labels: %v", items)
	}
	items[1] = "MUTATED"
	items[99] = "injected"

	fresh := valueItemsOf(t, c.Registers(), "mode")
	if fresh[1] != "Eco" {
		t.Errorf("catalog label was mutated through the web projection: %q", fresh[1])
	}
	if _, ok := fresh[99]; ok {
		t.Error("an entry injected via the web projection reached the catalog")
	}
}

func valueItemsOf(t *testing.T, regs []state.RegisterInfo, mqttKey string) map[int]string {
	t.Helper()
	for i := range regs {
		if regs[i].MQTT == mqttKey {
			return regs[i].ValueItems
		}
	}
	t.Fatalf("register %q missing from the projection", mqttKey)
	return nil
}

// --- helpers ---------------------------------------------------------------

type errInjected struct{}

func (errInjected) Error() string { return "injected: dial refused" }

func hasPublish(pubs []publishCall, topic, payload string) bool {
	for _, p := range pubs {
		if p.topic == topic && string(p.payload) == payload {
			return true
		}
	}
	return false
}

func summariseTopics(pubs []publishCall) string {
	parts := make([]string, 0, len(pubs))
	for _, p := range pubs {
		parts = append(parts, p.topic+"="+string(p.payload))
	}
	return strings.Join(parts, ", ")
}
