// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The keys 3.0.0 removed still load — ignored — and are reported, so an
// upgrade does not stop on a key that never did anything.
func TestRemovedKeysAreIgnoredAndReported(t *testing.T) {
	t.Parallel()
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Removed()) != 0 {
		t.Fatalf("removed %v without the keys", c.Removed())
	}
	env := fakeEnv{vars: map[string]string{"MTEC_MODBUS_RETRIES": "3"}}
	c, err = Load(strings.NewReader(minimumYAML+"modbus_framer: rtu\nMQTT_FLOAT_FORMAT: \"{:.2f}\"\n"), env)
	if err != nil {
		t.Fatalf("a removed key must not stop the daemon: %v", err)
	}
	want := []string{"MODBUS_FRAMER", "MODBUS_RETRIES", "MQTT_FLOAT_FORMAT"}
	if got := c.Removed(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("removed %v, want %v", got, want)
	}
}

// The config directory of releases up to 2.x is still found, after the
// current one.
func TestLocateFindsTheLegacyAppDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	base := filepath.Join(dir, "cfg")
	legacy := filepath.Join(base, LegacyAppDirName, ConfigFile)
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	homeVar := "XDG_CONFIG_HOME"
	if runtime.GOOS == "windows" {
		homeVar = "APPDATA"
	}
	env := fakeEnv{vars: map[string]string{homeVar: base}}
	if got, ok := Locate(env); !ok || got != legacy {
		t.Fatalf("Locate = %q, %v; want the legacy %q", got, ok, legacy)
	}

	current := filepath.Join(base, AppDirName, ConfigFile)
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := Locate(env); !ok || got != current {
		t.Fatalf("Locate = %q, %v; want the current %q first", got, ok, current)
	}
}
