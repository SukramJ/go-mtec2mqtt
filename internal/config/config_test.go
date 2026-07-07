// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	if c.ModbusRetries != DefaultModbusRetries {
		t.Errorf("MODBUS_RETRIES default not applied: %d", c.ModbusRetries)
	}
	if c.HASSBirthGracetime != DefaultHASSBirthGracetime {
		t.Errorf("HASS_BIRTH_GRACETIME default not applied: %d", c.HASSBirthGracetime)
	}
}

// Validate documents 0 as a legal value for MODBUS_RETRIES and
// HASS_BIRTH_GRACETIME, so an explicit 0 must not be replaced by the
// nonzero defaults — key presence, not the zero value, decides.
func TestLoadKeepsExplicitZeroRetriesAndGracetime(t *testing.T) {
	t.Run("yaml", func(t *testing.T) {
		yaml := minimumYAML + "MODBUS_RETRIES: 0\nHASS_BIRTH_GRACETIME: 0\n"
		c, err := Load(strings.NewReader(yaml), nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.ModbusRetries != 0 {
			t.Errorf("explicit MODBUS_RETRIES: 0 overwritten, got %d", c.ModbusRetries)
		}
		if c.HASSBirthGracetime != 0 {
			t.Errorf("explicit HASS_BIRTH_GRACETIME: 0 overwritten, got %d", c.HASSBirthGracetime)
		}
	})
	t.Run("env", func(t *testing.T) {
		env := fakeEnv{vars: map[string]string{
			"MTEC_MODBUS_RETRIES":       "0",
			"MTEC_HASS_BIRTH_GRACETIME": "0",
		}}
		c, err := Load(strings.NewReader(minimumYAML), env)
		if err != nil {
			t.Fatal(err)
		}
		if c.ModbusRetries != 0 {
			t.Errorf("MTEC_MODBUS_RETRIES=0 overwritten, got %d", c.ModbusRetries)
		}
		if c.HASSBirthGracetime != 0 {
			t.Errorf("MTEC_HASS_BIRTH_GRACETIME=0 overwritten, got %d", c.HASSBirthGracetime)
		}
	})
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
		"MTEC_DEVICE_NAME":   "Wohnzimmer",
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
	if c.DeviceName != "Wohnzimmer" {
		t.Errorf("MTEC_DEVICE_NAME override: %q", c.DeviceName)
	}
}

// TestEnvOverrideStringFieldsNotCoerced pins down that the bool/int/
// float coercion ladder never touches values destined for string-typed
// Config fields: a password "007" must not become "7", "True" must not
// become "true", and a numeric-looking MQTT_TOPIC must not change the
// topic layout.
func TestEnvOverrideStringFieldsNotCoerced(t *testing.T) {
	env := fakeEnv{vars: map[string]string{
		"MTEC_MQTT_PASSWORD": "007",  // int-parseable, leading zeros
		"MTEC_MQTT_LOGIN":    "1e5",  // float-parseable
		"MTEC_MQTT_TOPIC":    "0055", // topic layout must stay verbatim
		"MTEC_WEB_USER":      "True", // bool-parseable, case-sensitive
		"MTEC_WEB_PASSWORD":  "false",
		"MTEC_DEVICE_NAME":   "1.50",
	}}
	c, err := Load(strings.NewReader(minimumYAML), env)
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTTPassword != "007" {
		t.Errorf("MTEC_MQTT_PASSWORD mangled: %q", c.MQTTPassword)
	}
	if c.MQTTLogin != "1e5" {
		t.Errorf("MTEC_MQTT_LOGIN mangled: %q", c.MQTTLogin)
	}
	if c.MQTTTopic != "0055" {
		t.Errorf("MTEC_MQTT_TOPIC mangled: %q", c.MQTTTopic)
	}
	if c.WebUser != "True" {
		t.Errorf("MTEC_WEB_USER mangled: %q", c.WebUser)
	}
	if c.WebPassword != "false" {
		t.Errorf("MTEC_WEB_PASSWORD mangled: %q", c.WebPassword)
	}
	if c.DeviceName != "1.50" {
		t.Errorf("MTEC_DEVICE_NAME mangled: %q", c.DeviceName)
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

	// Locate consults APPDATA on Windows and XDG_CONFIG_HOME elsewhere,
	// so point the platform-appropriate variable at the same base dir.
	cfgHomeVar := "XDG_CONFIG_HOME"
	if runtime.GOOS == "windows" {
		cfgHomeVar = "APPDATA"
	}
	env := fakeEnv{vars: map[string]string{cfgHomeVar: xdg}}
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

func TestMQTTBrokerURL(t *testing.T) {
	cases := []struct {
		name string
		ssl  bool
		port int
		want string
	}{
		{"plain default port omitted", false, 1883, "tcp://localhost"},
		{"plain custom port appended", false, 1884, "tcp://localhost:1884"},
		{"tls default port omitted", true, 8883, "tls://localhost"},
		{"tls custom port appended", true, 8884, "tls://localhost:8884"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{MQTTServer: "localhost", MQTTPort: tc.port, MQTTSSL: tc.ssl}
			if got := c.MQTTBrokerURL(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMQTTSSLDefaultsFalse(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTTSSL {
		t.Error("MQTT_SSL should default to false")
	}
	if c.MQTTSSLInsecure {
		t.Error("MQTT_SSL_INSECURE should default to false")
	}
	if c.MQTTBrokerURL() != "tcp://localhost" {
		t.Errorf("default broker URL = %q, want tcp://localhost", c.MQTTBrokerURL())
	}
}

func TestMQTTSSLEnvOverride(t *testing.T) {
	env := fakeEnv{vars: map[string]string{
		"MTEC_MQTT_SSL":          "true",
		"MTEC_MQTT_SSL_INSECURE": "true",
	}}
	c, err := Load(strings.NewReader(minimumYAML), env)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MQTTSSL {
		t.Error("MTEC_MQTT_SSL override failed")
	}
	if !c.MQTTSSLInsecure {
		t.Error("MTEC_MQTT_SSL_INSECURE override failed")
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
