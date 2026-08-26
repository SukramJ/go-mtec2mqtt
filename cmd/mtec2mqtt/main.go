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
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

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

	// --- mqtt ---
	clientID := clientIDBase + cfg.MQTTTopic
	// Retained availability topic: the broker-side will covers
	// ungraceful death, the OnConnect hook below publishes the matching
	// "online" birth, and the shutdown path re-publishes "offline"
	// because a graceful DISCONNECT suppresses the will.
	lwtTopic := cfg.HASSBaseTopic + "/status/lwt"
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
		Will: &mqtt.Will{
			Topic:   lwtTopic,
			Payload: []byte("offline"),
			Retain:  true,
		},
		TLSConfig: tlsConfig,
		Logger:    logger,
	})
	mqttLifecycle := mqtt.NewLifecycle(mqtt.DefaultLifecycle(), mqttClient)
	// The will only fires on ungraceful death; without a matching birth
	// publish a single network blip would leave the retained
	// availability topic stuck at "offline" for the rest of the
	// daemon's uptime. Registered before Start so the first connect
	// announces too.
	mqttLifecycle.OnConnect(announceAvailability(mqttClient, lwtTopic, "online", logger))
	if err := startMQTT(ctx, mqttLifecycle, time.Second, 30*time.Second, logger); err != nil {
		return fmt.Errorf("mtec2mqtt: mqtt start: %w", err)
	}
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
	defer func() {
		// Graceful disconnect — bounded so a hung broker can't block
		// shutdown for more than a few seconds. A clean DISCONNECT
		// suppresses the broker-side will, so leave the retained
		// availability topic at "offline" ourselves first.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		announceAvailability(mqttClient, lwtTopic, "offline", logger)(stopCtx)
		_ = mqttLifecycle.Stop(stopCtx)
	}()

	// --- hass discovery (optional) ---
	// The synthetic charge/discharge "active" switches are defined once
	// and shared: the discovery builder advertises them to HA while the
	// coordinator implements their toggle/restore write logic.
	virtualSwitches := hass.DefaultVirtualSwitches(cfg.ChargeActiveValue, cfg.DischargeActiveValue)
	var discovery *hass.Discovery
	if cfg.HASSEnable {
		discovery = hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, catalog, cfg.Language, virtualSwitches, cfg.DeviceName)
		discovery.IncludeSerialInUniqueIDs(cfg.HassUniqueIDIncludeSerial)
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
		MQTT:    &mqttSession{Breaker: breaker, MQTTSubscriber: mqttClient},
		HASS:    discovery,
		Logger:  logger,
		Store:   store,
		Virtual: virtualSwitches,
	})

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

// announceAvailability returns a callback that publishes the retained
// availability payload to the LWT topic. Registered as the lifecycle's
// OnConnect hook with "online" (fired on every (re)connect) and called
// directly with "offline" during graceful shutdown. Publish failures
// are logged, never fatal — availability is best-effort.
func announceAvailability(pub coordinator.MQTTPublisher, topic, payload string, logger *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		if err := pub.Publish(ctx, topic, []byte(payload), mqtt.QoS0, true); err != nil {
			logger.Warn("mtec2mqtt.lwt_publish_failed",
				slog.String("payload", payload),
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

// mqttSession is the MQTT surface handed to the coordinator: Publish
// is gated by the circuit breaker, while Subscribe/Unsubscribe go
// straight to the client — subscriptions are startup-path calls with
// their own SUBACK-bounded wait and must not be rejected during a
// publish-side broker brownout.
type mqttSession struct {
	*mqtt.Breaker
	coordinator.MQTTSubscriber
}

// Compile-time contract: the session satisfies the coordinator's
// combined MQTT dependency.
var _ interface {
	coordinator.MQTTPublisher
	coordinator.MQTTSubscriber
} = (*mqttSession)(nil)

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
