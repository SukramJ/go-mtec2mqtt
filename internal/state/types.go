// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package state holds the live, in-memory view of the inverter that the
// optional web UI renders. The coordinator writes the latest processed
// register values here on every poll cycle; the web layer reads
// snapshots out for its REST and SSE endpoints.
//
// The package is a leaf: it imports only the standard library so both
// the coordinator and the web server can depend on it without creating
// an import cycle. The DTO types double as the JSON wire shapes the SPA
// consumes — keep their `json` tags stable.
package state

import "time"

// Snapshot is the live value view: per-group decoded register values
// plus the inverter identity learned from the STATIC group. Values are
// the processed (human-readable) forms the coordinator also publishes
// over MQTT, kept as native Go types (numbers, strings, bools) so the
// SPA can format them itself.
type Snapshot struct {
	Serial    string               `json:"serial"`
	Firmware  string               `json:"firmware"`
	Equipment string               `json:"equipment"`
	Groups    map[string]GroupView `json:"groups"`
}

// GroupView is one polling group's most recent values.
type GroupView struct {
	UpdatedAt time.Time      `json:"updated_at"`
	Values    map[string]any `json:"values"`
}

// Health is the operational status surface for the dashboard's header
// and health card. Assembled by the coordinator (it owns the live
// connection state and config) rather than stored here.
type Health struct {
	Status          string                 `json:"status"` // "ok" | "degraded" | "starting"
	Version         string                 `json:"version"`
	ModbusConnected bool                   `json:"modbus_connected"`
	ModbusAddr      string                 `json:"modbus_addr"`
	MQTTServer      string                 `json:"mqtt_server"`
	MQTTTopic       string                 `json:"mqtt_topic"`
	HASSEnabled     bool                   `json:"hass_enabled"`
	Serial          string                 `json:"serial"`
	Firmware        string                 `json:"firmware"`
	Equipment       string                 `json:"equipment"`
	Initialised     bool                   `json:"initialised"` // first STATIC read done
	StartedAt       time.Time              `json:"started_at"`
	UptimeSeconds   int64                  `json:"uptime_seconds"`
	Groups          map[string]GroupHealth `json:"groups"`
}

// GroupHealth summarises a polling group's freshness for the health view.
type GroupHealth struct {
	UpdatedAt  time.Time `json:"updated_at"`
	AgeSeconds int64     `json:"age_seconds"`
	Count      int       `json:"count"`
}

// RegisterInfo is the catalog metadata the SPA needs to label values and
// render edit controls for writable registers. Built from the register
// catalog by the coordinator.
type RegisterInfo struct {
	Key        string         `json:"key"`
	Name       string         `json:"name"`
	MQTT       string         `json:"mqtt"`
	Group      string         `json:"group"`
	Unit       string         `json:"unit"`
	Type       string         `json:"type"`
	Writable   bool           `json:"writable"`
	Component  string         `json:"component,omitempty"`   // hass_component_type (number/select/switch)
	ValueItems map[int]string `json:"value_items,omitempty"` // enum/select code → label
}

// ConfigView is the sanitised, read-only projection of the daemon config
// shown in the UI. Secrets (MQTT and web passwords) are deliberately
// omitted.
type ConfigView struct {
	ModbusIP      string `json:"modbus_ip"`
	ModbusPort    int    `json:"modbus_port"`
	ModbusSlave   int    `json:"modbus_slave"`
	ModbusTimeout int    `json:"modbus_timeout"`
	MQTTServer    string `json:"mqtt_server"`
	MQTTPort      int    `json:"mqtt_port"`
	MQTTTopic     string `json:"mqtt_topic"`
	HASSEnable    bool   `json:"hass_enable"`
	HASSBaseTopic string `json:"hass_base_topic"`
	RefreshNow    int    `json:"refresh_now"`
	RefreshConfig int    `json:"refresh_config"`
	RefreshDay    int    `json:"refresh_day"`
	RefreshStatic int    `json:"refresh_static"`
	RefreshTotal  int    `json:"refresh_total"`
	Debug         bool   `json:"debug"`
}
