// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"strings"
	"testing"
	"time"
)

func TestEVCCDefaultsOff(t *testing.T) {
	t.Parallel()
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.EVCCEnable || c.EVCCBatteryModeTimeoutDuration() != 0 {
		t.Fatalf("evcc on by default: %v %v", c.EVCCEnable, c.EVCCBatteryModeTimeout)
	}
}

func TestEVCCFromEnv(t *testing.T) {
	t.Parallel()
	env := fakeEnv{vars: map[string]string{
		"MTEC_EVCC_ENABLE":              "true",
		"MTEC_EVCC_BATTERYMODE_TIMEOUT": "120",
	}}
	c, err := Load(strings.NewReader(minimumYAML), env)
	if err != nil {
		t.Fatal(err)
	}
	if !c.EVCCEnable || c.EVCCBatteryModeTimeoutDuration() != 2*time.Minute {
		t.Fatalf("got %v %v", c.EVCCEnable, c.EVCCBatteryModeTimeout)
	}
}

func TestEVCCTimeoutRange(t *testing.T) {
	t.Parallel()
	_, err := Load(strings.NewReader(minimumYAML+"EVCC_BATTERYMODE_TIMEOUT: -1\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "EVCC_BATTERYMODE_TIMEOUT") {
		t.Fatalf("err = %v", err)
	}
}
