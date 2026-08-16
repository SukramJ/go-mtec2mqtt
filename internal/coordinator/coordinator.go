// SPDX-License-Identifier: LGPL-3.0-or-later
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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
)

// Errors surfaced to synchronous (web UI) writers whose command never
// reached the inverter. The MQTT command path only logs them.
var (
	// ErrWriteQueueFull means the write queue stayed full even after
	// making room, so the command was not accepted.
	ErrWriteQueueFull = errors.New("coordinator: write queue full")
	// ErrWriteSuperseded means the queued command was dropped to make
	// room for a newer one — the newer command is the one that runs.
	ErrWriteSuperseded = errors.New("coordinator: write superseded by a newer command")
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
// publishes through. Matches the interface in the go-mqtt module verbatim
// so the real client satisfies it for free.
type MQTTPublisher interface {
	Publish(ctx context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool, opts ...mqtt.PublishOption) error
}

// MQTTSubscriber is the subset of [*mqtt.TCPClient] used to receive
// inbound HA command messages.
type MQTTSubscriber interface {
	Subscribe(ctx context.Context, filter string, qos mqtt.QoS, handler mqtt.MessageHandler, opts ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error)
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
	// Store, when non-nil, receives a copy of every published group so
	// the optional web UI can render live values. Nil disables caching
	// (the pure-MQTT default path).
	Store *state.Store
	// Virtual lists synthetic switch entities the coordinator implements
	// (charge/discharge "active"). Their on/off state is derived from the
	// target register on every poll and their writes toggle that register
	// between the configured value and 0. Empty disables the feature.
	Virtual []hass.VirtualSwitch
}

// Coordinator is the M-TEC → MQTT data-flow root.
type Coordinator struct {
	deps Deps

	// initialised in Run after the first STATIC read succeeds.
	// serialNo/firmware/equipmentInfo are only accessed on the Run
	// goroutine; topicBase is additionally read from the MQTT dispatch
	// goroutine (onMessage) while Run may still be writing it, so it is
	// stored atomically — use loadTopicBase to read it.
	serialNo      string
	firmware      string
	equipmentInfo string
	topicBase     atomic.Value // string

	// startedAt stamps construction so the web health view can report
	// uptime. Set from Deps.Now in New.
	startedAt time.Time

	secondaryIdx  atomic.Int32
	discoverySent atomic.Bool
	// discoveryGen counts Home Assistant birth announcements.
	// publishDiscovery samples it before its first publish and only marks
	// discovery as sent when it is unchanged afterwards, so a birth that
	// arrives mid-publish is not swallowed by the final Store(true).
	discoveryGen atomic.Uint64
	writeQueue   chan writeReq
	// writeWorkerUp reports whether writeWorker is draining the queue.
	// The synchronous web write path waits for a queued command's reply,
	// which only ever arrives while the worker runs — before Run (or
	// after it returned) that path dispatches inline instead of blocking
	// on a queue nobody reads.
	writeWorkerUp atomic.Bool

	// hassStatusTopic caches "<hass_base>/status" so the message
	// handler can compare topic strings without rebuilding it on
	// every inbound publish.
	hassStatusTopic string

	// virtualByKey indexes the synthetic switches by their MQTT key for
	// write routing. lastActive remembers the last non-zero value seen on
	// each target register so a switch can restore it when toggled on;
	// guarded by lastActiveMu.
	virtualByKey map[string]hass.VirtualSwitch
	lastActiveMu sync.Mutex
	lastActive   map[string]float64

	// reconcileGate is a try-locked gate so only one discovery orphan
	// reconcile runs at a time; a re-entrant call while one is in flight
	// is skipped (discovery changes are infrequent). The zero value is
	// ready to use, so it needs no initialisation in New.
	reconcileGate sync.Mutex
}

// writeReq is one HA → device command pending dispatch.
type writeReq struct {
	mqttKey string
	value   string
	// reply, when non-nil, receives the dispatch outcome exactly once so
	// a synchronous caller (the web UI) can return it. Must be buffered
	// (capacity 1) — the worker never blocks on a caller that gave up.
	reply chan error
}

// replyTo delivers a result to a synchronous caller, if there is one.
// Non-blocking: the buffer holds the single value a caller can consume,
// and a caller whose context expired is simply gone.
func replyTo(req writeReq, err error) {
	if req.reply == nil {
		return
	}
	select {
	case req.reply <- err:
	default:
	}
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
	vbk := make(map[string]hass.VirtualSwitch, len(d.Virtual))
	for _, v := range d.Virtual {
		vbk[v.Key] = v
	}
	return &Coordinator{
		deps:            d,
		startedAt:       d.Now(),
		writeQueue:      make(chan writeReq, 32),
		hassStatusTopic: d.Cfg.HASSBaseTopic + "/status",
		virtualByKey:    vbk,
		lastActive:      make(map[string]float64),
	}
}

// Run executes the full daemon loop:
//
//  1. Modbus connect (synchronous, retried with bounded backoff until
//     it succeeds or ctx is cancelled)
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
// The first stage runs serially so nothing polls before the transport
// is up; an unreachable inverter is retried (and logged) rather than
// treated as fatal, because at boot a wrong IP and a gateway that is
// still coming up look identical.
func (c *Coordinator) Run(ctx context.Context) error {
	log := c.deps.Logger

	log.Info("coordinator.starting",
		slog.String("modbus", c.deps.Cfg.ModbusIP),
		slog.String("mqtt", c.deps.Cfg.MQTTServer),
		slog.Bool("hass", c.deps.Cfg.HASSEnable))

	if err := c.connectModbus(ctx); err != nil {
		return err
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
		published := c.publishDiscovery(ctx)
		// Clear any of our own retained discovery configs that we no longer
		// publish (entities removed, renamed or re-platformed across catalog
		// or daemon versions), so they don't linger as unavailable entities
		// in Home Assistant.
		c.reconcileOrphans(ctx, published)
	}

	g, runCtx := errgroup.WithContext(ctx)
	c.spawnPolls(runCtx, g)
	g.Go(func() error { return c.writeWorker(runCtx) })
	g.Go(func() error { return c.modbusWatchdog(runCtx) })
	if c.deps.HASS != nil {
		g.Go(func() error { return c.discoveryRepublisher(runCtx) })
	}

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
	// A failing subscribe is retried with the same bounded backoff the
	// Modbus connect uses instead of killing the daemon: a broker that
	// is still booting alongside us looks exactly like a broker that
	// will never accept the filter, and losing the command path for the
	// process lifetime is the worse outcome. Only the filter that failed
	// is retried, so a successful one is never re-subscribed.
	for _, s := range subs {
		err := c.retryWithBackoff(ctx, "coordinator.subscribe_retry",
			func(ctx context.Context) error {
				_, err := c.deps.MQTT.Subscribe(ctx, s, mqtt.QoS1, c.onMessage)
				return err
			},
			slog.String("filter", s))
		if err != nil {
			return fmt.Errorf("coordinator: subscribe %s: %w", s, err)
		}
	}
	return nil
}

// startupBackoff / startupMaxBackoff bound retryWithBackoff's wait
// between attempts. Vars, not consts, so tests can shorten them.
var (
	startupBackoff    = time.Second
	startupMaxBackoff = 30 * time.Second
)

// retryWithBackoff runs fn until it succeeds or ctx is cancelled,
// backing off exponentially (1 s → 30 s) between attempts and logging
// each failure under event. Returns ctx.Err() on cancellation.
//
// Every startup step that talks to a network peer goes through here:
// at boot an unreachable peer and a peer that is still coming up are
// indistinguishable, so retrying beats treating the first error as
// fatal.
func (c *Coordinator) retryWithBackoff(ctx context.Context, event string, fn func(context.Context) error, attrs ...slog.Attr) error {
	backoff := startupBackoff
	maxBackoff := startupMaxBackoff
	for {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logAttrs := slices.Concat(attrs, []slog.Attr{
			slog.String("err", err.Error()),
			slog.Duration("retry_in", backoff),
		})
		c.deps.Logger.LogAttrs(ctx, slog.LevelWarn, event, logAttrs...)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// onMessage dispatches one inbound publish. Errors are logged and
// swallowed — the message loop must not exit because a single bad
// payload arrived.
func (c *Coordinator) onMessage(msg *mqtt.Message) {
	log := c.deps.Logger
	topic, payload := msg.Topic, msg.Payload
	if topic == c.hassStatusTopic {
		if string(payload) == "online" {
			log.Info("coordinator.hass_birth_seen")
			// Bump the generation first: a publishDiscovery already in
			// flight compares it against the value it sampled and refuses
			// to mark discovery as sent, so this birth cannot be lost in
			// the gap between our Store(false) and its Store(true).
			c.discoveryGen.Add(1)
			c.discoverySent.Store(false) // trigger republish next chance
		}
		return
	}
	// Expected shape: <topic>/<serial>/<group>/<mqtt_key>/set
	topicBase := c.loadTopicBase()
	if topicBase == "" {
		return // not initialised yet — drop silently
	}
	if !startsWith(topic, topicBase+"/") || !endsWith(topic, "/set") {
		return
	}
	// A retained delivery is the broker replaying a past command on
	// (re)subscribe, not a live request — writing it to the inverter on
	// every restart and reconnect would keep overriding settings the
	// user has since changed. Home Assistant never publishes commands
	// retained, so dropping these loses nothing.
	if msg.Retain {
		log.Warn("coordinator.retained_command_ignored",
			slog.String("topic", topic))
		return
	}
	// We want the second-to-last path segment as the MQTT key.
	parts := splitPath(topic)
	if len(parts) < 4 {
		return
	}
	mqttKey := parts[len(parts)-2]
	// Errors are already logged by enqueueWrite; the MQTT path has no
	// caller to report back to.
	_ = c.enqueueWrite(writeReq{mqttKey: mqttKey, value: string(payload)})
}

// enqueueWrite puts req on the write queue, making room by dropping the
// OLDEST pending command when the queue is full.
//
// Dropping the newest (what a plain non-blocking send does) strands the
// inverter on an intermediate value: dragging a Home Assistant slider
// emits a burst of commands, and the one that must survive is the value
// the user released it at — the last one. The oldest entry is the most
// stale, so it is the one worth losing.
//
// The attempt count is bounded because several producers (the MQTT
// dispatch goroutine, web handler goroutines) can race here; without
// the bound a pathological interleaving could spin.
func (c *Coordinator) enqueueWrite(req writeReq) error {
	const maxAttempts = 8
	log := c.deps.Logger
	for range maxAttempts {
		select {
		case c.writeQueue <- req:
			return nil
		default:
		}
		select {
		case dropped := <-c.writeQueue:
			log.Warn("coordinator.write_queue_full_dropped_oldest",
				slog.String("dropped_mqtt_key", dropped.mqttKey),
				slog.String("dropped_value", dropped.value),
				slog.String("mqtt_key", req.mqttKey))
			replyTo(dropped, ErrWriteSuperseded)
		default:
		}
	}
	log.Warn("coordinator.write_queue_full", slog.String("mqtt_key", req.mqttKey))
	return ErrWriteQueueFull
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

// connectModbus retries the initial Modbus connect with bounded
// exponential backoff until it succeeds or ctx is cancelled. Later
// disconnects are already retried forever by modbusWatchdog; the
// boot-time connect gets the same resilience so an inverter gateway
// that is still rejoining the network (e.g. power-outage recovery)
// doesn't kill the daemon. Cancellation returns ctx.Err().
func (c *Coordinator) connectModbus(ctx context.Context) error {
	return c.retryWithBackoff(ctx, "coordinator.modbus_connect_retry", c.deps.Modbus.Connect)
}

// waitForStatic blocks until the STATIC group yields a usable serial
// number or the context is cancelled. The Python coordinator retries
// indefinitely; we do the same but cap the per-retry wait so test
// teardown isn't dragged out for full backoff cycles.
func (c *Coordinator) waitForStatic(ctx context.Context) error {
	log := c.deps.Logger
	const retry = 10 * time.Second
	for {
		err := c.tryInitFromStatic(ctx)
		if err == nil {
			return nil
		}
		log.Warn("coordinator.static_init_retry",
			slog.String("err", err.Error()))
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
	processed := processValues(c.deps.Catalog, data, c.deps.Cfg.Language)
	serial, _ := processed["serial_no"].(string)
	firmware, _ := processed["firmware_version"].(string)
	equip, _ := processed["equipment_info"].(string)
	if serial == "" {
		return fmt.Errorf("coordinator: STATIC read missing serial_no")
	}
	// The serial becomes a level of every MQTT topic; a corrupt read
	// (or a wrong device answering on the M-TEC register map) with a
	// '/', wildcard or control character in it would poison the topic
	// tree for the process lifetime. Reject it so the caller retries.
	if !isTopicSafe(serial) {
		return fmt.Errorf("coordinator: STATIC serial_no %q is not usable as an MQTT topic level", serial)
	}
	c.serialNo = serial
	c.firmware = firmware
	c.equipmentInfo = equip
	topicBase := c.deps.Cfg.MQTTTopic + "/" + serial
	c.topicBase.Store(topicBase)
	if c.deps.Store != nil {
		c.deps.Store.SetStatic(serial, firmware, equip, c.deps.Now())
	}
	c.deps.Logger.Info("coordinator.static_initialised",
		slog.String("serial", serial),
		slog.String("firmware", firmware),
		slog.String("equipment", equip),
		slog.String("topic_base", topicBase))
	return nil
}

// loadTopicBase returns the "<mqtt_topic>/<serial>" topic prefix, or
// "" while static initialisation has not completed yet. Safe to call
// from any goroutine.
func (c *Coordinator) loadTopicBase() string {
	tb, _ := c.topicBase.Load().(string)
	return tb
}

// publishDiscovery sends every HA discovery payload with retain=true
// and subscribes to every writable entity's command topic so HA can
// drive the inverter back. Existing command-topic subscriptions are
// idempotent on the adapter — re-subscribing on reconnect is safe.
//
// It returns the set of config topics it advertised. A topic is included
// even when its publish fails, so a transient broker error never makes
// orphan reconciliation clear an entity we still intend to publish.
//
// discoverySent is only raised when the whole batch went out AND no
// Home Assistant birth arrived meanwhile. Marking it sent
// unconditionally left HA without any entities until the next daemon
// restart whenever the broker was unavailable at startup (an open
// circuit breaker publishes nothing at all), and let the final
// Store(true) overwrite the Store(false) a concurrent birth had just
// set. Leaving it false makes discoveryRepublisher try again in 5 s.
func (c *Coordinator) publishDiscovery(ctx context.Context) map[string]bool {
	if c.deps.HASS == nil {
		return nil
	}
	log := c.deps.Logger
	gen := c.discoveryGen.Load()
	entries := c.deps.HASS.Entries()
	published := make(map[string]bool, len(entries))
	failed := 0
	for _, e := range entries {
		published[e.ConfigTopic] = true
		if err := c.deps.MQTT.Publish(ctx, e.ConfigTopic, e.Payload, mqtt.QoS0, true); err != nil {
			failed++
			log.Warn("coordinator.discovery_publish",
				slog.String("topic", e.ConfigTopic),
				slog.String("err", err.Error()))
		}
	}
	birthRaced := c.discoveryGen.Load() != gen
	complete := failed == 0 && !birthRaced
	c.discoverySent.Store(complete)
	if !complete {
		log.Warn("coordinator.discovery_incomplete",
			slog.Int("entries", len(entries)),
			slog.Int("failed", failed),
			slog.Bool("birth_raced", birthRaced))
		return published
	}
	log.Info("coordinator.discovery_sent", slog.Int("entries", len(entries)))
	return published
}

// discoveryRepublisher re-sends the HA discovery configs after
// onMessage clears discoverySent on a Home Assistant birth message —
// the Python coordinator does the same so entities reappear even when
// the broker lost its retained config topics. publishDiscovery is
// idempotent (same retained payloads to the same topics), so an extra
// pass is harmless.
func (c *Coordinator) discoveryRepublisher(ctx context.Context) error {
	const tick = 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(tick):
		}
		if c.discoverySent.Load() {
			continue
		}
		c.deps.Logger.Info("coordinator.discovery_republish")
		c.publishDiscovery(ctx)
	}
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

// writeRetries / writeRetryDelay bound the writeWorker's retry of a
// transient failure. Three attempts a second apart comfortably span the
// watchdog's reconnect window without holding the queue hostage. Vars,
// not consts, so tests can shorten the delay.
var (
	writeRetries    = 3
	writeRetryDelay = time.Second
)

// writeWorker drains the queue of inbound commands (HA /set publishes
// and web UI writes), calling WriteRegisterByMQTT for each. We process
// sequentially because the Modbus client serialises wire transactions
// anyway, so parallelism here would only deepen the queue without
// speeding the inverter up.
func (c *Coordinator) writeWorker(ctx context.Context) error {
	log := c.deps.Logger
	c.writeWorkerUp.Store(true)
	defer c.writeWorkerUp.Store(false)
	for {
		select {
		case <-ctx.Done():
			return nil
		case req := <-c.writeQueue:
			err := c.dispatchWriteRetrying(ctx, req)
			replyTo(req, err)
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

// dispatchWriteRetrying performs one queued write, retrying a transient
// transport failure a few times. A command that lands in the watchdog's
// reconnect window (or any other momentarily poisoned connection) used
// to be dropped without a trace — Home Assistant showed the new value
// while the inverter kept the old one, until the next poll snapped the
// entity back.
func (c *Coordinator) dispatchWriteRetrying(ctx context.Context, req writeReq) error {
	log := c.deps.Logger
	var err error
	for attempt := 1; attempt <= writeRetries; attempt++ {
		err = c.dispatchWrite(ctx, req.mqttKey, req.value)
		if err == nil || !isTransientWriteErr(err) {
			return err
		}
		if attempt == writeRetries {
			break
		}
		log.Warn("coordinator.write_retry",
			slog.String("mqtt_key", req.mqttKey),
			slog.String("value", req.value),
			slog.Int("attempt", attempt),
			slog.String("err", err.Error()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(writeRetryDelay):
		}
	}
	return err
}

// isTransientWriteErr reports whether a failed write is worth another
// attempt. The reader's typed rejections (unknown register, read-only,
// unparseable value, pseudo-register) and context errors are final —
// retrying them only burns queue time. Everything else is a
// transport-level failure (a poisoned connection, a timed-out
// round-trip) and gets another shot.
func isTransientWriteErr(err error) bool {
	switch {
	case errors.Is(err, modbus.ErrUnknownRegister),
		errors.Is(err, modbus.ErrNotWritable),
		errors.Is(err, modbus.ErrValueParse),
		errors.Is(err, modbus.ErrPseudoUnsupported),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

// --- string helpers (avoid pulling "strings" for two predicates) ----------

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// isTopicSafe reports whether s can be embedded as a single MQTT topic
// level: no control characters (including NUL), no '/' level separator
// and no '+'/'#' wildcards, all of which either break topic matching
// or make every derived topic an invalid publish topic. Every real
// (alphanumeric) inverter serial passes.
func isTopicSafe(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == '/' || r == '+' || r == '#' {
			return false
		}
	}
	return true
}

func splitPath(s string) []string {
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
