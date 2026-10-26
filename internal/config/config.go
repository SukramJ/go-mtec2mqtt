// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package config holds the daemon's runtime settings: one flat YAML
// document whose keys are the field tags below.
//
// Values flow: YAML file → env overrides (MTEC_* prefix) → defaults →
// validation. The result is a single typed [Config] the rest of the
// daemon reads from.
package config

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/SukramJ/go-mqtt"
)

// Daemon-wide constants: the env-var prefix and the config file lookup.
//
// LegacyAppDirName is the directory releases up to 2.x (and the Python
// aiomtec2mqtt) used; [Locate] still searches it after [AppDirName], so an
// existing installation keeps its config. It goes away in a later major
// release.
const (
	EnvPrefix        = "MTEC_"
	AppDirName       = "mtec2mqtt"
	LegacyAppDirName = "aiomtec2mqtt"
	ConfigFile       = "config.yaml"
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

	// --- MQTT ---
	MQTTServer   string `yaml:"MQTT_SERVER"`
	MQTTPort     int    `yaml:"MQTT_PORT"`
	MQTTLogin    string `yaml:"MQTT_LOGIN"`
	MQTTPassword string `yaml:"MQTT_PASSWORD"`
	// MQTTTopic is the mqtt-smarthome instance name, the first level of
	// every topic this daemon publishes (`<name>/status/…`,
	// `<name>/connected`, …). Defaults to [DefaultMQTTTopic]; an
	// explicitly configured value is kept verbatim. It is the ONLY thing
	// that keeps two instances on one broker apart, and nothing checks it.
	MQTTTopic string `yaml:"MQTT_TOPIC"`
	// MQTTMaintenance enables the mqtt-smarthome maintenance topics
	// (`<name>/maintenance/set/loglevel`, `…/set/restart`,
	// `<name>/maintenance/stats`). On by default; anyone who may publish
	// on the broker can then raise the log level or restart the daemon,
	// so an unsecured broker should run with it off.
	MQTTMaintenance bool `yaml:"MQTT_MAINTENANCE"`
	// MQTTStatsInterval is the period of `<name>/maintenance/stats` in
	// seconds. Default 60; 0 switches the topic off.
	MQTTStatsInterval int `yaml:"MQTT_STATS_INTERVAL"`
	// MQTTTLS enables TLS for the broker connection: the daemon dials
	// tls:// instead of tcp:// (default port 8883 instead of 1883, see
	// [Config.MQTTBrokerURL]). Off by default so existing plain-TCP
	// deployments keep working unchanged — credentials otherwise cross
	// the wire in clear text. Releases up to 3.2.0 called it MQTT_SSL;
	// see [renamedKeys].
	MQTTTLS bool `yaml:"MQTT_TLS"`
	// MQTTTLSInsecure, when true AND MQTTTLS is true, disables broker
	// certificate verification (TLS InsecureSkipVerify). This is an
	// explicit, dangerous opt-in meant only for self-signed certificates
	// on a broker the operator controls — it must never be the default,
	// since it removes protection against a man-in-the-middle. Prefer
	// [Config.MQTTTLSCA]; the two are mutually exclusive.
	MQTTTLSInsecure bool `yaml:"MQTT_TLS_INSECURE"`
	// MQTTTLSCA is the path of a PEM CA bundle the broker certificate is
	// verified against, replacing the system roots — for a broker whose
	// certificate a private CA issued. Empty (default) verifies against
	// the system roots. Only takes effect when MQTTTLS is true.
	MQTTTLSCA string `yaml:"MQTT_TLS_CA"`

	// --- Home Assistant ---
	HASSEnable         bool   `yaml:"HASS_ENABLE"`
	HASSBaseTopic      string `yaml:"HASS_BASE_TOPIC"`
	HASSBirthGracetime int    `yaml:"HASS_BIRTH_GRACETIME"` // seconds
	// DeviceName is an optional operator-chosen name for this inverter.
	// When set it becomes the Home Assistant device name (replacing the
	// generic "MTEC EnergyButler") and is slugged into each entity's
	// entity_id seed, so HA seeds fresh entity_ids like
	// sensor.<device_name>_grid_power instead of the generic ones. The seed
	// is the slugified English register name, never the localised display
	// name, so
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

	// --- evcc ---
	// EVCCEnable publishes the shared evcc profile (`<name>/status/<serial>/
	// evcc/<role>/<key>`: grid, PV and battery meters with evcc's signs) and
	// routes its battery mode command. Off by default; off publishes
	// nothing extra.
	EVCCEnable bool `yaml:"EVCC_ENABLE"`
	// EVCCBatteryModeTimeout returns the battery to normal when a hold or
	// charge mode has not been re-sent for this many seconds, so an evcc
	// that stopped never leaves the battery locked. 0 (default) is off.
	EVCCBatteryModeTimeout int `yaml:"EVCC_BATTERYMODE_TIMEOUT"`

	// --- Misc ---
	Debug bool `yaml:"DEBUG"`

	// removed lists the [removedKeys] the file or the environment set,
	// populated in Load. See [Config.Removed].
	removed []string `yaml:"-"`
}

// removedKeys were removed in 3.0.0. A configuration that still sets one
// loads — the key is ignored — and the daemon warns, so an upgrade does
// not stop on a key that never did anything: MODBUS_FRAMER and
// MODBUS_RETRIES were never read (the bridge speaks Modbus TCP and does
// not retry a transaction), and MQTT_FLOAT_FORMAT is replaced by the
// per-register `precision` in registers.yaml.
var removedKeys = []string{"MODBUS_FRAMER", "MODBUS_RETRIES", "MQTT_FLOAT_FORMAT"}

// Removed returns the removed keys this configuration still sets.
func (c *Config) Removed() []string { return c.removed }

// renamedKeys were renamed after 3.2.0, old key → new key. Unlike
// [removedKeys] a configuration that still sets one is REFUSED: the old
// key would otherwise be silently ignored, and an ignored MQTT_SSL: true
// falls back to plain TCP — credentials in clear text — without anyone
// noticing. Order is the order the errors are reported in.
var renamedKeys = [][2]string{
	{"MQTT_SSL", "MQTT_TLS"},
	{"MQTT_SSL_INSECURE", "MQTT_TLS_INSECURE"},
}

// IgnoredTLSKeys returns the TLS-only keys this configuration sets while
// MQTT_TLS is off, where they have no effect. The daemon warns about
// each; it does not refuse them, because a plain-TCP configuration with
// a prepared CA path is a legitimate intermediate state.
func (c *Config) IgnoredTLSKeys() []string {
	if c.MQTTTLS {
		return nil
	}
	var keys []string
	if c.MQTTTLSInsecure {
		keys = append(keys, "MQTT_TLS_INSECURE")
	}
	if c.MQTTTLSCA != "" {
		keys = append(keys, "MQTT_TLS_CA")
	}
	return keys
}

// MQTTTLSConfig builds the tls.Config for the broker connection, or
// returns nil when MQTT_TLS is off. The server name is MQTT_SERVER, the
// bare broker host (it is also the host part of [Config.MQTTBrokerURL]).
// An unusable MQTT_TLS_CA — missing, unreadable, no certificate in it —
// is an error naming the file, so the daemon stops at start-up instead
// of failing every handshake later.
func (c *Config) MQTTTLSConfig() (*tls.Config, error) {
	if !c.MQTTTLS {
		return nil, nil //nolint:nilnil // nil config = plain TCP, not an error
	}
	tlsConfig, err := mqtt.NewClientTLSConfigWithCA(c.MQTTServer, c.MQTTTLSCA, c.MQTTTLSInsecure)
	if err != nil {
		return nil, fmt.Errorf("config: MQTT_TLS_CA: %w", err)
	}
	return tlsConfig, nil
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
// dials from MQTTServer / MQTTPort / MQTTTLS. The scheme is "tls" when
// MQTTTLS is set, "tcp" otherwise. The port is only appended when it
// differs from the scheme's default (1883 plain / 8883 TLS); this keeps
// the common case's URL minimal and matches how [Config.MQTTPort] is
// documented in config-template.yaml.
func (c *Config) MQTTBrokerURL() string {
	scheme := "tcp"
	defaultPort := mqttDefaultPlainPort
	if c.MQTTTLS {
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

// EVCCBatteryModeTimeoutDuration returns EVCCBatteryModeTimeout as a
// time.Duration; 0 means off.
func (c *Config) EVCCBatteryModeTimeoutDuration() time.Duration {
	return time.Duration(c.EVCCBatteryModeTimeout) * time.Second
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
