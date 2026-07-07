// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package config

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

// applyDefaults fills in any field whose YAML+env round left it at its
// zero value with the documented default. raw is the merged file+env
// key set: MODBUS_RETRIES and HASS_BIRTH_GRACETIME document 0 as a
// legal value (see [Validate]), so for those two fields key presence
// in raw — not the zero value — decides whether the default applies.
// Fields without a default — the mandatory connection parameters —
// are left at zero and caught by [Validate].
func applyDefaults(c *Config, raw map[string]any) {
	if c.ModbusFramer == "" {
		c.ModbusFramer = DefaultModbusFramer
	}
	if _, set := raw["MODBUS_RETRIES"]; !set {
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
	if _, set := raw["HASS_BIRTH_GRACETIME"]; !set {
		// An explicit HASS_BIRTH_GRACETIME: 0 disables the startup
		// grace wait — only default when the key is truly absent.
		c.HASSBirthGracetime = DefaultHASSBirthGracetime
	}
	if c.RefreshNow == 0 {
		c.RefreshNow = DefaultRefreshNow
	}
	if c.RefreshConfig == 0 {
		c.RefreshConfig = DefaultRefreshConfig
	}
	if c.RefreshDay == 0 {
		c.RefreshDay = DefaultRefreshDay
	}
	if c.RefreshStatic == 0 {
		c.RefreshStatic = DefaultRefreshStatic
	}
	if c.RefreshTotal == 0 {
		c.RefreshTotal = DefaultRefreshTotal
	}
	if c.WebBind == "" {
		c.WebBind = DefaultWebBind
	}
	if c.Language == "" {
		c.Language = DefaultLanguage
	}
	if c.ChargeActiveValue == 0 {
		c.ChargeActiveValue = DefaultChargeActiveValue
	}
	if c.DischargeActiveValue == 0 {
		c.DischargeActiveValue = DefaultDischargeActiveValue
	}
}
