// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"math"
	"strings"
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
	got := processOne(reg, "01 27 52 20  03 04 05 06", "en")
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
	got := processOne(reg, "30 03", "en")
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

// faultFlag1Items mirrors the hass_value_items of register 10112
// (fault_flag_1) in registers.yaml verbatim. The keys are bit MASKS,
// not bit positions — the whole point of the test below.
var faultFlag1Items = map[int]string{
	1:   "Mains Lost",
	2:   "Grid Voltage Fault",
	4:   "Grid Frequency Fault",
	8:   "DCI Fault",
	16:  "ISO Over Limitation",
	32:  "GFCI Fault",
	64:  "PV Over Voltage",
	128: "Bus Voltage Fault",
}

func TestConvertCodeBitFieldMaskSemantics(t *testing.T) {
	cases := []struct {
		name string
		bits string
		want string
	}{
		// 0x0001: the lowest bit is mask 1 — the very first fault the
		// inverter can raise, and the one a position-based test happened
		// to get right only by accident (1<<0 == 1).
		{"single lowest bit", "0000000000000001", "Mains Lost"},
		// 0x0010 is mask 16, not "bit 16": the old 1<<code test looked at
		// bit 16 of the field and reported nothing at all here.
		{"single mid bit", "0000000000010000", "ISO Over Limitation"},
		// 0x0080 → the highest mask this register defines.
		{"highest defined mask", "0000000010000000", "Bus Voltage Fault"},
		// 0x0011 = masks 1 + 16, listed in ascending mask order.
		{"two bits", "0000000000010001", "Mains Lost, ISO Over Limitation"},
		// 0x00FF → every defined fault at once, ascending.
		{"all bits", "0000000011111111", "Mains Lost, Grid Voltage Fault, " +
			"Grid Frequency Fault, DCI Fault, ISO Over Limitation, GFCI Fault, " +
			"PV Over Voltage, Bus Voltage Fault"},
		// A bit the catalog has no label for is simply not reported; the
		// register is still faulty but we have nothing to name.
		{"undocumented bit only", "0000000100000000", "OK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := convertCode(tc.bits, faultFlag1Items); got != tc.want {
				t.Fatalf("convertCode(%q) = %q, want %q", tc.bits, got, tc.want)
			}
		})
	}
}

// TestConvertCodeBitFieldHighMask covers the BMS protection/alarm
// registers, whose masks run up to 32768 (0x8000). Under the old
// position semantics 1<<32768 could never match anything, so a leakage
// current protection event published as "OK".
func TestConvertCodeBitFieldHighMask(t *testing.T) {
	items := map[int]string{
		2:     "Cells High Voltage Protection",
		16384: "Ambient High Temperature Protection",
		32768: "Leakage Current Protection",
	}
	if got := convertCode("1000000000000000", items); got != "Leakage Current Protection" {
		t.Fatalf("0x8000 mask: %q", got)
	}
	if got := convertCode("1000000000000010", items); got != "Cells High Voltage Protection, Leakage Current Protection" {
		t.Fatalf("0x8002 mask combination: %q", got)
	}
	// A two-register (length: 2) BIT decode arrives as two space-joined
	// 16-bit words; the flattened field keeps the low word's bits in
	// place, so the same masks still match.
	if got := convertCode("0000000000000000 1000000000000000", items); got != "Leakage Current Protection" {
		t.Fatalf("two-word field: %q", got)
	}
}

func TestConvertCodeAllZeroIsOK(t *testing.T) {
	items := map[int]string{1: "X", 2: "Y"}
	if got := convertCode("0000000000000000", items); got != "OK" {
		t.Fatalf("zero bits: %q", got)
	}
}

// TestConvertCodeUnparseableBitFieldIsUnknown pins the fail-closed
// contract: if we cannot read the bit field (garbage, or wider than the
// 64 bits ParseUint handles) we must not claim the inverter is fine.
func TestConvertCodeUnparseableBitFieldIsUnknown(t *testing.T) {
	items := map[int]string{1: "Mains Lost"}
	tooWide := strings.Repeat("1", 80)
	for _, in := range []string{tooWide, "not-binary", "012"} {
		if got := convertCode(in, items); got != "Unknown" {
			t.Errorf("convertCode(%.12q…) = %q, want Unknown", in, got)
		}
	}
}

func TestProcessOneAppliesEnumConversion(t *testing.T) {
	reg := newRegister(10105, "inverter_status", func(r *registers.Register) {
		r.HassDeviceClass = "enum"
		r.HassValueItems = map[int]string{0: "wait", 2: "on-grid"}
	})
	if got := processOne(reg, 2, "en"); got != "on-grid" {
		t.Fatalf("enum apply: %v", got)
	}
}

// TestProcessOneConvertsWithoutEnumDeviceClass pins the criterion for
// label conversion: the presence of hass_value_items, nothing else. The
// BIT fault registers carry labels without claiming
// hass_device_class=enum, and tying the conversion to the device class
// published the raw bit string instead of the fault name.
func TestProcessOneConvertsWithoutEnumDeviceClass(t *testing.T) {
	bitReg := newRegister(10112, "fault_flag_1", func(r *registers.Register) {
		r.Type = registers.DataBIT
		r.Length = 2
		r.HassValueItems = faultFlag1Items
	})
	if got := processOne(bitReg, "0000000000000000 0000000000010001", "en"); got != "Mains Lost, ISO Over Limitation" {
		t.Errorf("BIT register without enum device class = %v", got)
	}

	// A select register that only declares its options, no device class.
	selReg := newRegister(52000, "mode", func(r *registers.Register) {
		r.HassComponentType = "select"
		r.HassValueItems = map[int]string{0: "General", 1: "Eco"}
	})
	if got := processOne(selReg, 1, "en"); got != "Eco" {
		t.Errorf("select register without enum device class = %v", got)
	}
}

func TestProcessOneEnumIsLocalised(t *testing.T) {
	// The published enum label must follow the configured language so it
	// matches the (localised) HA select options instead of showing as
	// "unknown".
	reg := newRegister(50000, "mode", func(r *registers.Register) {
		r.HassDeviceClass = "enum"
		r.HassValueItems = map[int]string{257: "General mode"}
		r.HassValueItemsDE = map[int]string{257: "Allgemeiner Modus"}
	})
	if got := processOne(reg, 257, "de"); got != "Allgemeiner Modus" {
		t.Errorf("de enum = %v, want Allgemeiner Modus", got)
	}
	if got := processOne(reg, 257, "en"); got != "General mode" {
		t.Errorf("en enum = %v, want General mode", got)
	}
}

func TestProcessOnePassesThroughWhenNoTransform(t *testing.T) {
	reg := newRegister(11000, "grid_power", func(r *registers.Register) {
		r.Type = registers.DataS32
	})
	if got := processOne(reg, -500, "en"); got != -500 {
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
		// U32/S32 registers decode to int64 so a wide counter cannot
		// overflow on a 32-bit build; the payload must be the plain
		// number, not an exponent or a truncated value.
		{int64(4294967295), "4294967295"},
		{int64(-2147483648), "-2147483648"},
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
	got, skipped := PseudoRegisters("now-base", data, now)
	if got[keyConsumption] != 3500.0 {
		t.Fatalf("consumption: %v", got[keyConsumption])
	}
	if got[keyAPIDate] != "2026-05-25 14:30:45" {
		t.Fatalf("api_date: %v", got[keyAPIDate])
	}
	if len(skipped) != 0 {
		t.Fatalf("nothing should be skipped with complete inputs: %v", skipped)
	}
}

// TestPseudoBaseAcceptsWideIntegers guards the U32/S32 → int64 decoder
// contract: the formulas must not silently treat a 64-bit register as a
// missing input.
func TestPseudoBaseAcceptsWideIntegers(t *testing.T) {
	data := map[string]any{
		keyInverterAC: int64(3000),
		keyGridPower:  int64(-500),
	}
	got, skipped := PseudoRegisters("now-base", data, time.Now())
	if got[keyConsumption] != 3500.0 || len(skipped) != 0 {
		t.Fatalf("int64 inputs: consumption=%v skipped=%v", got[keyConsumption], skipped)
	}
}

func TestPseudoBaseConsumptionClampedToZero(t *testing.T) {
	// Inverter idle, grid feeding loads — inverter < grid → negative
	// raw consumption must clamp to 0 (matches Python max(0, ...)).
	data := map[string]any{keyInverterAC: 0, keyGridPower: 100}
	got, _ := PseudoRegisters("now-base", data, time.Now())
	if got[keyConsumption] != 0.0 {
		t.Fatalf("clamp: %v", got[keyConsumption])
	}
}

// TestPseudoBaseSkipsConsumptionOnPartialRead is the regression: when
// the inverter cluster read fails, the grid power alone used to yield
// consumption = 0 - (-500) = 500 W — a believable but wrong number that
// lands in Home Assistant's long-term statistics. The value must be
// skipped instead, leaving the last good one in place.
func TestPseudoBaseSkipsConsumptionOnPartialRead(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"inverter missing":   {keyGridPower: -500},
		"grid power missing": {keyInverterAC: 3000},
		"both missing":       {},
	} {
		t.Run(name, func(t *testing.T) {
			got, skipped := PseudoRegisters("now-base", data, time.Now())
			if _, ok := got[keyConsumption]; ok {
				t.Errorf("consumption published from an incomplete read: %v", got[keyConsumption])
			}
			if len(skipped) != 1 || skipped[0] != keyConsumption {
				t.Errorf("skipped = %v, want [%s]", skipped, keyConsumption)
			}
			// api_date has no register inputs — it must still be published.
			if _, ok := got[keyAPIDate]; !ok {
				t.Error("api_date must survive a partial read")
			}
		})
	}
}

// TestPseudoPeriodSkipsOnPartialRead checks the per-formula gating: the
// energy counters that own_consumption needs (pv, grid_feed) can be
// present while a counter only consumption/autarky needs is missing.
func TestPseudoPeriodSkipsOnPartialRead(t *testing.T) {
	// battery_charge_day absent → consumption + autarky skipped,
	// own_consumption still computable from pv + grid_feed.
	data := map[string]any{
		keyPVDay:               20.0,
		keyGridPurchaseDay:     5.0,
		keyBatteryDischargeDay: 3.0,
		keyGridFeedDay:         8.0,
	}
	got, skipped := PseudoRegisters("day", data, time.Now())
	if _, ok := got[keyConsumptionDay]; ok {
		t.Errorf("consumption_day published without battery_charge_day: %v", got[keyConsumptionDay])
	}
	if _, ok := got[keyAutarkyRateDay]; ok {
		t.Errorf("autarky_rate_day published without battery_charge_day: %v", got[keyAutarkyRateDay])
	}
	if !approxEq(got[keyOwnConsumptionDay].(float64), 60.0) {
		t.Errorf("own_consumption_day = %v, want 60 (its own inputs are present)", got[keyOwnConsumptionDay])
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v, want the two consumption-formula keys", skipped)
	}

	// pv absent → own_consumption goes too.
	delete(data, keyPVDay)
	got, skipped = PseudoRegisters("day", data, time.Now())
	if len(got) != 0 || len(skipped) != 3 {
		t.Errorf("all three must be skipped without pv: got=%v skipped=%v", got, skipped)
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
	got, _ := PseudoRegisters("day", data, time.Now())
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
	got, _ := PseudoRegisters("total", data, time.Now())
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
	got, _ := PseudoRegisters("day", data, time.Now())
	if got[keyAutarkyRateDay] != 0.0 || got[keyOwnConsumptionDay] != 0.0 {
		t.Fatalf("zero div not guarded: %v", got)
	}
}

func TestPseudoUnknownGroupReturnsNil(t *testing.T) {
	got, skipped := PseudoRegisters("config", nil, time.Now())
	if got != nil || skipped != nil {
		t.Fatalf("config has no pseudos; got %v / %v", got, skipped)
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
	got := processValues(m, raw, "en")
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
