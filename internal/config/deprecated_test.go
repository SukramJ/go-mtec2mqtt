// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"strings"
	"testing"
)

func TestDeprecatedKeys(t *testing.T) {
	t.Parallel()
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Deprecated()) != 0 {
		t.Fatalf("deprecated %v without the keys", c.Deprecated())
	}
	env := fakeEnv{vars: map[string]string{"MTEC_MODBUS_RETRIES": "3"}}
	c, err = Load(strings.NewReader(minimumYAML+"modbus_framer: rtu\nMQTT_FLOAT_FORMAT: \".2f\"\n"), env)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"MODBUS_FRAMER", "MODBUS_RETRIES", "MQTT_FLOAT_FORMAT"}
	if got := c.Deprecated(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deprecated %v, want %v", got, want)
	}
}
