// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
	"github.com/SukramJ/go-mtec2mqtt/internal/version"
)

// This file implements the methods the optional web UI consumes. The set
// is kept here (rather than scattered through coordinator.go) so the
// pure data-flow core stays focused. Together these satisfy the
// `Backend` interface defined in internal/web — wired structurally, so
// the coordinator never imports the web package.

// Snapshot returns the current live register values for the dashboard.
// Empty when caching is disabled (no Store wired in).
func (c *Coordinator) Snapshot() state.Snapshot {
	if c.deps.Store == nil {
		return state.Snapshot{}
	}
	return c.deps.Store.Snapshot()
}

// Health assembles the operational status surface from live connection
// state, config and the cached group freshness.
func (c *Coordinator) Health() state.Health {
	now := c.deps.Now()
	cfg := c.deps.Cfg

	var serial, firmware, equip string
	var groups map[string]state.GroupHealth
	if c.deps.Store != nil {
		serial, firmware, equip = c.deps.Store.Static()
		groups = c.deps.Store.GroupHealth(now)
	}

	connected := c.deps.Modbus.IsConnected()
	status := "ok"
	switch {
	case serial == "":
		status = "starting"
	case !connected:
		status = "degraded"
	}

	return state.Health{
		Status:          status,
		Version:         version.Version,
		ModbusConnected: connected,
		ModbusAddr:      fmt.Sprintf("%s:%d", cfg.ModbusIP, cfg.ModbusPort),
		MQTTServer:      fmt.Sprintf("%s:%d", cfg.MQTTServer, cfg.MQTTPort),
		MQTTTopic:       cfg.MQTTTopic,
		HASSEnabled:     cfg.HASSEnable,
		Serial:          serial,
		Firmware:        firmware,
		Equipment:       equip,
		Initialised:     serial != "",
		StartedAt:       c.startedAt,
		UptimeSeconds:   int64(now.Sub(c.startedAt).Seconds()),
		Groups:          groups,
	}
}

// Config returns the sanitised, read-only config projection (no secrets).
func (c *Coordinator) Config() state.ConfigView {
	cfg := c.deps.Cfg
	return state.ConfigView{
		ModbusIP:      cfg.ModbusIP,
		ModbusPort:    cfg.ModbusPort,
		ModbusSlave:   int(cfg.ModbusSlave),
		ModbusTimeout: cfg.ModbusTimeout,
		MQTTServer:    cfg.MQTTServer,
		MQTTPort:      cfg.MQTTPort,
		MQTTTopic:     cfg.MQTTTopic,
		HASSEnable:    cfg.HASSEnable,
		HASSBaseTopic: cfg.HASSBaseTopic,
		RefreshNow:    cfg.RefreshNow,
		RefreshConfig: cfg.RefreshConfig,
		RefreshDay:    cfg.RefreshDay,
		RefreshStatic: cfg.RefreshStatic,
		RefreshTotal:  cfg.RefreshTotal,
		Debug:         cfg.Debug,
		Language:      cfg.Language,
	}
}

// Registers returns catalog metadata so the SPA can label values and
// render edit controls for writable registers. Names and enum labels are
// localised to the configured language; the synthetic charge/discharge
// "active" switches are appended so the UI can render them too. Order
// matches the YAML, virtual switches last.
func (c *Coordinator) Registers() []state.RegisterInfo {
	lang := c.deps.Cfg.Language
	out := make([]state.RegisterInfo, 0, len(c.deps.Catalog.All)+len(c.deps.Virtual))
	for _, r := range c.deps.Catalog.All {
		out = append(out, state.RegisterInfo{
			Key:        r.Key,
			Name:       r.LocalizedName(lang),
			MQTT:       r.MQTT,
			Group:      string(r.Group),
			Unit:       r.Unit,
			Type:       string(r.Type),
			Writable:   r.Writable,
			Component:  r.HassComponentType,
			ValueItems: r.LocalizedValueItems(lang),
		})
	}
	for _, v := range c.deps.Virtual {
		out = append(out, state.RegisterInfo{
			Key:       v.Key,
			Name:      v.LocalizedName(lang),
			MQTT:      v.Key,
			Group:     v.Group,
			Writable:  true,
			Component: string(hass.PlatformSwitch),
		})
	}
	return out
}

// Write sets a writable register identified by its MQTT suffix. It runs
// synchronously — the Modbus client serialises wire transactions, so
// this safely interleaves with the poll goroutines — and surfaces the
// transport's typed errors (ErrUnknownRegister / ErrNotWritable /
// ErrValueParse) to the caller for HTTP status mapping. Synthetic
// "active" switch keys are routed through the same toggle logic the HA
// command path uses.
func (c *Coordinator) Write(ctx context.Context, mqttKey, value string) error {
	return c.dispatchWrite(ctx, mqttKey, value)
}

// Changes returns a coalesced change-notification channel for SSE, plus
// a cancel func. Returns a never-firing channel when caching is disabled.
func (c *Coordinator) Changes() (events <-chan struct{}, cancel func()) {
	if c.deps.Store == nil {
		return make(chan struct{}), func() {}
	}
	return c.deps.Store.Subscribe()
}
