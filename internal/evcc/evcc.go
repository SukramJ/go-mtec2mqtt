// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package evcc is this bridge's side of the shared evcc profile — the
// one MQTT surface go-keba2mqtt, go-mtec2mqtt and go-zendure2mqtt expose
// for evcc (https://evcc.io), so an evcc.yaml reads the same for each:
//
//	<name>/status/<serial>/evcc/<role>/<key>   {"val": …}  (evcc: `jq: .val`)
//	<name>/set/<serial>/evcc/<role>/<key>      plain value or {"val": …}
//
// The inverter is three evcc meters at once, so every key sits below a
// role: grid, pv, battery. Units and signs are evcc's: W and kWh, grid
// power positive while importing, PV power positive while producing,
// battery power positive while discharging.
//
// The package is pure: it derives the profile's values from the
// inverter's processed registers and parses its commands. The
// coordinator owns the I/O.
package evcc

import (
	"errors"
	"fmt"
	"strings"
)

// Group is the first topic level of the profile below the serial.
const Group = "evcc"

// Meter roles.
const (
	RoleGrid    = "grid"
	RolePV      = "pv"
	RoleBattery = "battery"
)

// Keys.
const (
	KeyPower        = "power"
	KeyEnergy       = "energy"
	KeyReturnEnergy = "returnenergy"
	KeyPowerL1      = "power_l1"
	KeyPowerL2      = "power_l2"
	KeyPowerL3      = "power_l3"
	KeySoC          = "soc"
	KeyBatteryMode  = "batterymode"
)

// Item is one value of the profile: a key below a role.
type Item struct {
	Role, Key string
}

// String is the item's path below [Group], "role/key".
func (i Item) String() string { return i.Role + "/" + i.Key }

// BatteryMode is the profile's one command.
var BatteryMode = Item{RoleBattery, KeyBatteryMode}

// source maps a profile item onto the register it is derived from. sign
// turns the inverter's convention into evcc's.
type source struct {
	item Item
	mqtt string
	sign float64
}

// sources is the whole mapping. The inverter reports grid power positive
// while exporting (household consumption is inverter − grid), so grid
// values are negated; PV and battery power already have evcc's sign
// (battery positive while discharging). The same convention as evcc's own
// solinteg-gen3 template, which covers the Energybutler GEN3.
var sources = []source{
	{Item{RoleGrid, KeyPower}, "grid_power", -1},
	{Item{RoleGrid, KeyPowerL1}, "grid_a", -1},
	{Item{RoleGrid, KeyPowerL2}, "grid_b", -1},
	{Item{RoleGrid, KeyPowerL3}, "grid_c", -1},
	{Item{RoleGrid, KeyEnergy}, "grid_purchase_total", 1},
	{Item{RoleGrid, KeyReturnEnergy}, "grid_feed_total", 1},
	{Item{RolePV, KeyPower}, "pv", 1},
	{Item{RolePV, KeyEnergy}, "pv_total", 1},
	{Item{RoleBattery, KeyPower}, "battery", 1},
	{Item{RoleBattery, KeySoC}, "battery_soc", 1},
	{Item{RoleBattery, KeyEnergy}, "battery_discharge_total", 1},
	{Item{RoleBattery, KeyReturnEnergy}, "battery_charge_total", 1},
}

// StatusItems lists every item the profile publishes, for the self-echo
// guard and the documentation contract test.
func StatusItems() []Item {
	out := make([]Item, 0, len(sources)+1)
	for _, s := range sources {
		out = append(out, s.item)
	}
	return append(out, BatteryMode)
}

// Commands lists every item the profile routes.
func Commands() []Item { return []Item{BatteryMode} }

// Derive computes the profile values for the registers present in
// values (processed values keyed by MQTT key). Only items whose source is
// present and numeric are returned, so a partial read never publishes a
// guess.
func Derive(values map[string]any) map[Item]float64 {
	out := map[Item]float64{}
	for _, s := range sources {
		v, ok := toFloat(values[s.mqtt])
		if !ok {
			continue
		}
		v *= s.sign
		if v == 0 {
			v = 0 // no -0 on the wire
		}
		out[s.item] = v
	}
	return out
}

// Mode is an evcc battery mode.
type Mode string

// The three battery modes the profile supports.
const (
	ModeNormal Mode = "normal"
	ModeHold   Mode = "hold"
	ModeCharge Mode = "charge"
)

// Inverter operation modes (register 50000) the battery modes map onto,
// as evcc's solinteg-gen3 template maps them: Economic mode holds the
// battery, UPS mode charges it from the grid.
const (
	CodeGeneral  = 257
	CodeEconomic = 258
	CodeUPS      = 259
)

// Code is the inverter operation mode m runs the inverter in. Normal has
// none: leaving hold or charge restores whatever mode the inverter was in
// before.
func (m Mode) Code() (int, bool) {
	switch m {
	case ModeHold:
		return CodeEconomic, true
	case ModeCharge:
		return CodeUPS, true
	case ModeNormal:
	}
	return 0, false
}

// ErrCommand is returned for a command payload the profile cannot read.
var ErrCommand = errors.New("evcc: invalid command")

// ParseMode reads a batterymode command: evcc's integers (1 normal,
// 2 hold, 3 charge — what its custom meter sends) or the mode names. The
// other evcc modes (4 holdcharge, 5 discharge) have no inverter mode and
// are refused.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", string(ModeNormal):
		return ModeNormal, nil
	case "2", string(ModeHold):
		return ModeHold, nil
	case "3", string(ModeCharge):
		return ModeCharge, nil
	}
	return "", fmt.Errorf("%w: batterymode %q must be 1/normal, 2/hold or 3/charge", ErrCommand, s)
}

// toFloat coerces the numeric forms the register decoder emits.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}
