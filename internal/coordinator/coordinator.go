// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package coordinator orchestrates the M-TEC → MQTT data flow.
//
// One coordinator instance owns the Modbus transport, the MQTT
// transport, and (optionally) the Home Assistant integration. Run
// launches a fan-out of long-running goroutines — one per register
// group's polling cadence, plus a watchdog and a writer that drains
// inbound HA commands — and blocks until its context is cancelled or
// a child goroutine returns an error.
//
// All I/O lives in the wired-in transports; the coordinator itself
// only sequences calls and turns register values into MQTT payloads.
package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/mqtt"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// ModbusClient is the subset of [*modbus.Client] the coordinator needs
// for its watchdog. Defined narrow so tests can stub it without
// dragging the whole TCP machinery in.
type ModbusClient interface {
	Connect(ctx context.Context) error
	Close() error
	IsConnected() bool
}

// MQTTPublisher is the subset of [*mqtt.TCPClient] the coordinator
// publishes through. Matches the interface in internal/mqtt verbatim
// so the real client satisfies it for free.
type MQTTPublisher interface {
	Publish(ctx context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool) error
}

// MQTTSubscriber is the subset of [*mqtt.TCPClient] used to receive
// inbound HA command messages.
type MQTTSubscriber interface {
	Subscribe(ctx context.Context, filter string, qos mqtt.QoS, handler mqtt.MessageHandler) error
	Unsubscribe(ctx context.Context, filter string) error
}

// Reader is the subset of [*modbus.Reader] the coordinator pulls
// register data through.
type Reader interface {
	ReadGroup(ctx context.Context, g registers.Group) (map[string]any, error)
	ReadRegister(ctx context.Context, key string) (any, error)
	WriteRegisterByMQTT(ctx context.Context, mqttKey, value string) error
}

// Deps bundles the wired-in collaborators. Keeping them in a struct
// (rather than a long [New] parameter list) makes test setup
// readable and lets callers swap a single dependency at a time.
type Deps struct {
	Cfg     *config.Config
	Catalog *registers.Map
	Modbus  ModbusClient
	Reader  Reader
	MQTT    interface {
		MQTTPublisher
		MQTTSubscriber
	}
	HASS   *hass.Discovery // nil when HASS_ENABLE=false
	Logger *slog.Logger    // nil → slog.Default()
	// Now returns the wall-clock time used for api_date. Defaults to
	// time.Now; tests inject a fixed clock.
	Now func() time.Time
}

// Coordinator is the M-TEC → MQTT data-flow root.
type Coordinator struct {
	deps Deps

	// initialised in Run after the first STATIC read succeeds
	serialNo      string
	firmware      string
	equipmentInfo string
	topicBase     string

	secondaryIdx  atomic.Int32
	discoverySent atomic.Bool
	writeQueue    chan writeReq

	initOnce sync.Once
	initErr  error

	// hassStatusTopic caches "<hass_base>/status" so the message
	// handler can compare topic strings without rebuilding it on
	// every inbound publish.
	hassStatusTopic string
}

// writeReq is one HA → device command pending dispatch.
type writeReq struct {
	mqttKey string
	value   string
}

// New constructs a Coordinator. It does not touch the network or
// spawn goroutines — call [Coordinator.Run] for that.
func New(d Deps) *Coordinator {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Coordinator{
		deps:            d,
		writeQueue:      make(chan writeReq, 32),
		hassStatusTopic: d.Cfg.HASSBaseTopic + "/status",
	}
}

// Run executes the full daemon loop:
//
//  1. Modbus + MQTT connect (synchronous; first failure surfaces here)
//  2. Subscribe to the HASS status topic when HA discovery is enabled
//     and wait HASS_BIRTH_GRACETIME for an "online" message
//  3. Read the STATIC register group to learn the inverter's serial
//     number, firmware version and equipment code
//  4. Build + publish HA discovery payloads (when enabled) and
//     subscribe to every writable entity's /set topic
//  5. Spawn the per-group polling goroutines plus the write-queue
//     drainer; block until ctx is cancelled
//  6. Disconnect cleanly
//
// The first stage runs serially so a hard failure (wrong IP, broker
// down) surfaces immediately rather than from a background goroutine.
func (c *Coordinator) Run(ctx context.Context) error {
	log := c.deps.Logger

	log.Info("coordinator.starting",
		slog.String("modbus", c.deps.Cfg.ModbusIP),
		slog.String("mqtt", c.deps.Cfg.MQTTServer),
		slog.Bool("hass", c.deps.Cfg.HASSEnable))

	if err := c.deps.Modbus.Connect(ctx); err != nil {
		return fmt.Errorf("coordinator: modbus connect: %w", err)
	}
	defer func() { _ = c.deps.Modbus.Close() }()

	// MQTT connect is handled by the lifecycle layer above us — by
	// the time Run is called the client is already publishable. If a
	// caller wires us up without that lifecycle (e.g. tests) the
	// stub publisher noops anyway.

	// Wire the message handler before subscribing so no inbound
	// publish races past us.
	if err := c.installInboundHandler(ctx); err != nil {
		return err
	}

	// HA birth gracetime — the Python coordinator subscribes to
	// homeassistant/status, sleeps HASS_BIRTH_GRACETIME, then sends
	// discovery. We do the same so HA picks up the entities even if
	// the daemon starts before HA finishes booting.
	if c.deps.HASS != nil {
		c.waitForHASSBirth(ctx)
	}

	// First STATIC read is mandatory — it provides the serial number
	// the MQTT topic tree is keyed on. Block (with retries) until we
	// have one or ctx is cancelled.
	if err := c.waitForStatic(ctx); err != nil {
		return err
	}

	if c.deps.HASS != nil {
		c.deps.HASS.Initialize(c.serialNo, c.firmware, c.equipmentInfo)
		if err := c.publishDiscovery(ctx); err != nil {
			log.Warn("coordinator.discovery_failed", slog.String("err", err.Error()))
		}
	}

	g, runCtx := errgroup.WithContext(ctx)
	c.spawnPolls(g, runCtx)
	g.Go(func() error { return c.writeWorker(runCtx) })
	g.Go(func() error { return c.modbusWatchdog(runCtx) })

	err := g.Wait()
	// Context cancellation is the expected exit, not a failure.
	if err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("coordinator.stopped")
	return nil
}

// installInboundHandler wires the single subscriber callback that
// routes every inbound MQTT publish — HA birth messages and writable
// /set commands — into the right handler. Subscriptions for
// individual command topics happen later in publishDiscovery.
func (c *Coordinator) installInboundHandler(ctx context.Context) error {
	// Subscribe to a wide filter that catches both the HASS status
	// topic and every device /set topic. The TCPClient adapter does
	// the wildcard routing internally; one handler is plenty.
	//
	// We subscribe to two narrow filters rather than one fat #-wildcard
	// so an unrelated topic on the broker can't accidentally drive
	// the daemon. Wildcards:
	//   <hass_base>/status   → HA online/offline birth
	//   <mqtt_topic>/+/+/+/set → writable command path
	subs := []string{
		c.hassStatusTopic,
		c.deps.Cfg.MQTTTopic + "/+/+/+/set",
	}
	for _, s := range subs {
		if err := c.deps.MQTT.Subscribe(ctx, s, mqtt.QoS1, c.onMessage); err != nil {
			return fmt.Errorf("coordinator: subscribe %s: %w", s, err)
		}
	}
	return nil
}

// onMessage dispatches one inbound publish. Errors are logged and
// swallowed — the message loop must not exit because a single bad
// payload arrived.
func (c *Coordinator) onMessage(topic string, payload []byte) {
	log := c.deps.Logger
	if topic == c.hassStatusTopic {
		if string(payload) == "online" {
			log.Info("coordinator.hass_birth_seen")
			c.discoverySent.Store(false) // trigger republish next chance
		}
		return
	}
	// Expected shape: <topic>/<serial>/<group>/<mqtt_key>/set
	if c.topicBase == "" {
		return // not initialised yet — drop silently
	}
	if !startsWith(topic, c.topicBase+"/") || !endsWith(topic, "/set") {
		return
	}
	// We want the second-to-last path segment as the MQTT key.
	parts := splitPath(topic)
	if len(parts) < 4 {
		return
	}
	mqttKey := parts[len(parts)-2]
	select {
	case c.writeQueue <- writeReq{mqttKey: mqttKey, value: string(payload)}:
	default:
		log.Warn("coordinator.write_queue_full",
			slog.String("mqtt_key", mqttKey))
	}
}

// waitForHASSBirth subscribes to the HA status topic and sleeps the
// configured gracetime so HA — if it's coming up alongside the
// daemon — has time to announce itself before we publish discovery.
func (c *Coordinator) waitForHASSBirth(ctx context.Context) {
	c.deps.Logger.Info("coordinator.hass_birth_wait",
		slog.Duration("for", c.deps.Cfg.HASSBirthGracetimeDuration()))
	select {
	case <-ctx.Done():
	case <-time.After(c.deps.Cfg.HASSBirthGracetimeDuration()):
	}
}

// waitForStatic blocks until the STATIC group yields a usable serial
// number or the context is cancelled. The Python coordinator retries
// indefinitely; we do the same but cap the per-retry wait so test
// teardown isn't dragged out for full backoff cycles.
func (c *Coordinator) waitForStatic(ctx context.Context) error {
	log := c.deps.Logger
	const retry = 10 * time.Second
	for {
		if err := c.tryInitFromStatic(ctx); err == nil {
			return nil
		} else {
			log.Warn("coordinator.static_init_retry",
				slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
}

// tryInitFromStatic does one STATIC read and pulls the serial number,
// firmware version and equipment info out. Returns an error when any
// of those values are missing — caller retries.
func (c *Coordinator) tryInitFromStatic(ctx context.Context) error {
	data, err := c.deps.Reader.ReadGroup(ctx, registers.GroupStatic)
	if err != nil {
		return err
	}
	// Apply the same value processing so firmware/equipment land in
	// the same shape downstream consumers see.
	processed := processValues(c.deps.Catalog, data)
	serial, _ := processed["serial_no"].(string)
	firmware, _ := processed["firmware_version"].(string)
	equip, _ := processed["equipment_info"].(string)
	if serial == "" {
		return fmt.Errorf("coordinator: STATIC read missing serial_no")
	}
	c.serialNo = serial
	c.firmware = firmware
	c.equipmentInfo = equip
	c.topicBase = c.deps.Cfg.MQTTTopic + "/" + serial
	c.deps.Logger.Info("coordinator.static_initialised",
		slog.String("serial", serial),
		slog.String("firmware", firmware),
		slog.String("equipment", equip),
		slog.String("topic_base", c.topicBase))
	return nil
}

// publishDiscovery sends every HA discovery payload with retain=true
// and subscribes to every writable entity's command topic so HA can
// drive the inverter back. Existing command-topic subscriptions are
// idempotent on the adapter — re-subscribing on reconnect is safe.
func (c *Coordinator) publishDiscovery(ctx context.Context) error {
	if c.deps.HASS == nil {
		return nil
	}
	log := c.deps.Logger
	entries := c.deps.HASS.Entries()
	for _, e := range entries {
		if err := c.deps.MQTT.Publish(ctx, e.ConfigTopic, e.Payload, mqtt.QoS0, true); err != nil {
			log.Warn("coordinator.discovery_publish",
				slog.String("topic", e.ConfigTopic),
				slog.String("err", err.Error()))
		}
	}
	c.discoverySent.Store(true)
	log.Info("coordinator.discovery_sent", slog.Int("entries", len(entries)))
	return nil
}

// modbusWatchdog re-runs Connect when the transport reports a closed
// socket. The transport itself "poisons" the connection on any I/O
// error so a closed conn is the canonical "needs reconnect" signal.
func (c *Coordinator) modbusWatchdog(ctx context.Context) error {
	const tick = 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(tick):
		}
		if c.deps.Modbus.IsConnected() {
			continue
		}
		c.deps.Logger.Warn("coordinator.modbus_reconnect")
		if err := c.deps.Modbus.Connect(ctx); err != nil {
			c.deps.Logger.Warn("coordinator.modbus_reconnect_failed",
				slog.String("err", err.Error()))
		}
	}
}

// writeWorker drains the queue of inbound HA commands, calling
// WriteRegisterByMQTT for each. We process sequentially because the
// Modbus client serialises wire transactions anyway, so parallelism
// here would only deepen the queue without speeding the inverter up.
func (c *Coordinator) writeWorker(ctx context.Context) error {
	log := c.deps.Logger
	for {
		select {
		case <-ctx.Done():
			return nil
		case req := <-c.writeQueue:
			err := c.deps.Reader.WriteRegisterByMQTT(ctx, req.mqttKey, req.value)
			if err != nil {
				log.Warn("coordinator.write_failed",
					slog.String("mqtt_key", req.mqttKey),
					slog.String("value", req.value),
					slog.String("err", err.Error()))
				continue
			}
			log.Info("coordinator.write_ok",
				slog.String("mqtt_key", req.mqttKey),
				slog.String("value", req.value))
		}
	}
}

// --- string helpers (avoid pulling "strings" for two predicates) ----------

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func splitPath(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
