// SPDX-License-Identifier: MIT
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

	// --- Home Assistant ---
	HASSEnable         bool   `yaml:"HASS_ENABLE"`
	HASSBaseTopic      string `yaml:"HASS_BASE_TOPIC"`
	HASSBirthGracetime int    `yaml:"HASS_BIRTH_GRACETIME"` // seconds

	// --- Refresh intervals (seconds) ---
	RefreshNow    int `yaml:"REFRESH_NOW"`
	RefreshConfig int `yaml:"REFRESH_CONFIG"`
	RefreshDay    int `yaml:"REFRESH_DAY"`
	RefreshStatic int `yaml:"REFRESH_STATIC"`
	RefreshTotal  int `yaml:"REFRESH_TOTAL"`

	// --- Misc ---
	Debug bool `yaml:"DEBUG"`

	// goFloatVerb caches the translated [MQTTFloatFormat], populated
	// in Validate. Never set by callers; ignored by yaml.v3.
	goFloatVerb string `yaml:"-"`
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
