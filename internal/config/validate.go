// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/SukramJ/go-hamqtt/topic"
)

// ValidationError is returned by [Validate] when the loaded config
// fails one or more range or shape checks. The Issues slice contains
// human-readable problem descriptions in declaration order so the
// caller can log them all in one shot.
type ValidationError struct {
	Issues []string
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	if len(e.Issues) == 1 {
		return "config: " + e.Issues[0]
	}
	return fmt.Sprintf("config: %d validation issue(s):\n  - %s",
		len(e.Issues), strings.Join(e.Issues, "\n  - "))
}

// allowedLanguages is the set of UI / HA display languages the daemon
// ships translations for. English is the canonical fallback.
var allowedLanguages = map[string]bool{
	"en": true,
	"de": true,
}

// Validate checks the post-defaults config and returns a
// [*ValidationError] aggregating every problem. On success, it also
// caches the Go-side fmt verb for [Config.FormatFloat].
func Validate(c *Config) error {
	var issues []string
	add := func(format string, args ...any) {
		issues = append(issues, fmt.Sprintf(format, args...))
	}

	// --- Modbus ---
	if c.ModbusIP == "" {
		add("MODBUS_IP is required")
	}
	if c.ModbusPort < 1 || c.ModbusPort > 65535 {
		add("MODBUS_PORT must be 1..65535, got %d", c.ModbusPort)
	}
	// MODBUS_SLAVE needs no check: the field is a byte, so decoding
	// already refuses anything outside 0..255, and every value in that
	// range is a legal Modbus-TCP unit id. The serial slave-id cap of
	// 247 does not apply over TCP, where 255 (0xFF) is the spec's "unit
	// id not significant" value (issue #68). 0 is "broadcast": allowed
	// though writes in broadcast mode are silent.
	if c.ModbusTimeout < 1 || c.ModbusTimeout > 600 {
		add("MODBUS_TIMEOUT must be 1..600 seconds, got %d", c.ModbusTimeout)
	}

	// --- MQTT ---
	if c.MQTTServer == "" {
		add("MQTT_SERVER is required")
	}
	if c.MQTTPort < 1 || c.MQTTPort > 65535 {
		add("MQTT_PORT must be 1..65535, got %d", c.MQTTPort)
	}
	// The instance name is the first level of every topic. A multi-level
	// value ("home/mtec") is kept verbatim — refusing it would break an
	// installation on upgrade — but runs outside mqtt-smarthome §3, and the
	// daemon says so at start. Wildcards, NUL, a leading `$` and empty
	// levels are refused: none of them can be published to.
	if _, err := topic.NewSmartHomeMultiLevel(c.MQTTTopic); err != nil {
		add("MQTT_TOPIC %q: %v", c.MQTTTopic, err)
	}
	if c.MQTTStatsInterval < 0 || c.MQTTStatsInterval > 86400 {
		add("MQTT_STATS_INTERVAL must be 0..86400 seconds, got %d", c.MQTTStatsInterval)
	}

	// --- HASS ---
	if c.HASSBirthGracetime < 0 || c.HASSBirthGracetime > 600 {
		add("HASS_BIRTH_GRACETIME must be 0..600 seconds, got %d", c.HASSBirthGracetime)
	}

	// --- Refresh ---
	rangeCheck := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			add("%s must be %d..%d seconds, got %d", name, lo, hi, v)
		}
	}
	rangeCheck("REFRESH_NOW", c.RefreshNow, 1, 3600)
	rangeCheck("REFRESH_CONFIG", c.RefreshConfig, 1, 3600)
	rangeCheck("REFRESH_DAY", c.RefreshDay, 1, 86400)
	rangeCheck("REFRESH_STATIC", c.RefreshStatic, 1, 86400)
	rangeCheck("REFRESH_TOTAL", c.RefreshTotal, 1, 86400)

	// --- Web UI ---
	// Only meaningful when the server is enabled; an unused bind address
	// shouldn't block startup of a pure-MQTT deployment.
	if c.WebEnable {
		host, port, err := net.SplitHostPort(c.WebBind)
		if err != nil {
			add("WEB_BIND must be host:port, got %q: %v", c.WebBind, err)
		} else {
			if host == "" {
				// An empty host means "all interfaces"; allow it but it's
				// worth being explicit, so nudge toward 0.0.0.0.
				host = "0.0.0.0"
			}
			if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
				add("WEB_BIND port must be 1..65535, got %q", port)
			}
			_ = host
		}
		// Basic auth is all-or-nothing: a username without a password
		// (or vice versa) is almost certainly a config mistake.
		if (c.WebUser == "") != (c.WebPassword == "") {
			add("WEB_USER and WEB_PASSWORD must both be set or both be empty")
		}
	}

	// --- Localisation ---
	if !allowedLanguages[c.Language] {
		add("LANGUAGE must be one of [de en], got %q", c.Language)
	}

	// --- Charge/discharge "active" switch values ---
	// The on-value is written through the same scaled uint16 path as a
	// manual limit edit, so cap it well inside that range. Zero would make
	// "on" indistinguishable from "off", so require at least 1.
	if c.ChargeActiveValue < 1 || c.ChargeActiveValue > 6000 {
		add("CHARGE_ACTIVE_VALUE must be 1..6000, got %d", c.ChargeActiveValue)
	}
	if c.DischargeActiveValue < 1 || c.DischargeActiveValue > 6000 {
		add("DISCHARGE_ACTIVE_VALUE must be 1..6000, got %d", c.DischargeActiveValue)
	}
	if c.EVCCBatteryModeTimeout < 0 || c.EVCCBatteryModeTimeout > 86400 {
		add("EVCC_BATTERYMODE_TIMEOUT must be 0..86400 (0 = off), got %d", c.EVCCBatteryModeTimeout)
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}
