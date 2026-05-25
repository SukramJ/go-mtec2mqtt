// SPDX-License-Identifier: MIT
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

	DefaultHASSEnable         = false
	DefaultHASSBaseTopic      = "homeassistant"
	DefaultHASSBirthGracetime = 15

	DefaultRefreshNow    = 10
	DefaultRefreshConfig = 30
	DefaultRefreshDay    = 300
	DefaultRefreshStatic = 3600
	DefaultRefreshTotal  = 300
)

// applyDefaults fills in any field whose YAML+env round left it at its
// zero value with the documented default. Fields without a default —
// the mandatory connection parameters — are left at zero and caught by
// [Validate].
func applyDefaults(c *Config) {
	if c.ModbusFramer == "" {
		c.ModbusFramer = DefaultModbusFramer
	}
	if c.ModbusRetries == 0 {
		// Note: zero retries is a legitimate value but matches the
		// schema default anyway, so distinguishing the two adds no
		// information.
		c.ModbusRetries = DefaultModbusRetries
	}
	if c.MQTTFloatFormat == "" {
		c.MQTTFloatFormat = DefaultMQTTFloatFormat
	}
	if c.HASSBaseTopic == "" {
		c.HASSBaseTopic = DefaultHASSBaseTopic
	}
	if c.HASSBirthGracetime == 0 {
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
}
