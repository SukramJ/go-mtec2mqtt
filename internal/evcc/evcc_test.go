// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package evcc

import (
	"errors"
	"math"
	"testing"
)

// The inverter exports with a positive grid power; evcc wants import
// positive. PV and battery keep the inverter's sign.
func TestDeriveSigns(t *testing.T) {
	t.Parallel()
	got := Derive(map[string]any{
		"grid_power":              int64(1200), // exporting 1.2 kW
		"grid_a":                  int64(-300), // importing on phase A
		"grid_b":                  int64(0),
		"pv":                      int64(4000),
		"battery":                 int64(-2500), // charging
		"battery_soc":             55.5,
		"grid_purchase_total":     1234.5,
		"grid_feed_total":         2345.6,
		"pv_total":                9876.5,
		"battery_charge_total":    111.1,
		"battery_discharge_total": 99.9,
		"inverter":                int64(3000), // not part of the profile
	})
	want := map[Item]float64{
		{RoleGrid, KeyPower}:           -1200,
		{RoleGrid, KeyPowerL1}:         300,
		{RoleGrid, KeyPowerL2}:         0,
		{RolePV, KeyPower}:             4000,
		{RoleBattery, KeyPower}:        -2500,
		{RoleBattery, KeySoC}:          55.5,
		{RoleGrid, KeyEnergy}:          1234.5,
		{RoleGrid, KeyReturnEnergy}:    2345.6,
		{RolePV, KeyEnergy}:            9876.5,
		{RoleBattery, KeyReturnEnergy}: 111.1,
		{RoleBattery, KeyEnergy}:       99.9,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d items %v, want %d", len(got), got, len(want))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, w)
		}
	}
	if v := got[Item{RoleGrid, KeyPowerL2}]; math.Signbit(v) {
		t.Error("a zero grid power is published as -0")
	}
}

// A register missing from the read, or not numeric, yields nothing.
func TestDerivePartial(t *testing.T) {
	t.Parallel()
	got := Derive(map[string]any{"battery_soc": 80.0, "pv": "n/a"})
	if len(got) != 1 || got[Item{RoleBattery, KeySoC}] != 80 {
		t.Fatalf("got %v", got)
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Mode{
		"1": ModeNormal, "2": ModeHold, "3": ModeCharge,
		"normal": ModeNormal, "Hold\n": ModeHold, "CHARGE": ModeCharge,
	} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "4", "5", "holdcharge", "discharge", ""} {
		if _, err := ParseMode(in); !errors.Is(err, ErrCommand) {
			t.Errorf("ParseMode(%q) err = %v, want ErrCommand", in, err)
		}
	}
}

func TestModeCode(t *testing.T) {
	t.Parallel()
	if c, ok := ModeHold.Code(); !ok || c != CodeEconomic {
		t.Errorf("hold → %d %v", c, ok)
	}
	if c, ok := ModeCharge.Code(); !ok || c != CodeUPS {
		t.Errorf("charge → %d %v", c, ok)
	}
	if _, ok := ModeNormal.Code(); ok {
		t.Error("normal must have no fixed code: it restores")
	}
}

func TestStatusItemsAreUnique(t *testing.T) {
	t.Parallel()
	seen := map[Item]bool{}
	for _, it := range StatusItems() {
		if seen[it] {
			t.Errorf("duplicate %s", it)
		}
		seen[it] = true
	}
	if !seen[BatteryMode] {
		t.Error("batterymode status missing")
	}
}

func TestSourceKey(t *testing.T) {
	t.Parallel()
	if got := SourceKey(Item{RoleGrid, KeyPower}); got != "grid_power" {
		t.Errorf("grid power source = %q", got)
	}
	if got := SourceKey(BatteryMode); got != "" {
		t.Errorf("batterymode source = %q, want none", got)
	}
}
