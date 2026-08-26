// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package registers

import (
	"errors"
	"testing"
)

// Vectors below mirror the equivalent paths in Python's
// _decode_register. The Python implementation is the authoritative
// reference for these formats — downstream MQTT consumers (HA, evcc)
// rely on the exact string shape.

func TestDecodeU16(t *testing.T) {
	r := &Register{Type: DataU16, Length: 1}
	v, err := Decode(r, []uint16{0x1234})
	if err != nil || v.(int) != 0x1234 {
		t.Fatalf("U16: got %v / %v, want 4660", v, err)
	}
}

func TestDecodeS16SignExtension(t *testing.T) {
	r := &Register{Type: DataS16, Length: 1}
	cases := []struct {
		raw  uint16
		want int
	}{
		{0x0000, 0},
		{0x7FFF, 32767},
		{0x8000, -32768},
		{0xFFFF, -1},
		{0xFF9C, -100},
	}
	for _, tc := range cases {
		v, err := Decode(r, []uint16{tc.raw})
		if err != nil || v.(int) != tc.want {
			t.Errorf("S16 0x%04x: got %v / %v, want %d", tc.raw, v, err, tc.want)
		}
	}
}

func TestDecodeI16AliasMatchesS16(t *testing.T) {
	r := &Register{Type: DataI16, Length: 1}
	v, err := Decode(r, []uint16{0xFFFF})
	if err != nil || v.(int) != -1 {
		t.Fatalf("I16: got %v / %v", v, err)
	}
}

func TestDecodeU32(t *testing.T) {
	r := &Register{Type: DataU32, Length: 2}
	v, err := Decode(r, []uint16{0x0001, 0x0000})
	if err != nil || v.(int64) != 0x0001_0000 {
		t.Fatalf("U32: got %v / %v", v, err)
	}
}

// TestDecodeU32FullRangeIsInt64 pins the width of the U32 result: on a
// 32-bit GOARCH (armv7 — the Home Assistant add-on image, Raspberry Pi)
// `int` is 32 bits, so an int-typed U32 would wrap 0xFFFFFFFF to −1 and
// publish a negative lifetime energy counter.
func TestDecodeU32FullRangeIsInt64(t *testing.T) {
	r := &Register{Type: DataU32, Length: 2}
	v, err := Decode(r, []uint16{0xFFFF, 0xFFFF})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := v.(int64)
	if !ok {
		t.Fatalf("U32 must decode to int64 on every GOARCH, got %T", v)
	}
	if got != 4294967295 {
		t.Fatalf("U32 0xFFFFFFFF: got %d, want 4294967295", got)
	}
	// 0x80000000 must not flip sign either.
	v, _ = Decode(r, []uint16{0x8000, 0x0000})
	if got := v.(int64); got != 2147483648 {
		t.Fatalf("U32 0x80000000: got %d, want 2147483648", got)
	}
}

// TestDecodeU32ScaledStillFloat guards the scaling branch: it type-
// switches on the integer form, so widening U32 to int64 must not drop
// scaled U32 registers (e.g. total energy, scale 10) back to raw ints.
func TestDecodeU32ScaledStillFloat(t *testing.T) {
	r := &Register{Type: DataU32, Length: 2, Scale: 10}
	v, err := Decode(r, []uint16{0x0001, 0x0000})
	f, ok := v.(float64)
	if err != nil || !ok || f != 6553.6 {
		t.Fatalf("scaled U32: got %v (%T) / %v, want 6553.6 float64", v, v, err)
	}
}

func TestDecodeS32SignExtension(t *testing.T) {
	r := &Register{Type: DataS32, Length: 2}
	// 0xFFFFFE0C == −500 (matches goldenfile fc03_resp_addr11000)
	v, err := Decode(r, []uint16{0xFFFF, 0xFE0C})
	if err != nil || v.(int) != -500 {
		t.Fatalf("S32 -500: got %v / %v", v, err)
	}
	// 0x80000000 == −2147483648 (boundary)
	v, _ = Decode(r, []uint16{0x8000, 0x0000})
	if v.(int) != -2147483648 {
		t.Fatalf("S32 min: got %v", v)
	}
	// 0x7FFFFFFF == 2147483647 (max positive)
	v, _ = Decode(r, []uint16{0x7FFF, 0xFFFF})
	if v.(int) != 2147483647 {
		t.Fatalf("S32 max: got %v", v)
	}
}

func TestDecodeScalingProducesFloat(t *testing.T) {
	// Voltage register: scale=10, raw 2348 → 234.8 V
	r := &Register{Type: DataU16, Length: 1, Scale: 10}
	v, err := Decode(r, []uint16{2348})
	f, ok := v.(float64)
	if err != nil || !ok || f != 234.8 {
		t.Fatalf("scaled U16: got %v (%T) / %v, want 234.8 float64", v, v, err)
	}
}

func TestDecodeScaleOneStaysInt(t *testing.T) {
	// Scale=1 must NOT be cast to float, else MQTT_FLOAT_FORMAT would
	// fire on values that shouldn't have decimals.
	r := &Register{Type: DataU16, Length: 1, Scale: 1}
	v, _ := Decode(r, []uint16{42})
	if _, ok := v.(int); !ok {
		t.Fatalf("unscaled value should stay int, got %T", v)
	}
}

func TestDecodeBYTELength1(t *testing.T) {
	// 0x270F → high 39, low 15 → "39 15" (DECIMAL).
	r := &Register{Type: DataBYTE, Length: 1}
	v, err := Decode(r, []uint16{0x270F})
	if err != nil || v.(string) != "39 15" {
		t.Fatalf("BYTE len=1: got %q / %v", v, err)
	}
}

func TestDecodeBYTELength2(t *testing.T) {
	r := &Register{Type: DataBYTE, Length: 2}
	v, err := Decode(r, []uint16{0x0102, 0x0304})
	// Double-space between register groups is load-bearing — the
	// coordinator's firmware-version path splits on it.
	if err != nil || v.(string) != "01 02  03 04" {
		t.Fatalf("BYTE len=2: got %q / %v", v, err)
	}
}

func TestDecodeBYTELength4(t *testing.T) {
	// Mimics the firmware register layout (10011): two firmware halves
	// each represented by two 16-bit registers, separated by a double
	// space so the coordinator can split them.
	r := &Register{Type: DataBYTE, Length: 4}
	v, err := Decode(r, []uint16{0x011B, 0x3414, 0x0101, 0x0203})
	want := "01 27 52 20  01 01 02 03"
	if err != nil || v.(string) != want {
		t.Fatalf("BYTE len=4: got %q / %v, want %q", v, err, want)
	}
}

func TestDecodeBIT(t *testing.T) {
	r := &Register{Type: DataBIT, Length: 1}
	v, _ := Decode(r, []uint16{0xABCD})
	if v.(string) != "1010101111001101" {
		t.Fatalf("BIT len=1: got %q", v)
	}
	r.Length = 2
	v, _ = Decode(r, []uint16{0x0001, 0x8000})
	if v.(string) != "0000000000000001 1000000000000000" {
		t.Fatalf("BIT len=2: got %q", v)
	}
}

func TestDecodeDAT(t *testing.T) {
	// year=26, month=5, day=25, hour=14, min=30, sec=45
	r := &Register{Type: DataDAT, Length: 3}
	v, err := Decode(r, []uint16{
		0x1A05, // 26 . 5
		0x190E, // 25 . 14
		0x1E2D, // 30 . 45
	})
	if err != nil || v.(string) != "26-05-25 14:30:45" {
		t.Fatalf("DAT: got %q / %v", v, err)
	}
}

func TestDecodeSTRTrimsNullsAndSpaces(t *testing.T) {
	// "M-TEC-SN" packed into 8 registers (last 4 are NULs).
	r := &Register{Type: DataSTR, Length: 8}
	v, err := Decode(r, []uint16{
		0x4D2D, 0x5445, 0x432D, 0x534E,
		0x0000, 0x0000, 0x0000, 0x0000,
	})
	if err != nil || v.(string) != "M-TEC-SN" {
		t.Fatalf("STR: got %q / %v", v, err)
	}
}

// TestDecodeSTRTrims0xFFPadding: inverters pad short strings with 0xFF
// (erased flash). 0xFF is invalid UTF-8, so the whole string used to
// fall into the Latin-1 branch and keep the padding as "ÿÿÿÿ" — which
// then ended up in the MQTT topic (serial number) and in the Home
// Assistant device identifier.
func TestDecodeSTRTrims0xFFPadding(t *testing.T) {
	r := &Register{Type: DataSTR, Length: 8}
	cases := []struct {
		name string
		raw  []uint16
		want string
	}{
		{
			name: "0xFF padding",
			raw: []uint16{
				0x4D2D, 0x5445, 0x432D, 0x534E,
				0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF,
			},
			want: "M-TEC-SN",
		},
		{
			name: "mixed 0xFF and NUL padding",
			raw: []uint16{
				0x4D2D, 0x5445, 0x432D, 0x534E,
				0xFF00, 0x00FF, 0xFFFF, 0x0000,
			},
			want: "M-TEC-SN",
		},
		{
			name: "odd number of padding bytes",
			raw: []uint16{
				0x4D2D, 0x5445, 0x432D, 0x53FF,
				0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF,
			},
			want: "M-TEC-S",
		},
		{
			name: "all padding",
			raw: []uint16{
				0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF,
				0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF,
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Decode(r, tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.(string); got != tc.want {
				t.Fatalf("STR: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeSTRLatin1Fallback(t *testing.T) {
	// 0xC4 is 'Ä' in Latin-1 but not a valid UTF-8 start byte on its own,
	// so the decoder must fall back to Latin-1 instead of producing
	// replacement characters (or panicking).
	r := &Register{Type: DataSTR, Length: 2}
	v, err := Decode(r, []uint16{0xC400, 0x4100})
	if err != nil {
		t.Fatal(err)
	}
	s := v.(string)
	if s == "" {
		t.Fatal("Latin-1 fallback produced empty string")
	}
}

func TestDecodeBoundsCheck(t *testing.T) {
	r := &Register{Type: DataU32, Length: 2}
	_, err := Decode(r, []uint16{0x0001})
	if !errors.Is(err, ErrDecodeBounds) {
		t.Fatalf("want ErrDecodeBounds, got %v", err)
	}
}

// TestDecodeU32MisdeclaredLengthReturnsError guards against a
// registers.yaml entry that wrongly declares a 32-bit type with
// length=1. Without the explicit length>=2 check, the U32 case would
// index raw[1] and panic instead of failing gracefully.
func TestDecodeU32MisdeclaredLengthReturnsError(t *testing.T) {
	r := &Register{Type: DataU32, Length: 1}
	_, err := Decode(r, []uint16{0x1234})
	if !errors.Is(err, ErrDecodeBounds) {
		t.Fatalf("want ErrDecodeBounds, got %v", err)
	}
}

// TestDecodeS32I32MisdeclaredLengthReturnsError mirrors
// TestDecodeU32MisdeclaredLengthReturnsError for the S32/I32 case,
// which shares the raw[1] access.
func TestDecodeS32I32MisdeclaredLengthReturnsError(t *testing.T) {
	for _, typ := range []DataType{DataS32, DataI32} {
		r := &Register{Type: typ, Length: 1}
		_, err := Decode(r, []uint16{0x1234})
		if !errors.Is(err, ErrDecodeBounds) {
			t.Errorf("%s: want ErrDecodeBounds, got %v", typ, err)
		}
	}
}

func TestDecodeUnknownType(t *testing.T) {
	r := &Register{Type: "WAT", Length: 1}
	if _, err := Decode(r, []uint16{0}); err == nil {
		t.Fatal("expected error for unknown type")
	}
}
