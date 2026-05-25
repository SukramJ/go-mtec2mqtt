// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/mqtt"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// --- stubs -----------------------------------------------------------------

// stubReader simulates modbus.Reader with canned per-group responses.
type stubReader struct {
	mu        sync.Mutex
	groupData map[registers.Group]map[string]any
	calls     map[registers.Group]int
	writes    []writeCall
	failReads map[registers.Group]error
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
	s.writes = append(s.writes, writeCall{key, value})
	s.mu.Unlock()
	return nil
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
	mu         sync.Mutex
	publishes  []publishCall
	handlers   map[string]mqtt.MessageHandler
	subscribes []string
}

type publishCall struct {
	topic   string
	payload []byte
	retain  bool
}

func newStubMQTT() *stubMQTT {
	return &stubMQTT{handlers: map[string]mqtt.MessageHandler{}}
}

func (s *stubMQTT) Publish(_ context.Context, topic string, payload []byte, _ mqtt.QoS, retain bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(payload))
	copy(cp, payload)
	s.publishes = append(s.publishes, publishCall{topic, cp, retain})
	return nil
}

func (s *stubMQTT) Subscribe(_ context.Context, filter string, _ mqtt.QoS, h mqtt.MessageHandler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribes = append(s.subscribes, filter)
	s.handlers[filter] = h
	return nil
}

func (s *stubMQTT) Unsubscribe(_ context.Context, filter string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handlers, filter)
	return nil
}

// deliver invokes every handler whose filter matches topic. The TCPClient
// adapter does proper wildcard matching internally; for tests we use a
// substring rule that is good enough for our specific filters.
func (s *stubMQTT) deliver(topic string, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for filter, h := range s.handlers {
		if matchTopicFilter(filter, topic) {
			h(topic, payload)
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
		deps.HASS = hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, catalog)
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
		if err != nil && err != context.Canceled {
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
		if ready && c.topicBase != "" {
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

func TestRunFailsFastOnInitialModbusConnect(t *testing.T) {
	c, _, _, modbusStub := buildDeps(t, false)
	modbusStub.connectErr = errInjected{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := c.Run(ctx)
	if err == nil {
		t.Fatal("expected initial-connect error to bubble")
	}
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
