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
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/coordinator"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/mqtt"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
	"github.com/SukramJ/go-mtec2mqtt/internal/version"
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
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	logger.Info("mtec2mqtt.boot", slog.String("build", version.String()))

	if err := run(*configPath, *registersPath, logger); err != nil {
		logger.Error("mtec2mqtt.fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
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
	mqttClient := mqtt.NewTCPClient(mqtt.TCPConfig{
		BrokerURL:    fmt.Sprintf("tcp://%s:%d", cfg.MQTTServer, cfg.MQTTPort),
		ClientID:     clientID,
		Username:     cfg.MQTTLogin,
		Password:     cfg.MQTTPassword,
		KeepAlive:    60 * time.Second,
		WillTopic:    cfg.HASSBaseTopic + "/status/lwt",
		WillPayload:  []byte("offline"),
		WillRetain:   true,
		CleanSession: true,
		Logger:       logger,
	})
	mqttLifecycle := mqtt.NewLifecycle(mqtt.DefaultLifecycle(), mqttClient)
	if err := mqttLifecycle.Start(ctx); err != nil {
		return fmt.Errorf("mtec2mqtt: mqtt start: %w", err)
	}
	defer func() {
		// Graceful disconnect — bounded so a hung broker can't block
		// shutdown for more than a few seconds.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		_ = mqttLifecycle.Stop(stopCtx)
	}()

	// --- hass discovery (optional) ---
	var discovery *hass.Discovery
	if cfg.HASSEnable {
		discovery = hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, catalog)
	}

	// --- coordinator ---
	c := coordinator.New(coordinator.Deps{
		Cfg:     cfg,
		Catalog: catalog,
		Modbus:  modbusClient,
		Reader:  reader,
		MQTT:    mqttClient,
		HASS:    discovery,
		Logger:  logger,
	})

	return c.Run(ctx)
}

// loadConfig finds and parses the daemon's YAML config. An explicit
// --config flag overrides the standard search order so the daemon can
// run from anywhere without relying on env vars.
func loadConfig(explicit string, logger *slog.Logger) (*config.Config, error) {
	path := explicit
	if path == "" {
		var ok bool
		path, ok = config.Locate(config.OSEnv{})
		if !ok {
			return nil, fmt.Errorf("mtec2mqtt: no config.yaml found (set $XDG_CONFIG_HOME/aiomtec2mqtt/config.yaml or pass --config)")
		}
	}
	logger.Info("mtec2mqtt.config_loaded", slog.String("path", path))
	return config.LoadFile(path, config.OSEnv{})
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
