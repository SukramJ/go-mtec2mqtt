// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"math"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

func newRegister(addr uint16, mqtt string, opts ...func(*registers.Register)) *registers.Register {
	r := &registers.Register{
		Address: addr,
		Key:     "k",
		Name:    mqtt,
		MQTT:    mqtt,
		Length:  1,
		Scale:   1,
		Type:    registers.DataU16,
		Group:   "now-base",
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// --- firmware (10011) ------------------------------------------------------

func TestFormatFirmware(t *testing.T) {
	got, ok := formatFirmware("01 27 52 20  03 04 05 06")
	if !ok {
		t.Fatal("ok=false for valid input")
	}
	want := "V1.27.52.20-V3.4.5.6"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFormatFirmwareRejectsSingleHalf(t *testing.T) {
	if _, ok := formatFirmware("01 02 03 04"); ok {
		t.Fatal("single-half input must not match")
	}
}

func TestProcessOneFormatsFirmware(t *testing.T) {
	reg := newRegister(10011, "firmware_version", func(r *registers.Register) {
		r.Type = registers.DataBYTE
		r.Length = 4
	})
	got := processOne(reg, "01 27 52 20  03 04 05 06")
	if got != "V1.27.52.20-V3.4.5.6" {
		t.Fatalf("processOne firmware: %v", got)
	}
}

// --- equipment (10008) -----------------------------------------------------

func TestParseEquipmentBytes(t *testing.T) {
	hi, lo, ok := parseEquipmentBytes("30 03")
	if !ok || hi != 30 || lo != 3 {
		t.Fatalf("got hi=%d lo=%d ok=%v", hi, lo, ok)
	}
}

func TestEquipmentLookupKnownAndUnknown(t *testing.T) {
	if got := equipmentLookup(30, 3); got != "8.0K-25A-3P" {
		t.Fatalf("known: %q", got)
	}
	if got := equipmentLookup(99, 0); got != "unknown" {
		t.Fatalf("unknown high: %q", got)
	}
	if got := equipmentLookup(30, 99); got != "unknown" {
		t.Fatalf("unknown low: %q", got)
	}
}

func TestProcessOneFormatsEquipment(t *testing.T) {
	reg := newRegister(10008, "equipment_info", func(r *registers.Register) {
		r.Type = registers.DataBYTE
	})
	got := processOne(reg, "30 03")
	if got != "8.0K-25A-3P" {
		t.Fatalf("processOne equipment: %v", got)
	}
}

// --- enum / bit-field conversion -------------------------------------------

func TestConvertCodeIntegerLookup(t *testing.T) {
	items := map[int]string{0: "wait", 1: "self-check", 2: "on-grid"}
	if got := convertCode(2, items); got != "on-grid" {
		t.Fatalf("int lookup: %q", got)
	}
	if got := convertCode(99, items); got != "Unknown" {
		t.Fatalf("missing code: %q", got)
	}
}

func TestConvertCodeBitField(t *testing.T) {
	// bits set at positions 1 and 4 → "Mains Lost" + "ISO Over Limitation"
	items := map[int]string{
		1: "Mains Lost",
		2: "Grid Voltage Fault",
		4: "ISO Over Limitation",
	}
	// 0001 0010 → bit 1 + bit 4 set
	got := convertCode("00010010", items)
	// Sort is by code, so 1 (Mains Lost) before 4 (ISO Over Limitation).
	want := "Mains Lost, ISO Over Limitation"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestConvertCodeAllZeroIsOK(t *testing.T) {
	items := map[int]string{1: "X", 2: "Y"}
	if got := convertCode("0000000000000000", items); got != "OK" {
		t.Fatalf("zero bits: %q", got)
	}
}

func TestProcessOneAppliesEnumConversion(t *testing.T) {
	reg := newRegister(10105, "inverter_status", func(r *registers.Register) {
		r.HassDeviceClass = "enum"
		r.HassValueItems = map[int]string{0: "wait", 2: "on-grid"}
	})
	if got := processOne(reg, 2); got != "on-grid" {
		t.Fatalf("enum apply: %v", got)
	}
}

func TestProcessOnePassesThroughWhenNoTransform(t *testing.T) {
	reg := newRegister(11000, "grid_power", func(r *registers.Register) {
		r.Type = registers.DataS32
	})
	if got := processOne(reg, -500); got != -500 {
		t.Fatalf("pass-through: %v", got)
	}
}

// --- formatValue -----------------------------------------------------------

func TestFormatValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{1.5, "1.50"},
		{-500, "-500"},
		{true, "1"},
		{false, "0"},
		{"hello", "hello"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := formatValue(tc.in, "%.2f"); got != tc.want {
			t.Errorf("formatValue(%v): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- pseudo-registers ------------------------------------------------------

func TestPseudoBase(t *testing.T) {
	now := time.Date(2026, 5, 25, 14, 30, 45, 0, time.UTC)
	data := map[string]any{
		keyInverterAC: 3000, // W from inverter
		keyGridPower:  -500, // negative = exporting to grid
	}
	got := PseudoRegisters("now-base", data, now)
	if got[keyConsumption] != 3500.0 {
		t.Fatalf("consumption: %v", got[keyConsumption])
	}
	if got[keyAPIDate] != "2026-05-25 14:30:45" {
		t.Fatalf("api_date: %v", got[keyAPIDate])
	}
}

func TestPseudoBaseConsumptionClampedToZero(t *testing.T) {
	// Inverter idle, grid feeding loads — inverter < grid → negative
	// raw consumption must clamp to 0 (matches Python max(0, ...)).
	data := map[string]any{keyInverterAC: 0, keyGridPower: 100}
	got := PseudoRegisters("now-base", data, time.Now())
	if got[keyConsumption] != 0.0 {
		t.Fatalf("clamp: %v", got[keyConsumption])
	}
}

func TestPseudoDay(t *testing.T) {
	data := map[string]any{
		keyPVDay:               20.0, // kWh produced
		keyGridPurchaseDay:     5.0,
		keyBatteryDischargeDay: 3.0,
		keyGridFeedDay:         8.0,
		keyBatteryChargeDay:    4.0,
	}
	got := PseudoRegisters("day", data, time.Now())
	// consumption = 20 + 5 + 3 - 8 - 4 = 16
	if got[keyConsumptionDay] != 16.0 {
		t.Fatalf("consumption_day: %v", got[keyConsumptionDay])
	}
	// autarky = 100 * (1 - 5/16) = 68.75
	if !approxEq(got[keyAutarkyRateDay].(float64), 68.75) {
		t.Fatalf("autarky_rate_day: %v", got[keyAutarkyRateDay])
	}
	// own_consumption = 100 * (1 - 8/20) = 60
	if !approxEq(got[keyOwnConsumptionDay].(float64), 60.0) {
		t.Fatalf("own_consumption_day: %v", got[keyOwnConsumptionDay])
	}
}

func TestPseudoTotalUsesDifferentKeys(t *testing.T) {
	// Identical formulas, different input keys. The catalog mismatch
	// between day ("Grid purchased energy (day)") and total ("Grid
	// energy purchased (total)") was a real footgun in the Python
	// port — this test pins both bucket plumbings.
	data := map[string]any{
		keyPVTotal:               100.0,
		keyGridPurchaseTotal:     20.0,
		keyBatteryDischargeTotal: 10.0,
		keyGridFeedTotal:         30.0,
		keyBatteryChargeTotal:    15.0,
	}
	got := PseudoRegisters("total", data, time.Now())
	if got[keyConsumptionTotal] != 85.0 {
		t.Fatalf("consumption_total: %v", got[keyConsumptionTotal])
	}
}

func TestPseudoZeroDenominatorAvoidsNaN(t *testing.T) {
	// Brand-new day, nothing produced yet: pv=0 → own_consumption stays 0
	// (not NaN), consumption=0 → autarky stays 0.
	data := map[string]any{
		keyPVDay:               0.0,
		keyGridPurchaseDay:     0.0,
		keyBatteryDischargeDay: 0.0,
		keyGridFeedDay:         0.0,
		keyBatteryChargeDay:    0.0,
	}
	got := PseudoRegisters("day", data, time.Now())
	if got[keyAutarkyRateDay] != 0.0 || got[keyOwnConsumptionDay] != 0.0 {
		t.Fatalf("zero div not guarded: %v", got)
	}
}

func TestPseudoUnknownGroupReturnsNil(t *testing.T) {
	if got := PseudoRegisters("config", nil, time.Now()); got != nil {
		t.Fatalf("config has no pseudos; got %v", got)
	}
}

// --- processValues batch ---------------------------------------------------

func TestProcessValuesBatch(t *testing.T) {
	const yaml = `
"10008":
  name: Equipment info
  length: 1
  type: BYTE
  mqtt: equipment_info
  group: static
"11000":
  name: Grid power
  length: 2
  type: I32
  unit: W
  mqtt: grid_power
  group: now-base
`
	m, _, err := registers.LoadFromString(yaml)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{
		"equipment_info": "30 03",
		"grid_power":     -500,
		"unknown_key":    "leave-me",
	}
	got := processValues(m, raw)
	if got["equipment_info"] != "8.0K-25A-3P" {
		t.Errorf("equipment_info: %v", got["equipment_info"])
	}
	if got["grid_power"] != -500 {
		t.Errorf("grid_power: %v", got["grid_power"])
	}
	if got["unknown_key"] != "leave-me" {
		t.Errorf("unknown key must pass through, got %v", got["unknown_key"])
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
