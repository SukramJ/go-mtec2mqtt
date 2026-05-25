// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package registers

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrDecodeBounds is returned by Decode when the raw register slice is
// shorter than the register's declared Length.
var ErrDecodeBounds = errors.New("registers: decode bounds exceeded")

// Decode turns a raw window of holding-register values into a typed
// Go value, applying scaling and the M-TEC-specific formatting that
// the Python coordinator (and downstream MQTT consumers) expect.
//
// The returned value is one of:
//
//   - int      (unscaled U16/S16/U32/S32/I16/I32)
//   - float64  (scaled numeric, divided by Scale)
//   - string   (BYTE / BIT / DAT / STR)
//
// `raw` must contain at least reg.Length 16-bit words starting at the
// register's offset within a cluster read; pass the right window.
func Decode(reg *Register, raw []uint16) (any, error) {
	length := reg.Length
	if length <= 0 {
		length = 1
	}
	if len(raw) < length {
		return nil, fmt.Errorf("%w: type=%s length=%d available=%d",
			ErrDecodeBounds, reg.Type, length, len(raw))
	}

	var val any
	switch reg.Type {
	case DataU16, "":
		val = int(raw[0])
	case DataS16, DataI16:
		val = signExtend16(raw[0])
	case DataU32:
		val = int(uint32(raw[0])<<16 | uint32(raw[1]))
	case DataS32, DataI32:
		val = signExtend32(uint32(raw[0])<<16 | uint32(raw[1]))
	case DataBYTE:
		return formatBYTE(raw[:length], length)
	case DataBIT:
		return formatBIT(raw[:length], length), nil
	case DataDAT:
		if length < 3 {
			return nil, fmt.Errorf("%w: DAT requires length>=3, got %d",
				ErrDecodeBounds, length)
		}
		return formatDAT(raw[0], raw[1], raw[2]), nil
	case DataSTR:
		return decodeSTR(raw[:length]), nil
	default:
		return nil, fmt.Errorf("registers: unknown data type %q", reg.Type)
	}

	// Scaling applies to integer-typed values only. Python uses true
	// division (`/`) which always yields a float — match that so the
	// MQTT_FLOAT_FORMAT path is reached for scaled registers.
	if reg.Scale > 1 {
		if iv, ok := val.(int); ok {
			return float64(iv) / float64(reg.Scale), nil
		}
	}
	return val, nil
}

// signExtend16 turns a uint16 carrying a two's-complement value into a
// proper signed int. Avoids the more cryptic `int(int16(x))` cast so
// the intent is obvious.
func signExtend16(v uint16) int {
	if v > 32767 {
		return int(v) - 65536
	}
	return int(v)
}

// signExtend32 mirrors signExtend16 at 32-bit width.
func signExtend32(v uint32) int {
	if v > 2147483647 {
		return int(int64(v) - 4294967296)
	}
	return int(v)
}

// formatBYTE renders raw words as `XX YY ...` (decimal, two digits)
// with a double space between register groups when length > 1.
//
// The exact format is what register 10011 (firmware version) consumers
// rely on — the coordinator splits the resulting string on the double
// space to recover the two firmware halves.
func formatBYTE(raw []uint16, length int) (string, error) {
	switch length {
	case 1:
		return fmt.Sprintf("%02d %02d", raw[0]>>8, raw[0]&0xFF), nil
	case 2:
		return fmt.Sprintf("%02d %02d  %02d %02d",
			raw[0]>>8, raw[0]&0xFF,
			raw[1]>>8, raw[1]&0xFF), nil
	case 4:
		return fmt.Sprintf("%02d %02d %02d %02d  %02d %02d %02d %02d",
			raw[0]>>8, raw[0]&0xFF,
			raw[1]>>8, raw[1]&0xFF,
			raw[2]>>8, raw[2]&0xFF,
			raw[3]>>8, raw[3]&0xFF), nil
	default:
		return "", fmt.Errorf("registers: unsupported BYTE length %d", length)
	}
}

// formatBIT renders each register as a 16-bit binary string, joined
// by single spaces (matches Python f"{r:016b}").
func formatBIT(raw []uint16, length int) string {
	parts := make([]string, length)
	for i := 0; i < length; i++ {
		parts[i] = fmt.Sprintf("%016b", raw[i])
	}
	return strings.Join(parts, " ")
}

// formatDAT renders three registers as `YY-MM-DD HH:MM:SS`. The byte
// layout matches the M-TEC inverter's `Inverter date` register pair:
//
//	reg1 = year<<8 | month
//	reg2 = day<<8  | hour
//	reg3 = minute<<8 | second
func formatDAT(r1, r2, r3 uint16) string {
	return fmt.Sprintf("%02d-%02d-%02d %02d:%02d:%02d",
		r1>>8, r1&0xFF, r2>>8, r2&0xFF, r3>>8, r3&0xFF)
}

// decodeSTR concatenates `length` 16-bit words into big-endian bytes,
// decodes them as UTF-8 (falling back to Latin-1 on invalid runes),
// and strips trailing NULs and spaces.
func decodeSTR(raw []uint16) string {
	buf := make([]byte, 0, len(raw)*2)
	for _, w := range raw {
		buf = append(buf, byte(w>>8), byte(w&0xFF))
	}
	var s string
	if utf8.Valid(buf) {
		s = string(buf)
	} else {
		// Latin-1 round-trip: every byte maps to U+0000..U+00FF.
		runes := make([]rune, len(buf))
		for i, b := range buf {
			runes[i] = rune(b)
		}
		s = string(runes)
	}
	// Python does `.rstrip(" ").rstrip("\x00").rstrip(" ")` — leaving
	// no trailing space or null. strings.TrimRight handles all three
	// characters in one pass.
	return strings.TrimRight(s, " \x00")
}
