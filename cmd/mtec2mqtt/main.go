// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Command mtec2mqtt is the M-TEC Energybutler → MQTT bridge daemon.
//
// It loads its configuration from $XDG_CONFIG_HOME/aiomtec2mqtt/config.yaml
// (or a path supplied via --config), opens a Modbus-TCP connection to
// the inverter, opens an MQTT session to the configured broker, and
// publishes register values plus Home Assistant auto-discovery
// payloads on the schedule defined by the REFRESH_* config keys.
//
// The daemon installs SIGINT/SIGTERM handlers so a `Ctrl-C` or systemd
// stop cleanly cancels every in-flight transaction before exiting.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"
	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/coordinator"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
	"github.com/SukramJ/go-mtec2mqtt/internal/version"
	"github.com/SukramJ/go-mtec2mqtt/internal/web"
)

const (
	registersFilename = "registers.yaml"
	clientIDBase      = "mtec2mqtt-"
)

func main() {
	configPath := flag.String("config", "",
		"explicit config.yaml path (defaults to the standard search order)")
	registersPath := flag.String("registers", "",
		"explicit registers.yaml path (defaults next to the binary)")
	showVersion := flag.Bool("version", false, "print build info and exit")
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local web UI health endpoint and exit 0 (healthy) or 1; container HEALTHCHECK hook")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}
	if *healthcheck {
		os.Exit(runHealthcheck(*configPath))
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	logger.Info("mtec2mqtt.boot", slog.String("build", version.String()))

	if err := run(*configPath, *registersPath, logger); fatalErr(err) {
		logger.Error("mtec2mqtt.fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// fatalErr reports whether err is a genuine failure rather than the
// normal outcome of a cancelled run context. A SIGINT/SIGTERM that
// lands during the startup phase (HASS birth gracetime, static-read
// retries, connect retries) surfaces as context.Canceled; exiting 1
// for that would make systemd record a clean `systemctl stop` as a
// unit failure.
func fatalErr(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled)
}

// run is the testable entry point: returns a non-nil error on any
// startup or runtime failure, nil on clean shutdown.
func run(configPath, registersPath string, logger *slog.Logger) error {
	// --- config ---
	cfg, err := loadConfig(configPath, logger)
	if err != nil {
		return err
	}
	if cfg.Debug {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		slog.SetDefault(logger)
	}

	// --- registers ---
	catalog, err := loadCatalog(registersPath, logger)
	if err != nil {
		return err
	}

	// --- ctx wired to SIGINT/SIGTERM so the daemon shuts down on a
	//     normal stop signal without leaving the inverter holding
	//     half-open Modbus sockets.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- modbus ---
	modbusClient := modbus.New(modbus.Config{
		Host:    cfg.ModbusIP,
		Port:    cfg.ModbusPort,
		UnitID:  cfg.ModbusSlave,
		Timeout: cfg.ModbusTimeoutDuration(),
		Logger:  logger,
	})
	reader := modbus.NewReader(modbusClient, catalog)

	// --- home assistant runtime (LWT, retained configs, sweep) ---
	//
	// Built BEFORE the MQTT client, because the Last Will is part of
	// CONNECT and the will is this runtime's statement: Will() returns the
	// same topic and the same two payloads AnnounceOnline and
	// AnnounceOffline write, and publisher.Config refuses a StatusTopic
	// that disagrees with its Layout's Bridge(). So the bridge structurally
	// cannot configure a will no published entity references — the measured
	// defect of two sibling bridges, where a hard crash writes "offline"
	// where nothing reads it and every entity in Home Assistant stays
	// available forever, showing the last value it ever saw.
	//
	// The status topic is hass.BridgeStatusTopic, the SAME function the
	// discovery builder points all 100 entities at. It is deliberately NOT
	// under the serial: the marker is published at CONNECT, before the
	// STATIC read that learns the serial number.
	//
	// The client it publishes through does not exist yet, so the transport
	// is wired in below, before anything connects.
	// A FACTORY, not an instance. publisher.Runtime remembers what it has
	// published and — decisively — which per-entity configs it has already
	// retracted to clear the way for the device document. That memory is
	// per process, while the QoS 0 success it records is per CONNECTION, so
	// a runtime that outlived a dropped connection would skip retractions
	// the broker never applied and publish the document into a conflict
	// Home Assistant refuses in silence. The coordinator therefore builds a
	// fresh one on every (re)connect; see coordinator.Coordinator.resetHAPlane.
	//
	// Every runtime this returns answers Will() identically — the will is
	// derived from the config and from nothing else — which is what lets
	// the will be read here, before the MQTT client exists, and still be
	// the same statement the runtime makes later.
	haLink := &deferredTransport{}
	haConfig := coordinator.HARuntimeConfig(cfg, logger)
	newHARuntime := func() *publisher.Runtime { return publisher.New(haLink, haConfig) }

	// One instance built here, for the two answers that are pure config and
	// are needed before the coordinator exists: the Last Will (part of
	// CONNECT) and the state plane's transport and logger. It publishes no
	// discovery config of its own and holds none of the per-connection
	// memory above.
	bootRuntime := newHARuntime()
	defer bootRuntime.Close()
	will, err := bootRuntime.Will()
	if err != nil {
		return fmt.Errorf("mtec2mqtt: mqtt will: %w", err)
	}

	// --- mqtt ---
	clientID := clientIDBase + cfg.MQTTTopic
	// Releases up to 1.9.0 wrote the availability marker to
	// "<hass_base>/status/lwt" — homeassistant/status/lwt by default,
	// inside Home Assistant's own birth tree. Nothing read it, so nothing
	// ever surfaced that it was in the wrong place. It moved; the retained
	// copy an upgrading broker still holds has to be cleared, or it sits
	// there forever claiming a daemon that no longer publishes it is
	// online.

	// TLS is opt-in via MQTT_SSL; NewClientTLSConfig always sets
	// ServerName (tls.Client does not infer it from the dialed address)
	// and only disables certificate verification when the operator has
	// explicitly set MQTT_SSL_INSECURE — never by default.
	var tlsConfig *tls.Config
	if cfg.MQTTSSL {
		tlsConfig = mqtt.NewClientTLSConfig(cfg.MQTTServer, cfg.MQTTSSLInsecure)
	}
	mqttClient := mqtt.NewTCPClient(mqtt.TCPConfig{
		BrokerURL:  cfg.MQTTBrokerURL(),
		ClientID:   clientID,
		Username:   cfg.MQTTLogin,
		Password:   cfg.MQTTPassword,
		KeepAlive:  60 * time.Second,
		CleanStart: true,
		// Every field from Will(), none of them a literal here. A literal
		// would be a second statement of the availability policy that has
		// to agree with the library's, and "has to agree" is how the two
		// sibling bridges got theirs wrong.
		Will: &mqtt.Will{
			Topic:   will.Topic,
			Payload: will.Payload,
			QoS:     mqtt.QoS(will.QoS),
			Retain:  will.Retain,
		},
		TLSConfig: tlsConfig,
		Logger:    logger,
	})
	mqttLifecycle := mqtt.NewLifecycle(mqtt.DefaultLifecycle(), mqttClient)
	// Circuit breaker between the coordinator and the broker: during a
	// degraded-broker phase (TCP link up, acks missing) publishes fail
	// fast with mqtt.ErrCircuitOpen instead of each stalling on the ack
	// timeout, and bounded half-open probes test recovery. Defaults: 5
	// consecutive broker-side failures open the circuit, recovery is
	// probed after 30s. The lifecycle's reconnect loop stays in charge
	// of the link itself.
	breaker := mqtt.NewBreaker(mqttClient, mqtt.BreakerConfig{
		OnStateChange: func(from, to mqtt.BreakerState) {
			logger.Warn("mtec2mqtt.mqtt_breaker_state",
				slog.String("from", from.String()),
				slog.String("to", to.String()))
		},
	})
	// The runtime publishes through the breaker and subscribes around it,
	// for the same reason the coordinator's client is split: breaking the
	// subscribe path would only delay resubscription after a reconnect
	// without preventing anything.
	haLink.wire(hagomqtt.Split(breaker, mqttClient))

	// --- state plane ---
	//
	// StateFor inherits the runtime's transport and logger; what it does
	// NOT inherit is a QoS it was never told, which is the whole point of
	// stating coordinator.StateQoS. publisher.QoS's zero value means
	// *unset* and resolves to QoS 1; this bridge has published its entire
	// state plane at QoS 0 since its first release, and a step whose
	// purpose is de-duplication is not where an installed base's delivery
	// guarantee changes on the wire.
	//
	// CommandFilters is the one thing the library can check that this
	// bridge could not: a state topic falling inside this process's own
	// /set subscription would be echoed back into its own command handler.
	// The filter is stated once, in hass.CommandFilter, and read by the
	// router route and by this guard.
	statePlane := publisher.StateFor(bootRuntime, publisher.StateConfig{
		QoS:            coordinator.StateQoS,
		Encoding:       discovery.RawEncoding,
		CommandFilters: []string{hass.CommandFilter(cfg.MQTTTopic)},
		Logger:         logger,
	})

	// --- hass discovery (optional) ---
	// The synthetic charge/discharge "active" switches are defined once
	// and shared: the discovery builder advertises them to HA while the
	// coordinator implements their toggle/restore write logic.
	virtualSwitches := hass.DefaultVirtualSwitches(cfg.ChargeActiveValue, cfg.DischargeActiveValue)
	var hassDiscovery *hass.Discovery
	if cfg.HASSEnable {
		hassDiscovery = hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, catalog, cfg.Language, virtualSwitches, cfg.DeviceName)
		hassDiscovery.IncludeSerialInUniqueIDs(cfg.HassUniqueIDIncludeSerial)
	}

	// --- web ui (optional) ---
	// The store is only allocated when the UI is on; without it the
	// coordinator skips value caching entirely and stays a pure MQTT
	// bridge.
	var store *state.Store
	if cfg.WebEnable {
		store = state.New()
	}

	// --- coordinator ---
	c := coordinator.New(coordinator.Deps{
		Cfg:     cfg,
		Catalog: catalog,
		Modbus:  modbusClient,
		Reader:  reader,
		// Publish is gated by the circuit breaker, while
		// Subscribe/Unsubscribe go straight to the client —
		// subscriptions are startup-path calls with their own
		// SUBACK-bounded wait and must not be rejected during a
		// publish-side broker brownout.
		MQTT:         mqtt.SplitClient(breaker, mqttClient),
		HASS:         hassDiscovery,
		Logger:       logger,
		Store:        store,
		Virtual:      virtualSwitches,
		NewHARuntime: newHARuntime,
		StatePlane:   statePlane,
		// The broker's advertised Maximum Packet Size, renegotiated on every
		// CONNACK, so the device document's size is checked before the
		// retraction that cannot be undone rather than by the publish that
		// follows it. Absent on an MQTT 3.1.1 link and on a broker that sets
		// no limit, and absent is not small: the preflight is then skipped.
		BrokerMaxPacketSize: func() (uint32, bool) {
			res, ok := mqttClient.ConnectResult()
			if !ok {
				return 0, false
			}
			return res.MaximumPacketSize, true
		},
	})

	// Registered before Start so the first connect announces too. The
	// will only fires on ungraceful death; without a matching birth
	// publish a single network blip would leave the retained availability
	// topic stuck at "offline" for the rest of the daemon's uptime.
	mqttLifecycle.OnConnect(func(hookCtx context.Context) {
		retractLegacyAvailability(breaker, cfg.HASSBaseTopic, logger)(hookCtx)
		c.PublishOnline(hookCtx)
	})
	if err := startMQTT(ctx, mqttLifecycle, time.Second, 30*time.Second, logger); err != nil {
		return fmt.Errorf("mtec2mqtt: mqtt start: %w", err)
	}
	defer func() {
		// Graceful disconnect — bounded so a hung broker can't block
		// shutdown for more than a few seconds. A clean DISCONNECT
		// suppresses the broker-side will, so leave the retained
		// availability topic at "offline" ourselves first.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		c.PublishOffline(stopCtx)
		_ = mqttLifecycle.Stop(stopCtx)
	}()

	if !cfg.WebEnable {
		return c.Run(ctx)
	}

	// Run the coordinator and the web server together; either returning
	// (a fatal coordinator error or a web bind failure) cancels the other
	// via the shared context.
	webSrv := web.New(web.Config{
		Bind:     cfg.WebBind,
		User:     cfg.WebUser,
		Password: cfg.WebPassword,
		Logger:   logger,
	}, c)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.Run(gctx) })
	g.Go(func() error { return webSrv.Run(gctx) })
	return g.Wait()
}

// mqttStarter is the subset of [*mqtt.Lifecycle] that startMQTT
// drives, narrowed so tests can inject first-connect failures.
type mqttStarter interface {
	Start(ctx context.Context) error
}

// startMQTT retries the lifecycle's synchronous first broker connect
// with bounded exponential backoff until it succeeds or ctx is
// cancelled. [mqtt.Lifecycle.Start] makes exactly one connect attempt
// and only runs its reconnect loop after that first success, so a
// broker that is still booting (power-outage recovery: mosquitto and
// this daemon starting simultaneously) must be retried here instead
// of being treated like a fatal configuration error.
func startMQTT(ctx context.Context, lc mqttStarter, backoff, maxBackoff time.Duration, logger *slog.Logger) error {
	for {
		err := lc.Start(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn("mtec2mqtt.mqtt_start_retry",
			slog.String("err", err.Error()),
			slog.Duration("retry_in", backoff))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// retractLegacyAvailability clears the retained availability marker this
// daemon wrote before the status topic moved out of Home Assistant's
// discovery tree ("<hass_base>/status/lwt", by default
// homeassistant/status/lwt). An empty retained payload is MQTT's
// retraction: the broker drops the stored message, so a subscriber that
// connects later finds nothing rather than a stale "online" for a topic
// this daemon no longer maintains.
//
// It runs on every connect rather than once at first boot: the daemon
// keeps no state across restarts, the publish is one empty message, and a
// broker that lost its retained set in the meantime (a restart without
// persistence) would otherwise keep a stale copy from some other source
// forever. Failures are logged, never fatal — like the birth publish, it
// is best-effort.
//
// It takes the discovery PREFIX and derives the topic through
// [hass.LegacyAvailabilityTopic], rather than taking the topic. The topic
// used to be composed at the call site above, where no test reached it:
// the test passed a literal in and asserted publication to the same
// literal, which is argument forwarding, so mutating the production
// spelling to "/status/LWT" left the suite green while the retraction
// cleared a topic no release ever wrote. The arithmetic now lives inside
// the function the test drives.
func retractLegacyAvailability(pub coordinator.MQTTPublisher, hassBaseTopic string, logger *slog.Logger) func(context.Context) {
	topic := hass.LegacyAvailabilityTopic(hassBaseTopic)
	return func(ctx context.Context) {
		if err := pub.Publish(ctx, topic, nil, mqtt.QoS0, true); err != nil {
			logger.Warn("mtec2mqtt.legacy_lwt_retract_failed",
				slog.String("topic", topic),
				slog.String("err", err.Error()))
		}
	}
}

// runHealthcheck implements the container HEALTHCHECK hook: it loads
// the same config the daemon runs with and probes the web UI's
// /api/health endpoint. Exit 0 means healthy — or "no signal
// available" (web UI disabled, config unreadable), so the hook stays a
// no-op instead of flapping a container whose operator turned the UI
// off. Exit 1 means the daemon should be serving but does not answer.
func runHealthcheck(configPath string) int {
	cfg, err := loadConfig(configPath, slog.New(slog.DiscardHandler))
	if err != nil || !cfg.WebEnable {
		return 0
	}
	host, port, err := net.SplitHostPort(cfg.WebBind)
	if err != nil {
		return 0
	}
	// A wildcard bind is reachable via loopback from inside the
	// container; an explicit host is probed as configured.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+net.JoinHostPort(host, port)+"/api/health", http.NoBody)
	if err != nil {
		return 1
	}
	if cfg.WebUser != "" && cfg.WebPassword != "" {
		req.SetBasicAuth(cfg.WebUser, cfg.WebPassword)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}

// loadConfig finds and parses the daemon's YAML config. An explicit
// --config flag overrides the standard search order so the daemon can
// run from anywhere without relying on env vars.
//
// When no config file is supplied or located, the config is built from
// MTEC_* environment variables and defaults alone. The Home Assistant
// add-on (and env-only `docker run`) drives every setting via MTEC_*
// env and ships no file, so a missing file must not be fatal — Validate
// still enforces the required values (MODBUS_IP, MQTT_SERVER, …).
func loadConfig(explicit string, logger *slog.Logger) (*config.Config, error) {
	env := config.OSEnv{}
	path := explicit
	if path == "" {
		if located, ok := config.Locate(env); ok {
			path = located
		}
	}
	if path == "" {
		cfg, err := config.Load(strings.NewReader(""), env)
		if err != nil {
			return nil, err
		}
		logger.Info("mtec2mqtt.config_loaded", slog.String("path", "(environment only)"))
		return cfg, nil
	}
	logger.Info("mtec2mqtt.config_loaded", slog.String("path", path))
	return config.LoadFile(path, env)
}

// loadCatalog finds registers.yaml. Search order:
//
//  1. --registers flag
//  2. directory next to the binary (os.Executable)
//  3. current working directory
//
// The catalog lives outside the binary so an operator can patch
// register definitions without recompiling — matches the explicit
// design choice not to embed YAML assets.
func loadCatalog(explicit string, logger *slog.Logger) (*registers.Map, error) {
	path := explicit
	if path == "" {
		path = locateRegisters()
	}
	if path == "" {
		return nil, fmt.Errorf("mtec2mqtt: no %s found (place next to the binary or pass --registers)",
			registersFilename)
	}
	m, diag, err := registers.Load(path)
	if err != nil {
		return nil, err
	}
	for _, d := range diag {
		logger.Warn("mtec2mqtt.catalog_diag", slog.String("note", d))
	}
	logger.Info("mtec2mqtt.catalog_loaded",
		slog.String("path", path),
		slog.Int("registers", len(m.All)))
	return m, nil
}

func locateRegisters() string {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), registersFilename))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, registersFilename))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// errTransportNotWired is returned by a [deferredTransport] used before
// its client was supplied. A programming error, reported rather than
// panicked because the caller is a publish path and the daemon losing one
// config message is better than the daemon dying.
var errTransportNotWired = errors.New("mtec2mqtt: transport used before the client was wired")

// deferredTransport is a [publisher.Transport] whose client is supplied
// after construction.
//
// It exists for one ordering constraint, and it is a real one: the Last
// Will is part of CONNECT, so the MQTT client must be built with it —
// while the will itself is [publisher.Runtime.Will]'s answer, which is
// what makes the will's topic and the availability topic all 100 entities
// reference provably one string. One of the two has to be built first, and
// making it the runtime is what keeps the will a single statement instead
// of a literal here that has to agree with a literal in the library.
//
// wire is called before the lifecycle connects, so nothing can reach a
// method here beforehand. The field is guarded anyway: once connected it
// is read from the transport's read loop (the sweep's snapshot window) and
// from the poll path at the same time.
type deferredTransport struct {
	mu sync.RWMutex
	tr publisher.Transport
}

// wire supplies the transport. Calling it twice is a programming error and
// the last call wins; nothing in this daemon does.
func (d *deferredTransport) wire(tr publisher.Transport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tr = tr
}

func (d *deferredTransport) target() (publisher.Transport, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.tr == nil {
		return nil, errTransportNotWired
	}
	return d.tr, nil
}

// Publish implements [publisher.Transport].
func (d *deferredTransport) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	tr, err := d.target()
	if err != nil {
		return err
	}
	return tr.Publish(ctx, topic, payload, qos, retain)
}

// Subscribe implements [publisher.Transport].
func (d *deferredTransport) Subscribe(ctx context.Context, filter string, qos byte, h publisher.Handler) error {
	tr, err := d.target()
	if err != nil {
		return err
	}
	return tr.Subscribe(ctx, filter, qos, h)
}

// Unsubscribe implements [publisher.Transport].
func (d *deferredTransport) Unsubscribe(ctx context.Context, filter string) error {
	tr, err := d.target()
	if err != nil {
		return err
	}
	return tr.Unsubscribe(ctx, filter)
}
