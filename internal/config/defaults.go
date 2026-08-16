// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package config

import "strings"

// Default values applied when the YAML omits a field. These mirror
// ConfigSchema in aiomtec2mqtt/config_schema.py — the daemon must keep
// the same defaults so existing config.yaml files behave the same way
// when loaded by the Go port.
const (
	DefaultModbusFramer  = "rtu"
	DefaultModbusRetries = 3

	DefaultMQTTLogin       = ""
	DefaultMQTTPassword    = ""
	DefaultMQTTFloatFormat = ".3f"

	// DefaultMQTTSSL / DefaultMQTTSSLInsecure keep existing plain-TCP
	// deployments unchanged on upgrade: TLS is opt-in, and even with TLS
	// enabled, certificate verification stays on unless explicitly
	// disabled. Both already equal the bool zero value, so applyDefaults
	// does not assign them — the constants exist to document the
	// MQTT_SSL / MQTT_SSL_INSECURE yaml keys' documented defaults.
	DefaultMQTTSSL         = false
	DefaultMQTTSSLInsecure = false

	DefaultHASSEnable         = false
	DefaultHASSBaseTopic      = "homeassistant"
	DefaultHASSBirthGracetime = 15

	// DefaultHassUniqueIDIncludeSerial keeps existing installations
	// unchanged on upgrade: folding the inverter serial into HA
	// unique_ids / discovery topics is opt-in only, since flipping it on
	// an existing install orphans the previously discovered entities
	// (see Config.HassUniqueIDIncludeSerial). Already equal to the bool
	// zero value, so applyDefaults does not assign it — the constant
	// exists to document the HASS_UNIQUE_ID_INCLUDE_SERIAL yaml key's
	// documented default.
	DefaultHassUniqueIDIncludeSerial = false

	DefaultRefreshNow    = 10
	DefaultRefreshConfig = 30
	DefaultRefreshDay    = 300
	DefaultRefreshStatic = 3600
	DefaultRefreshTotal  = 300

	// DefaultWebBind binds the optional UI to localhost only. Operators
	// who want LAN access set WEB_BIND: 0.0.0.0:8080 explicitly.
	DefaultWebBind = "127.0.0.1:8080"

	// DefaultLanguage is the fallback UI / HA display language.
	DefaultLanguage = "en"

	// DefaultChargeActiveValue / DefaultDischargeActiveValue is the
	// amperage the charge/discharge "active" switch writes when first
	// switched on with no cached value. Matches a typical M-TEC inverter.
	DefaultChargeActiveValue    = 50
	DefaultDischargeActiveValue = 50
)

// rawHasKey reports whether raw contains key, compared case-
// insensitively. yaml.v3 binds struct fields against their yaml tag
// case-insensitively (e.g. "modbus_retries: 0" decodes into
// Config.ModbusRetries exactly like "MODBUS_RETRIES: 0" does), but raw
// is a plain map[string]any decoded straight from the document, so its
// keys keep whatever case the YAML author (or an MTEC_* env var) used.
// A presence check that only tries the canonical upper-case tag would
// therefore miss a lower/mixed-case key and wrongly treat an explicit
// value as absent — silently overwriting it with the field's default.
func rawHasKey(raw map[string]any, key string) bool {
	for k := range raw {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// applyDefaults fills in any field whose YAML+env round left it at its
// zero value with the documented default. For fields where the zero
// value is itself a legal, documented setting — MODBUS_RETRIES,
// HASS_BIRTH_GRACETIME, REFRESH_NOW/CONFIG/DAY/STATIC/TOTAL,
// CHARGE_ACTIVE_VALUE, DISCHARGE_ACTIVE_VALUE (see [Validate]) — key
// presence in raw, not the zero value, decides whether the default
// applies: an explicit 0 must survive so Validate can reject (or
// accept) it on its own merits, rather than being silently replaced by
// the default. raw is the merged file+env key set; presence is checked
// case-insensitively via [rawHasKey] since yaml.v3 itself binds struct
// fields case-insensitively.
// Fields without a default — the mandatory connection parameters —
// are left at zero and caught by [Validate].
func applyDefaults(c *Config, raw map[string]any) {
	if c.ModbusFramer == "" {
		c.ModbusFramer = DefaultModbusFramer
	}
	if !rawHasKey(raw, "MODBUS_RETRIES") {
		// DefaultModbusRetries is 3, so an explicit MODBUS_RETRIES: 0
		// must survive — only default when the key is truly absent.
		c.ModbusRetries = DefaultModbusRetries
	}
	if c.MQTTFloatFormat == "" {
		c.MQTTFloatFormat = DefaultMQTTFloatFormat
	}
	if c.HASSBaseTopic == "" {
		c.HASSBaseTopic = DefaultHASSBaseTopic
	}
	if !rawHasKey(raw, "HASS_BIRTH_GRACETIME") {
		// An explicit HASS_BIRTH_GRACETIME: 0 disables the startup
		// grace wait — only default when the key is truly absent.
		c.HASSBirthGracetime = DefaultHASSBirthGracetime
	}
	if !rawHasKey(raw, "REFRESH_NOW") {
		c.RefreshNow = DefaultRefreshNow
	}
	if !rawHasKey(raw, "REFRESH_CONFIG") {
		c.RefreshConfig = DefaultRefreshConfig
	}
	if !rawHasKey(raw, "REFRESH_DAY") {
		c.RefreshDay = DefaultRefreshDay
	}
	if !rawHasKey(raw, "REFRESH_STATIC") {
		c.RefreshStatic = DefaultRefreshStatic
	}
	if !rawHasKey(raw, "REFRESH_TOTAL") {
		c.RefreshTotal = DefaultRefreshTotal
	}
	if c.WebBind == "" {
		c.WebBind = DefaultWebBind
	}
	if c.Language == "" {
		c.Language = DefaultLanguage
	}
	if !rawHasKey(raw, "CHARGE_ACTIVE_VALUE") {
		c.ChargeActiveValue = DefaultChargeActiveValue
	}
	if !rawHasKey(raw, "DISCHARGE_ACTIVE_VALUE") {
		c.DischargeActiveValue = DefaultDischargeActiveValue
	}
}
