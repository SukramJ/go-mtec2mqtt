// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

// Package config holds the daemon's runtime settings. The shape mirrors
// the YAML schema used by aiomtec2mqtt so existing config.yaml files
// can be reused unchanged.
//
// Values flow: YAML file → env overrides (MTEC_* prefix) → defaults →
// validation. The result is a single typed [Config] the rest of the
// daemon reads from.
package config

import (
	"fmt"
	"time"
)

// Daemon-wide constants. These match the Python project so MQTT topic
// paths, env-var prefix, and config-file lookup stay compatible.
const (
	ClientID      = "M-TEC-MQTT"
	MTECTopicRoot = "MTEC"
	EnvPrefix     = "MTEC_"
	AppDirName    = "aiomtec2mqtt"
	ConfigFile    = "config.yaml"
)

// Config is the validated daemon configuration. Fields are flat to
// match the YAML keys 1:1 — grouping into sub-structs would force a
// custom unmarshaller for what is otherwise a trivial yaml.v3 decode.
//
// Time-valued fields (Timeout / Refresh*) are time.Duration even
// though the YAML stores plain seconds; the helpers in load.go widen
// ints to durations during defaulting so callers never see raw ints.
type Config struct {
	// --- Modbus ---
	ModbusIP      string `yaml:"MODBUS_IP"`
	ModbusPort    int    `yaml:"MODBUS_PORT"`
	ModbusSlave   byte   `yaml:"MODBUS_SLAVE"`
	ModbusTimeout int    `yaml:"MODBUS_TIMEOUT"` // seconds
	ModbusFramer  string `yaml:"MODBUS_FRAMER"`
	ModbusRetries int    `yaml:"MODBUS_RETRIES"`

	// --- MQTT ---
	MQTTServer   string `yaml:"MQTT_SERVER"`
	MQTTPort     int    `yaml:"MQTT_PORT"`
	MQTTLogin    string `yaml:"MQTT_LOGIN"`
	MQTTPassword string `yaml:"MQTT_PASSWORD"`
	MQTTTopic    string `yaml:"MQTT_TOPIC"`
	// MQTTFloatFormat is the original Python-style format spec from
	// the YAML (e.g. "{:.3f}" or ".3f"). Consumers should call
	// [Config.FormatFloat] rather than interpret it directly.
	MQTTFloatFormat string `yaml:"MQTT_FLOAT_FORMAT"`
	// MQTTSSL enables TLS for the broker connection: the daemon dials
	// tls:// instead of tcp:// (default port 8883 instead of 1883, see
	// [Config.MQTTBrokerURL]). Off by default so existing plain-TCP
	// deployments keep working unchanged — credentials otherwise cross
	// the wire in clear text.
	MQTTSSL bool `yaml:"MQTT_SSL"`
	// MQTTSSLInsecure, when true AND MQTTSSL is true, disables broker
	// certificate verification (TLS InsecureSkipVerify). This is an
	// explicit, dangerous opt-in meant only for self-signed certificates
	// on a broker the operator controls — it must never be the default,
	// since it removes protection against a man-in-the-middle.
	MQTTSSLInsecure bool `yaml:"MQTT_SSL_INSECURE"`

	// --- Home Assistant ---
	HASSEnable         bool   `yaml:"HASS_ENABLE"`
	HASSBaseTopic      string `yaml:"HASS_BASE_TOPIC"`
	HASSBirthGracetime int    `yaml:"HASS_BIRTH_GRACETIME"` // seconds
	// DeviceName is an optional operator-chosen name for this inverter.
	// When set it becomes the Home Assistant device name (replacing the
	// generic "MTEC EnergyButler") and is slugged into each entity's
	// entity_id seed, so HA seeds fresh entity_ids like
	// sensor.<device_name>_grid_power instead of the generic ones. The seed
	// is the slugified English register name (matching the Python
	// aiomtec2mqtt entity_ids), never the localised display name, so
	// entity_ids stay language-independent; only the display name follows
	// LANGUAGE. The entity unique_id is deliberately left unchanged, so
	// enabling this on an existing install does not orphan established
	// entities or lose their history — only newly created entities pick up
	// the nicer id. Leave empty to keep the previous behaviour unchanged.
	// The MQTT topic tree also stays keyed on the inverter serial regardless.
	DeviceName string `yaml:"DEVICE_NAME"`
	// HassUniqueIDIncludeSerial is an opt-in: when true, the inverter's
	// serial number is folded into every Home Assistant entity's
	// unique_id and into the MQTT discovery topics, so multiple
	// inverters (multiple instances of this daemon) can share one Home
	// Assistant installation without their entities colliding. Off by
	// default — existing single-inverter installs keep behaving exactly
	// as before.
	//
	// WARNING: flipping this to true on an EXISTING installation
	// creates a brand-new set of unique_ids. The previously discovered
	// entities become orphaned: MQTT discovery has no migration path
	// for a unique_id change, so Home Assistant cannot rename the
	// existing entity — it only discovers the new unique_id as a fresh
	// entity, leaving history and any manual customisation (dashboards,
	// automations, entity renames) behind on the orphaned one. Only
	// enable this before the first HA discovery, or be prepared to
	// manually re-apply customisations to the newly created entities.
	HassUniqueIDIncludeSerial bool `yaml:"HASS_UNIQUE_ID_INCLUDE_SERIAL"`

	// --- Refresh intervals (seconds) ---
	RefreshNow    int `yaml:"REFRESH_NOW"`
	RefreshConfig int `yaml:"REFRESH_CONFIG"`
	RefreshDay    int `yaml:"REFRESH_DAY"`
	RefreshStatic int `yaml:"REFRESH_STATIC"`
	RefreshTotal  int `yaml:"REFRESH_TOTAL"`

	// --- Web UI (optional status/health dashboard) ---
	// WebEnable toggles the embedded HTTP server. Off by default so the
	// daemon stays a pure MQTT bridge unless an operator opts in.
	WebEnable bool `yaml:"WEB_ENABLE"`
	// WebBind is the listen address "host:port". Defaults to
	// 127.0.0.1:8080 — localhost-only — so enabling the UI never exposes
	// it to the network by accident.
	WebBind string `yaml:"WEB_BIND"`
	// WebUser / WebPassword enable HTTP Basic auth when both are set.
	// Leave both empty to serve without authentication (e.g. behind a
	// reverse proxy or on a trusted LAN).
	WebUser     string `yaml:"WEB_USER"`
	WebPassword string `yaml:"WEB_PASSWORD"`

	// --- Localisation ---
	// Language selects the UI / Home-Assistant display language. "en"
	// (default) or "de". It localises the web dashboard chrome and the
	// friendly names of HA entities; entity_ids stay language-independent.
	Language string `yaml:"LANGUAGE"`

	// --- Charge/discharge "active" switches ---
	// ChargeActiveValue / DischargeActiveValue are the amperage written to
	// the charge/discharge limit register when the corresponding "active"
	// switch is turned on for the first time (no previous value cached).
	// Plain numbers (the register's native unit), default 50.
	ChargeActiveValue    int `yaml:"CHARGE_ACTIVE_VALUE"`
	DischargeActiveValue int `yaml:"DISCHARGE_ACTIVE_VALUE"`

	// --- Misc ---
	Debug bool `yaml:"DEBUG"`

	// goFloatVerb caches the translated [MQTTFloatFormat], populated
	// in Validate. Never set by callers; ignored by yaml.v3.
	goFloatVerb string `yaml:"-"`
}

// mqttDefaultPlainPort / mqttDefaultTLSPort are the IANA-registered
// default ports for plain and TLS MQTT, mirrored by [Config.MQTTBrokerURL]
// so an explicit MQTT_PORT matching the scheme's default stays elidable
// from the broker URL (letting the transport's own scheme-based default
// stay authoritative).
const (
	mqttDefaultPlainPort = 1883
	mqttDefaultTLSPort   = 8883
)

// MQTTBrokerURL derives the broker URL the go-mqtt transport
// dials from MQTTServer / MQTTPort / MQTTSSL. The scheme is "tls" when
// MQTTSSL is set, "tcp" otherwise. The port is only appended when it
// differs from the scheme's default (1883 plain / 8883 TLS); this keeps
// the common case's URL minimal and matches how [Config.MQTTPort] is
// documented in config-template.yaml.
func (c *Config) MQTTBrokerURL() string {
	scheme := "tcp"
	defaultPort := mqttDefaultPlainPort
	if c.MQTTSSL {
		scheme = "tls"
		defaultPort = mqttDefaultTLSPort
	}
	if c.MQTTPort == defaultPort {
		return fmt.Sprintf("%s://%s", scheme, c.MQTTServer)
	}
	return fmt.Sprintf("%s://%s:%d", scheme, c.MQTTServer, c.MQTTPort)
}

// ModbusTimeoutDuration returns ModbusTimeout as a time.Duration.
func (c *Config) ModbusTimeoutDuration() time.Duration {
	return time.Duration(c.ModbusTimeout) * time.Second
}

// HASSBirthGracetimeDuration returns HASSBirthGracetime as a time.Duration.
func (c *Config) HASSBirthGracetimeDuration() time.Duration {
	return time.Duration(c.HASSBirthGracetime) * time.Second
}

// RefreshNowDuration etc. — semantic helpers so callers do not litter
// time.Second multiplications.
func (c *Config) RefreshNowDuration() time.Duration {
	return time.Duration(c.RefreshNow) * time.Second
}

// RefreshConfigDuration — see RefreshNowDuration.
func (c *Config) RefreshConfigDuration() time.Duration {
	return time.Duration(c.RefreshConfig) * time.Second
}

// RefreshDayDuration — see RefreshNowDuration.
func (c *Config) RefreshDayDuration() time.Duration {
	return time.Duration(c.RefreshDay) * time.Second
}

// RefreshStaticDuration — see RefreshNowDuration.
func (c *Config) RefreshStaticDuration() time.Duration {
	return time.Duration(c.RefreshStatic) * time.Second
}

// RefreshTotalDuration — see RefreshNowDuration.
func (c *Config) RefreshTotalDuration() time.Duration {
	return time.Duration(c.RefreshTotal) * time.Second
}
