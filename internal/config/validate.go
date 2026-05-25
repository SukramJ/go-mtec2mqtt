// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ValidationError is returned by [Validate] when the loaded config
// fails one or more range or shape checks. The Issues slice contains
// human-readable problem descriptions in declaration order so the
// caller can log them all in one shot — matches the Pydantic
// "aggregate" behaviour the Python schema relies on.
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

// allowedFramers mirrors the whitelist from ConfigSchema in Python.
var allowedFramers = map[string]bool{
	"rtu":    true,
	"socket": true,
	"ascii":  true,
	"tls":    true,
}

// floatFormatPattern accepts the Python format specs the daemon
// actually uses for MQTT_FLOAT_FORMAT: optional width, optional
// precision, one of the float verbs (e/f/g/E/F/G). Anything fancier
// (thousands separators, sign flags, padding chars) is rejected with a
// clear error rather than silently ignored.
var floatFormatPattern = regexp.MustCompile(`^(?:[0-9]+)?(?:\.[0-9]+)?[efgEFG]$`)

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
	// Slave-id 0 is "broadcast" — Modbus spec allows it but writes to
	// holding registers in broadcast mode are silent, which is never
	// what an end user wants. Allow it (Python schema does) but cap at
	// 247 per the standard slave-id range.
	if c.ModbusSlave > 247 {
		add("MODBUS_SLAVE must be 0..247, got %d", c.ModbusSlave)
	}
	if c.ModbusTimeout < 1 || c.ModbusTimeout > 600 {
		add("MODBUS_TIMEOUT must be 1..600 seconds, got %d", c.ModbusTimeout)
	}
	if !allowedFramers[c.ModbusFramer] {
		add("MODBUS_FRAMER must be one of [ascii rtu socket tls], got %q", c.ModbusFramer)
	}
	if c.ModbusRetries < 0 || c.ModbusRetries > 20 {
		add("MODBUS_RETRIES must be 0..20, got %d", c.ModbusRetries)
	}

	// --- MQTT ---
	if c.MQTTServer == "" {
		add("MQTT_SERVER is required")
	}
	if c.MQTTPort < 1 || c.MQTTPort > 65535 {
		add("MQTT_PORT must be 1..65535, got %d", c.MQTTPort)
	}
	if c.MQTTTopic == "" {
		add("MQTT_TOPIC is required")
	}
	verb, err := translateFloatFormat(c.MQTTFloatFormat)
	if err != nil {
		add("MQTT_FLOAT_FORMAT %q: %v", c.MQTTFloatFormat, err)
	} else {
		c.goFloatVerb = verb
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

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// translateFloatFormat turns a Python-style float format spec into a
// Go fmt verb. Accepts the three shapes the Python project actually
// produces — `"{:.3f}"`, `":.3f"`, `".3f"` — plus the underlying
// `[width][.prec]<e|f|g>` body.
//
// Returns an error for anything the regex does not match; better to
// fail loudly at startup than to silently render values with a
// degenerate format at runtime.
func translateFloatFormat(spec string) (string, error) {
	if spec == "" {
		return "", errors.New("empty format spec")
	}
	body := strings.TrimSuffix(strings.TrimPrefix(spec, "{"), "}")
	body = strings.TrimPrefix(body, ":")
	if !floatFormatPattern.MatchString(body) {
		return "", fmt.Errorf("unsupported format %q (allowed: [width][.prec]<e|f|g>)", body)
	}
	return "%" + body, nil
}

// FormatFloat renders v through the validated MQTT_FLOAT_FORMAT spec.
// Panics if called before Validate has succeeded — call sites should
// only ever see a fully validated Config.
func (c *Config) FormatFloat(v float64) string {
	if c.goFloatVerb == "" {
		panic("config: FormatFloat called before Validate")
	}
	return fmt.Sprintf(c.goFloatVerb, v)
}

// GoFloatVerb exposes the translated fmt verb for diagnostics and
// tests. Do not use it in hot paths — call [Config.FormatFloat].
func (c *Config) GoFloatVerb() string { return c.goFloatVerb }
