// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEnv is a deterministic [Env] for tests.
type fakeEnv struct{ vars map[string]string }

func (f fakeEnv) LookupEnv(k string) (string, bool) {
	v, ok := f.vars[k]
	return v, ok
}

func (f fakeEnv) Environ() []string {
	out := make([]string, 0, len(f.vars))
	for k, v := range f.vars {
		out = append(out, k+"="+v)
	}
	return out
}

// minimumYAML is the smallest valid config — every non-defaulted field
// gets a real value. Reused across happy-path tests.
const minimumYAML = `
MODBUS_IP: 192.168.0.10
MODBUS_PORT: 502
MODBUS_SLAVE: 247
MODBUS_TIMEOUT: 5
MQTT_SERVER: localhost
MQTT_PORT: 1883
MQTT_TOPIC: MTEC
`

func TestLoadHappyPathAppliesDefaults(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModbusFramer != DefaultModbusFramer {
		t.Errorf("framer default not applied: %q", c.ModbusFramer)
	}
	if c.RefreshNow != DefaultRefreshNow {
		t.Errorf("REFRESH_NOW default not applied: %d", c.RefreshNow)
	}
	if c.HASSBaseTopic != DefaultHASSBaseTopic {
		t.Errorf("HASS_BASE_TOPIC default not applied: %q", c.HASSBaseTopic)
	}
	if c.GoFloatVerb() != "%.3f" {
		t.Errorf("MQTT_FLOAT_FORMAT default not translated: %q", c.GoFloatVerb())
	}
}

func TestLoadAggregatesValidationErrors(t *testing.T) {
	bad := `
MODBUS_PORT: 99999
MODBUS_TIMEOUT: 0
MQTT_PORT: 0
MODBUS_FRAMER: nope
`
	_, err := Load(strings.NewReader(bad), nil)
	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	// All four issues plus MODBUS_IP / MQTT_SERVER / MQTT_TOPIC required.
	if len(v.Issues) < 4 {
		t.Fatalf("expected multiple aggregated issues, got %d: %v", len(v.Issues), v.Issues)
	}
}

func TestEnvOverrideCoercion(t *testing.T) {
	env := fakeEnv{vars: map[string]string{
		"MTEC_MODBUS_IP":     "10.0.0.5",
		"MTEC_MODBUS_PORT":   "5743",
		"MTEC_HASS_ENABLE":   "true",
		"MTEC_REFRESH_NOW":   "20",
		"MTEC_MQTT_PASSWORD": "s3cret!", // stays string
		"UNRELATED_VAR":      "ignored",
		"MTEC_":              "empty key, ignored",
	}}
	c, err := Load(strings.NewReader(minimumYAML), env)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModbusIP != "10.0.0.5" {
		t.Errorf("MTEC_MODBUS_IP override: %q", c.ModbusIP)
	}
	if c.ModbusPort != 5743 {
		t.Errorf("int coercion failed: %d", c.ModbusPort)
	}
	if !c.HASSEnable {
		t.Errorf("bool coercion failed")
	}
	if c.RefreshNow != 20 {
		t.Errorf("REFRESH_NOW override: %d", c.RefreshNow)
	}
	if c.MQTTPassword != "s3cret!" {
		t.Errorf("string preserved: %q", c.MQTTPassword)
	}
}

func TestFloatFormatTranslation(t *testing.T) {
	cases := map[string]string{
		"{:.3f}": "%.3f",
		":.3f":   "%.3f",
		".3f":    "%.3f",
		".5g":    "%.5g",
		"8.2f":   "%8.2f",
		"5e":     "%5e",
	}
	for in, want := range cases {
		got, err := translateFloatFormat(in)
		if err != nil || got != want {
			t.Errorf("translate %q: got %q / %v, want %q", in, got, err, want)
		}
	}

	rejected := []string{"", "x", ".3", "abc", ",.2f", "{0:.3f}"}
	for _, in := range rejected {
		if _, err := translateFloatFormat(in); err == nil {
			t.Errorf("translate %q: expected error, got nil", in)
		}
	}
}

func TestFormatFloatUsesTranslatedVerb(t *testing.T) {
	yaml := minimumYAML + "MQTT_FLOAT_FORMAT: \"{:.2f}\"\n"
	c, err := Load(strings.NewReader(yaml), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.FormatFloat(1.2345); got != "1.23" {
		t.Errorf("FormatFloat: got %q, want 1.23", got)
	}
}

func TestLocateFindsCWDFirst(t *testing.T) {
	dir := t.TempDir()
	// Drop a config.yaml into the temp CWD, then chdir there.
	target := filepath.Join(dir, ConfigFile)
	if err := os.WriteFile(target, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	env := fakeEnv{} // no XDG/APPDATA set
	got, ok := Locate(env)
	if !ok {
		t.Fatal("expected Locate hit")
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

func TestLocateFallsBackToXDGOrHome(t *testing.T) {
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	target := filepath.Join(xdg, AppDirName, ConfigFile)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CWD does NOT contain a config.yaml — Locate must keep walking.
	t.Chdir(dir)

	env := fakeEnv{vars: map[string]string{"XDG_CONFIG_HOME": xdg}}
	got, ok := Locate(env)
	if !ok {
		t.Fatal("expected Locate hit on XDG fallback")
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

func TestLocateMissReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Point HOME at the empty temp dir too — otherwise the
	// ~/.config/aiomtec2mqtt/config.yaml branch may hit a real
	// install on the developer's machine.
	t.Setenv("HOME", dir)
	env := fakeEnv{vars: map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(dir, "nope"),
	}}
	if _, ok := Locate(env); ok {
		t.Fatal("expected miss, got hit")
	}
}

// Smoke-test against the shipped config-template.yaml — it must load
// out of the box (every required field is set, MODBUS_IP is the
// 0.0.0.0 placeholder that passes the non-empty check).
func TestLoadTemplateValidates(t *testing.T) {
	f, err := os.Open("../../config-template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := Load(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModbusPort != 502 || c.MQTTPort != 1883 || c.MQTTTopic != "MTEC" {
		t.Errorf("template values drifted: modbus=%d mqtt=%d topic=%q",
			c.ModbusPort, c.MQTTPort, c.MQTTTopic)
	}
}

func TestFormatFloatPanicsBeforeValidate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when goFloatVerb is empty")
		}
	}()
	var c Config
	_ = c.FormatFloat(1.0)
}
