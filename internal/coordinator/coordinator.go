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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"
	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
	"github.com/SukramJ/go-mtec2mqtt/internal/version"
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

	// HARuntime owns this daemon's Home Assistant plane: the retained
	// discovery configs it has published, the bridge availability marker
	// its Last Will clears, and the orphan sweep. Required.
	//
	// It is built at the composition root rather than here for a reason
	// that cannot be worked around: [publisher.Runtime.Will] has to be read
	// BEFORE the MQTT client is constructed, because the Last Will is part
	// of CONNECT. Having the runtime state the will is what makes the topic
	// the broker writes "offline" to and the topic every one of the 100
	// entities names as its availability source provably one string — a
	// will no entity references is the measured defect of two sibling
	// bridges, where a hard crash leaves every entity available forever,
	// showing the last value it ever saw.
	HARuntime *publisher.Runtime

	// StatePlane writes every register's retained state value. Required.
	//
	// Built at the composition root for the same reason as HARuntime and
	// for one of its own: the one thing it must be told is the quality of
	// service, and that is a statement about an installed base rather than
	// about this package. See [StateQoS].
	StatePlane *publisher.StatePublisher
}

// StateQoS is the delivery guarantee of every state publish this daemon
// makes, stated rather than defaulted.
//
// QoS 0, unchanged: every release of this bridge has published its state
// plane at QoS 0 (poll.go's publish call, pinned by
// TestPublishQoSAndRetain), and a migration step whose purpose is
// de-duplication is not the place to change the delivery guarantee of an
// installed base on the wire.
//
// It has to be SAID, and that is the trap this constant exists to close.
// [publisher.QoS]'s zero value is QoSUnset, which resolves to QoS 1 —
// so a StateConfig that simply omitted the field would have tripled this
// bridge's broker traffic silently, with a broker capture as the only
// evidence. [publisher.QoSAtMostOnce] is deliberately 0x80, outside the
// wire's 0-2 range, precisely so "unset" and "deliberately at most once"
// cannot be written the same way.
//
// Changing it is its own release with its own changelog line.
const StateQoS = publisher.QoSAtMostOnce

// CommandQoS is the delivery guarantee of this daemon's command
// subscription, likewise stated.
//
// QoS 1, unchanged and pinned by TestSubscribeQoS: a command dropped in
// transit is a button press in Home Assistant that did nothing, with
// nothing anywhere to explain it. It is spelled out beside [StateQoS]
// because the two are different answers to different questions and the
// library reads an omitted field as neither.
const CommandQoS = publisher.QoSAtLeastOnce

// HARuntimeConfig is the publisher.Config this daemon's Home Assistant
// plane runs on, in ONE place.
//
// It exists because the composition root and the test fixtures each used
// to spell it out, and a mutation pass found the consequence: dropping
// [publisher.Config.LegacyEntityTopics] from the composition root was
// caught by nothing at all, while every test went on exercising a runtime
// that still had it. That field is not a detail — omitting it makes the
// library retract the five-segment form this fleet is not on, which
// retracts nothing, publishes the device document into a tree still
// holding all 100 per-entity configs, and produces the silent refusal this
// whole release exists to avoid. Now there is one spelling and the tests
// run on it.
//
// The logger is a parameter because the daemon and the fixtures want
// different ones; everything else is derived.
func HARuntimeConfig(cfg *config.Config, logger *slog.Logger) publisher.Config {
	return publisher.Config{
		Prefix: cfg.HASSBaseTopic,
		Layout: hass.Layout{Root: cfg.MQTTTopic},
		QoS:    DiscoveryQoS,
		// The per-entity topic form this fleet's installed base is on,
		// stated rather than defaulted. publisher.Runtime.PublishBundle
		// reads it to decide which retained configs the device document
		// supersedes; naming a form REPLACES the library's five-segment
		// default rather than adding to it, which is the intent — 100 of
		// 100 of this fleet's retained configs are on the four-segment
		// form and the default matches 0.
		LegacyEntityTopics: hass.LegacyConfigTopicForms(),
		Logger:             logger,
	}
}

// DiscoveryQoS is the delivery guarantee of every retained discovery
// config publish and of the bridge availability marker.
//
// QoS 0, unchanged and pinned by TestPublishQoSAndRetain. It is the
// library's [publisher.Config] QoS, which also governs the sweep's
// snapshot subscription — the reason that window subscribes at QoS 0
// where this daemon's hand-rolled reconcile did the same.
const DiscoveryQoS = publisher.QoSAtMostOnce

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

	// haBundle is the one retained device document this daemon publishes,
	// rendered once after the STATIC read that supplies the serial the
	// node id and the device block are keyed on. Nil means the document
	// could not be built or did not validate, and publishDiscovery then
	// publishes NOTHING — see buildBundle.
	haBundle *discovery.Bundle
	// haBundleTopic caches haBundle's config topic so the orphan sweep's
	// claim set does not have to re-derive it.
	haBundleTopic string
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

	// commands routes inbound /set publishes. It is built here rather than
	// at the composition root because the handler it dispatches to is a
	// method of this type; what the composition root would otherwise own —
	// the QoS — is stated once, in [CommandQoS].
	commands *publisher.CommandRouter
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
	if d.HARuntime == nil {
		panic("coordinator: Deps.HARuntime is required; build it at the composition root so Will() can be read before CONNECT")
	}
	if d.StatePlane == nil {
		panic("coordinator: Deps.StatePlane is required; build it at the composition root so the state QoS is stated there")
	}
	c := &Coordinator{
		deps:            d,
		startedAt:       d.Now(),
		writeQueue:      make(chan writeReq, 32),
		hassStatusTopic: d.Cfg.HASSBaseTopic + "/status",
		virtualByKey:    vbk,
		lastActive:      make(map[string]float64),
	}
	// The router subscribes and dispatches; it never publishes, so it goes
	// straight to the client rather than through whatever breaker the
	// composition root put in front of the publish half. Breaking the
	// subscribe path would only delay resubscription after a reconnect.
	c.commands = publisher.NewCommandRouter(hagomqtt.Split(d.MQTT, d.MQTT), publisher.CommandConfig{
		QoS: CommandQoS,
		// One route, one worker: this daemon serialises every write behind
		// its own drop-oldest queue anyway (see enqueueWrite), and the
		// Modbus client serialises the wire transactions behind that, so a
		// second worker would only deepen a queue without speeding the
		// inverter up.
		Workers: 1,
		// Unchanged, and the default is the behaviour this daemon already
		// had by hand: a retained delivery on a command topic is the broker
		// replaying a past command on (re)subscribe, not a live request.
		// Writing it to the inverter on every restart and reconnect would
		// keep overriding settings the user has since changed. Home
		// Assistant never publishes commands retained, so dropping them
		// loses nothing.
		DeliverRetained: false,
		Logger:          d.Logger,
	})
	return c
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

	// The self-echo guard, now that every state topic's serial is known.
	if err := c.checkCommandDisjoint(); err != nil {
		return err
	}

	if c.deps.HASS != nil {
		c.deps.HASS.Initialize(c.serialNo, c.firmware, c.equipmentInfo)
		// A register asking for a platform the builder cannot emit would
		// otherwise be polled and published with no entity ever appearing
		// and nothing in the log to say why.
		for _, diag := range c.deps.HASS.Diagnostics() {
			c.deps.Logger.Warn("coordinator.discovery_diag", slog.String("note", diag))
		}
		c.buildBundle()
		published := c.publishDiscovery(ctx)
		// Clear any of our own retained discovery configs that we no longer
		// publish (entities removed, renamed or re-platformed across catalog
		// or daemon versions), so they don't linger as unavailable entities
		// in Home Assistant.
		//
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

	// Stop the command plane before the availability marker goes to
	// "offline": a command accepted after this daemon has announced itself
	// gone would be executed by nobody and acknowledged by nothing.
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	c.stopCommands(stopCtx)
	stopCancel()

	// Context cancellation is the expected exit, not a failure.
	if err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("coordinator.stopped")
	return nil
}

// installInboundHandler wires this daemon's two inbound subscriptions:
// Home Assistant's own birth topic, and — through
// [publisher.CommandRouter] — every writable entity's /set topic.
//
// Two narrow filters rather than one fat '#' wildcard, so an unrelated
// topic on the broker cannot drive the daemon:
//
//	<hass_base>/status     → HA online/offline birth
//	<mqtt_topic>/+/+/+/set → the command plane
//
// The command half moved onto the library's router in ADR 0070 phase 6
// step 5. What that buys, beyond one fewer hand-rolled dispatch: the
// route's '+' levels arrive already split ([publisher.Command.Wildcards])
// instead of being recovered by index arithmetic — two of the measured
// consumer's confirmed defects were exactly that — handlers run on a
// router worker rather than the transport's read loop, retained commands
// are dropped by policy rather than by a hand-written check, and the
// subscription carries MQTT 5.0's No Local so this process cannot echo
// its own publishes back into its own handler. The filter, the QoS and
// the topic shape on the wire are unchanged.
//
// A failing subscribe is retried with the same bounded backoff the Modbus
// connect uses instead of killing the daemon: a broker that is still
// booting alongside us looks exactly like a broker that will never accept
// the filter, and losing the command path for the process lifetime is the
// worse outcome. Only the half that failed is retried.
func (c *Coordinator) installInboundHandler(ctx context.Context) error {
	if err := c.retryWithBackoff(ctx, "coordinator.subscribe_retry",
		func(ctx context.Context) error {
			_, err := c.deps.MQTT.Subscribe(ctx, c.hassStatusTopic, mqtt.QoS1, c.onMessage)
			return err
		},
		slog.String("filter", c.hassStatusTopic)); err != nil {
		return fmt.Errorf("coordinator: subscribe %s: %w", c.hassStatusTopic, err)
	}

	filter := hass.CommandFilter(c.deps.Cfg.MQTTTopic)
	if err := c.commands.Handle(filter, c.onCommand); err != nil {
		// A rejected route is a programming error (a malformed filter, a
		// duplicate, or an overlap with another route), not a broker
		// condition, so it is not retried.
		return fmt.Errorf("coordinator: route %s: %w", filter, err)
	}
	if err := c.retryWithBackoff(ctx, "coordinator.subscribe_retry",
		c.commands.Start,
		slog.String("filter", filter)); err != nil {
		return fmt.Errorf("coordinator: subscribe %s: %w", filter, err)
	}
	return nil
}

// checkCommandDisjoint refuses a boot in which anything this daemon
// publishes would land inside its own command subscription and be echoed
// straight back into [Coordinator.onCommand].
//
// This daemon had no equivalent guard. Checked by hand in the phase-6
// measurement and clean then — a five-segment state topic ending `state`
// against a five-segment filter ending `set`, a three-segment bridge
// topic, four-segment configs — but clean-by-inspection is what it was,
// and the inspection had to be redone every time a topic moved. It runs
// after the STATIC read because every state topic is keyed on the serial.
//
// It is a boot failure rather than a warning: a self-echo turns a state
// publish into a command, and the least diagnosable shape of that is a
// device that appears to change its own settings.
func (c *Coordinator) checkCommandDisjoint() error {
	topics := []string{hass.BridgeStatusTopic(c.deps.Cfg.MQTTTopic)}
	serial := c.serialNo
	for _, g := range c.deps.Catalog.Groups {
		for _, r := range c.deps.Catalog.ByGroup(g) {
			topics = append(topics, hass.StateTopic(c.deps.Cfg.MQTTTopic, serial, string(g), r.MQTT))
		}
	}
	for _, v := range c.deps.Virtual {
		topics = append(topics, hass.StateTopic(c.deps.Cfg.MQTTTopic, serial, v.Group, v.Key))
	}
	if err := c.commands.CheckDisjoint(topics...); err != nil {
		return fmt.Errorf("coordinator: command routes are not disjoint from what this daemon publishes: %w", err)
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

// onMessage handles one delivery on the Home Assistant birth topic.
//
// It no longer routes commands: those arrive through
// [publisher.CommandRouter] at [Coordinator.onCommand]. What is left is
// the birth edge, which this daemon deliberately keeps rather than moving
// onto publisher.Runtime.WatchBirth — see
// [Coordinator.discoveryRepublisher].
//
// Errors are logged and swallowed: the message loop must not exit because
// a single bad payload arrived.
func (c *Coordinator) onMessage(msg *mqtt.Message) {
	if msg.Topic != c.hassStatusTopic {
		return
	}
	if string(msg.Payload) != "online" {
		return
	}
	c.deps.Logger.Info("coordinator.hass_birth_seen")
	// Bump the generation first: a publishDiscovery already in flight
	// compares it against the value it sampled and refuses to mark
	// discovery as sent, so this birth cannot be lost in the gap between
	// our Store(false) and its Store(true).
	c.discoveryGen.Add(1)
	c.discoverySent.Store(false) // trigger republish next chance
}

// onCommand handles one routed /set publish.
//
// The route is "<root>/+/+/+/set", so [publisher.Command.Wildcards] is
// exactly [serial, group, mqtt_key] — the topic arithmetic this daemon
// used to do by hand with splitPath and a negative index. The serial is
// checked against this process's own rather than ignored: one broker can
// carry several inverters, and a command addressed to a sibling's serial
// is not this daemon's to execute.
//
// It runs on a router worker, not the transport's read loop, and it still
// hands the write to the bounded drop-oldest queue rather than touching
// Modbus inline — the queue's policy (drop the OLDEST pending command) is
// what keeps a dragged Home Assistant slider ending on the value the user
// released it at, and the router's own FIFO does not have it.
func (c *Coordinator) onCommand(_ context.Context, cmd publisher.Command) {
	topicBase := c.loadTopicBase()
	if topicBase == "" {
		return // not initialised yet — drop silently
	}
	if len(cmd.Wildcards) != 3 {
		return
	}
	serial, mqttKey := cmd.Wildcards[0], cmd.Wildcards[2]
	if c.deps.Cfg.MQTTTopic+"/"+serial != topicBase {
		return // addressed to another inverter on the same broker
	}
	// Errors are already logged by enqueueWrite; the MQTT path has no
	// caller to report back to.
	_ = c.enqueueWrite(writeReq{mqttKey: mqttKey, value: string(cmd.Payload)})
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
	c.topicBase.Store(topicParts{root: c.deps.Cfg.MQTTTopic, serial: serial})
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

// topicParts is the "<mqtt_topic>" root and the inverter serial, stored
// together because every state topic is rendered from both.
//
// They travel as one value rather than as two atomics: the serial is
// written once by Run and read by the poll goroutines, and a reader that
// could observe a new serial against an old root would publish under a
// topic tree that never existed.
type topicParts struct{ root, serial string }

// loadTopicBase returns the "<mqtt_topic>/<serial>" topic prefix, or
// "" while static initialisation has not completed yet. Safe to call
// from any goroutine.
func (c *Coordinator) loadTopicBase() string {
	tp, ok := c.loadTopicParts()
	if !ok {
		return ""
	}
	return tp.root + "/" + tp.serial
}

// loadTopicParts returns the two levels every state and command topic is
// built from, and whether static initialisation has completed. Safe to
// call from any goroutine.
func (c *Coordinator) loadTopicParts() (topicParts, bool) {
	tp, ok := c.topicBase.Load().(topicParts)
	return tp, ok
}

// buildBundle renders the one retained device document this daemon
// publishes, and refuses to publish anything at all if it does not hold up.
//
// It runs once, after [hass.Discovery.Initialize], because the node id and
// the device block are both keyed on the serial the STATIC read supplies.
//
// # Why a failure here withholds the whole migration
//
// Publishing the document retracts the 100 per-entity configs first, and
// that retraction is not reversible by this process: between the two the
// entities are ABSENT rather than unavailable. So the one outcome worse
// than not migrating is migrating into a document Home Assistant then
// discards — which it does in silence, with no line on the wire and one
// WARNING in its own log. Leaving haBundle nil means publishDiscovery
// publishes nothing, nothing is retracted, and the installed base keeps
// the per-entity configs it already has and goes on working.
//
// discovery.Validate is therefore run here rather than on the publish path.
// The library warns that wiring it into a publish path is fail-closed — one
// bad component costs a device all of its entities — and that is precisely
// the trade being made on purpose: the alternative failure mode is the same
// loss plus a retraction that cannot be undone.
//
// The failure is deterministic (the catalogue is compiled in), so
// discoverySent is left true and discoveryRepublisher does not spin on it.
func (c *Coordinator) buildBundle() {
	log := c.deps.Logger
	bundle, err := hass.RenderBundle(c.deps.HASS, hass.BundleOrigin(version.Version))
	if err != nil {
		log.Error("coordinator.discovery_bundle_render", slog.String("err", err.Error()))
		c.discoverySent.Store(true)
		return
	}
	if err := discovery.Validate(bundle); err != nil {
		var verr *discovery.ValidationError
		if errors.As(err, &verr) && !verr.Blocking() {
			// Warnings are keys Home Assistant accepts and then rewrites.
			// They are worth saying out loud and are not worth withholding
			// a hundred entities over.
			log.Warn("coordinator.discovery_bundle_warnings",
				slog.String("node_id", bundle.NodeID),
				slog.Any("warnings", verr.Warnings))
		} else {
			log.Error("coordinator.discovery_bundle_invalid",
				slog.String("node_id", bundle.NodeID),
				slog.String("err", err.Error()))
			c.discoverySent.Store(true)
			return
		}
	}
	c.haBundle = bundle
	c.haBundleTopic = hass.BundleConfigTopic(c.deps.HARuntime.Prefix(), c.deps.HASS)
	log.Info("coordinator.discovery_bundle_built",
		slog.String("topic", c.haBundleTopic),
		slog.Int("components", len(bundle.Components)))
}

// publishDiscovery writes the retained device document — ONE message
// carrying all 100 entities — and returns the set of config topics this
// daemon claims, for the orphan sweep.
//
// # The ordering this function exists to get right
//
// Home Assistant refuses a device document while a per-entity config for
// one of its components' unique_ids is still retained, and it refuses the
// per-entity config while the document is retained: the refusal is
// symmetric, silent on the wire, and shows up as exactly one line —
// "WARNING [mqtt.entity] Received a conflicting MQTT discovery message".
// The entities simply do not appear. Measured on Home Assistant 2026.9 on
// 2026-09-10/11.
//
// So the order is always retract first, publish second, and the retraction
// must be COMPLETE before the document goes out. publisher.Runtime's
// PublishBundle does both in that order: it renders the superseded topics
// from the document's own components through the form stated in
// publisher.Config.LegacyEntityTopics (hass.LegacyConfigTopicForms, the
// four-segment shape 100 of 100 of this fleet's configs are on), publishes
// a retained empty payload to each, and ABORTS before the document if one
// of them fails.
//
// "Complete" at [DiscoveryQoS] — QoS 0 — means the bytes have been written
// and flushed to the broker's socket, in order, ahead of the document's.
// There is no acknowledgement at QoS 0 and this code does not pretend there
// is one; what it relies on is MQTT's ordering guarantee for equal-QoS
// publishes on one connection, which is the property that actually matters:
// the document cannot reach the broker down a path its retractions did not
// already travel. A connection that dies in between loses both, and the
// next connect rebuilds from scratch. TestRetractionsPrecedeTheBundle and
// TestAFailedRetractionWithholdsTheBundle pin both halves.
//
// The document is written even when it is byte-identical to the retained
// one only on the first publish of a process: the runtime dedups, and on a
// birth-triggered republish of an unchanged fleet that costs zero broker
// writes and zero retractions.
func (c *Coordinator) publishDiscovery(ctx context.Context) map[string]bool {
	if c.deps.HASS == nil || c.haBundle == nil {
		return nil
	}
	log := c.deps.Logger
	gen := c.discoveryGen.Load()

	// The claim set the sweep subtracts. It names the document even when
	// the publish below fails, so a transient broker error never makes the
	// sweep clear a config this daemon still intends to publish.
	published := map[string]bool{c.haBundleTopic: true}

	written, err := c.deps.HARuntime.PublishBundle(ctx, c.haBundle)
	if err != nil {
		log.Warn("coordinator.discovery_publish",
			slog.String("topic", c.haBundleTopic),
			slog.String("err", err.Error()))
		c.discoverySent.Store(false)
		log.Warn("coordinator.discovery_incomplete",
			slog.Int("entries", len(c.haBundle.Components)),
			slog.Int("failed", 1),
			slog.Bool("birth_raced", false))
		return published
	}
	birthRaced := c.discoveryGen.Load() != gen
	c.discoverySent.Store(!birthRaced)
	if birthRaced {
		log.Warn("coordinator.discovery_incomplete",
			slog.Int("entries", len(c.haBundle.Components)),
			slog.Int("failed", 0),
			slog.Bool("birth_raced", true))
		return published
	}
	// written is reported because the gap between it and the component
	// count is the measurable effect of the dedup gate: a birth-triggered
	// republish of an unchanged fleet should show written=false.
	log.Info("coordinator.discovery_sent",
		slog.Int("entries", len(c.haBundle.Components)),
		slog.Bool("written", written))
	return published
}

// discoveryRepublisher re-sends the HA discovery configs after
// onMessage clears discoverySent on a Home Assistant birth message —
// the Python coordinator does the same so entities reappear even when
// the broker lost its retained config topics. publishDiscovery is
// idempotent (same retained payloads to the same topics), so an extra
// pass is harmless.
//
// This is deliberately NOT replaced by publisher.Runtime.WatchBirth,
// which the phase-6 measurement's step 5 offered as a candidate. Three
// reasons, all of which point the same way:
//
//   - WatchBirth subscribes at the runtime's own QoS, which here is
//     [DiscoveryQoS] (0). This daemon subscribes <hass_base>/status at
//     QoS 1 and has since its first release, pinned by TestSubscribeQoS;
//     downgrading a birth subscription inside a step whose claim is that
//     nothing moved is exactly the trade this programme keeps refusing.
//   - The generation counter. publishDiscovery samples discoveryGen
//     before its first publish and only marks the batch sent when it is
//     unchanged afterwards, so a birth arriving mid-publish is not
//     swallowed by the final Store(true). WatchBirth has no equivalent
//     and no seam to add one.
//   - This loop retries a FAILED batch every 5 s, not only on a birth. An
//     open circuit breaker at startup publishes nothing at all, and
//     WatchBirth would then leave Home Assistant without entities until
//     the next birth — which, if Home Assistant was already up, never
//     comes.
//
// What did move onto the library is the writer underneath
// (publishDiscovery publishes through publisher.Runtime), so the replay
// still feeds the claim set the sweep reads.
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

// PublishOnline (re)announces this daemon's availability and reopens the
// state plane's dedup gate. Wired to the MQTT lifecycle's OnConnect hook,
// so it runs on every (re)connect and not only at boot.
//
// The will only fires on ungraceful death; without a matching birth
// publish a single network blip would leave the retained availability
// topic stuck at "offline" for the rest of the daemon's uptime.
//
// The Reset is the half that is new rather than preserved. A (re)connect
// may be to a broker that came back without its retained store, in which
// case the dedup gate would suppress every value it believes is already
// there and leave every entity blank until its next change — which for
// the `static` and `total` groups is effectively never. Reset opens the
// gate without forgetting the index, so the next poll writes the fleet
// once and is deduped again afterwards. The poll cycle is the snapshot
// pass the library's Reset documentation asks a consumer to pair it with.
func (c *Coordinator) PublishOnline(ctx context.Context) {
	c.deps.StatePlane.Reset()
	if err := c.deps.HARuntime.AnnounceOnline(ctx); err != nil {
		c.deps.Logger.Warn("coordinator.online_failed", slog.String("err", err.Error()))
	}
}

// PublishOffline marks this daemon unavailable on a clean shutdown. The
// Last Will only fires on an ungraceful disconnect, so a graceful
// DISCONNECT must announce offline explicitly or the retained status
// stays "online" for a daemon that is gone.
func (c *Coordinator) PublishOffline(ctx context.Context) {
	if err := c.deps.HARuntime.AnnounceOffline(ctx); err != nil {
		c.deps.Logger.Warn("coordinator.offline_failed", slog.String("err", err.Error()))
	}
}

// stopCommands takes the command subscription down. Failures are logged:
// a broker that will not accept the UNSUBSCRIBE is about to lose the
// connection anyway.
func (c *Coordinator) stopCommands(ctx context.Context) {
	if err := c.commands.Stop(ctx); err != nil {
		c.deps.Logger.Warn("coordinator.command_stop_failed", slog.String("err", err.Error()))
	}
}
