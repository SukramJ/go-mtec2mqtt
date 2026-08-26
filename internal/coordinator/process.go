// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// processValues replays every coordinator-side transformation the
// Python `_process_register_value` applies before publishing:
//
//   - register 10011 (firmware_version): split on the BYTE-decoder's
//     double space and reformat as "Vx.x.x.x-Vy.y.y.y"
//   - register 10008 (equipment_info): parse the two-byte payload and
//     look up the inverter model code
//   - any register with hass_value_items: translate codes / bit-fields
//     into human-readable labels
//
// The input is a map keyed by MQTT suffix (Name fallback) as returned
// by modbus.Reader.ReadGroup; the returned map has the same keys but
// the values may have changed type (raw int → label string).
//
// lang selects the enum label language ("en"/"de"); the published label
// must match the HA select options (which are localised) so a German
// state value lands on a German option instead of showing as "unknown".
func processValues(catalog *registers.Map, raw map[string]any, lang string) map[string]any {
	out := make(map[string]any, len(raw))
	for key, val := range raw {
		reg := findRegisterByOutputKey(catalog, key)
		if reg == nil {
			out[key] = val
			continue
		}
		out[key] = processOne(reg, val, lang)
	}
	return out
}

// processOne applies the single-register transformations. Exposed for
// targeted tests; ProcessValues batches it across the whole group.
func processOne(reg *registers.Register, val any, lang string) any {
	switch reg.Address {
	case 10011:
		if s, ok := val.(string); ok {
			if formatted, ok := formatFirmware(s); ok {
				return formatted
			}
		}
	case 10008:
		if s, ok := val.(string); ok {
			if hi, lo, ok := parseEquipmentBytes(s); ok {
				return equipmentLookup(hi, lo)
			}
		}
	}

	// The presence of hass_value_items is the only criterion: it is what
	// makes a raw code translatable. Tying this to
	// hass_device_class=enum used to silently disable the conversion for
	// every register that carries labels without claiming the enum
	// device class (the BIT fault/alarm registers, selects), publishing
	// raw bit strings instead of fault names.
	if reg.HassValueItems != nil {
		return convertCode(val, reg.LocalizedValueItems(lang))
	}
	return val
}

// formatFirmware turns the register 10011 BYTE-decoder output
// ("01 27 52 20  03 04 05 06") into the conventional firmware
// version string "V1.27.52.20-V3.4.5.6". Returns ok=false when the
// input doesn't match the expected two-halves shape.
func formatFirmware(s string) (string, bool) {
	parts := strings.Split(s, "  ")
	if len(parts) != 2 {
		return "", false
	}
	left := strings.ReplaceAll(stripLeadingZeros(parts[0]), " ", ".")
	right := strings.ReplaceAll(stripLeadingZeros(parts[1]), " ", ".")
	if left == "" || right == "" {
		return "", false
	}
	return "V" + left + "-V" + right, true
}

// stripLeadingZeros drops leading zeros from each space-separated
// numeric token so "01 27 52 20" reads as "1.27.52.20" not
// "01.27.52.20" — matches the Python behaviour where `.replace(' ', '.')`
// runs on the already-decimal byte strings without zero padding cleanup.
//
// In practice the Python output reads e.g. "01.27.52.20" too (BYTE
// decoder formats with %02d). We deliberately diverge here because
// "V01.27.52.20" looks broken and the upstream HA dashboards display
// V-strings raw. Document this if/when the divergence ever causes a
// surprise.
func stripLeadingZeros(s string) string {
	parts := strings.Split(s, " ")
	for i, p := range parts {
		// Trim leading zeros but keep at least one digit so "00" → "0".
		trimmed := strings.TrimLeft(p, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		parts[i] = trimmed
	}
	return strings.Join(parts, " ")
}

// parseEquipmentBytes splits "HH LL" (decimal) into (high, low).
// Returns ok=false on malformed input rather than guessing.
func parseEquipmentBytes(s string) (hi, lo int, ok bool) {
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, 0, false
	}
	hi, errHi := strconv.Atoi(parts[0])
	lo, errLo := strconv.Atoi(parts[1])
	if errHi != nil || errLo != nil {
		return 0, 0, false
	}
	return hi, lo, true
}

// convertCode maps a register value to a human-readable label using
// the catalog's hass_value_items map. Two cases mirror Python's
// _convert_code:
//
//   - integer value → single label (or "Unknown" when the code is
//     missing)
//   - string value (BIT register output, e.g. "0000000000000001
//     1000000000000000") → comma-joined list of every label whose
//     mask matches; "OK" when the field is all zeroes
//
// For the BIT case the hass_value_items keys are bit MASKS, not bit
// positions: registers.yaml declares 1, 2, 4, … 32768 (fault_flag_2
// starts at 2, the BMS protection/alarm codes run up to 32768). Testing
// `1<<key` instead of `key` reported the wrong fault for every mask
// beyond 1 and could not match the high masks at all.
func convertCode(val any, items map[int]string) string {
	if iv, ok := toInt(val); ok {
		if label, has := items[iv]; has {
			return label
		}
		return "Unknown"
	}
	s, ok := val.(string)
	if !ok {
		return "Unknown"
	}
	flat := strings.ReplaceAll(s, " ", "")
	if flat == "" {
		return "OK"
	}
	bits, err := strconv.ParseUint(flat, 2, 64)
	if err != nil {
		// Unparseable bit field (garbage, or wider than 64 bits): we
		// cannot prove the device is fault-free, so report it as unknown
		// rather than fail open with a reassuring "OK" — same contract
		// as the integer path.
		return "Unknown"
	}
	if bits == 0 {
		return "OK"
	}
	// Sort by mask value so the output is deterministic — Python's dict
	// iteration order is insertion-ordered (YAML order); Go's is
	// randomised. Ascending masks means lowest bits first, matching the
	// catalog order.
	codes := make([]int, 0, len(items))
	for c := range items {
		codes = append(codes, c)
	}
	sortInts(codes)
	var faults []string
	for _, c := range codes {
		if c <= 0 {
			continue // 0 (and any negative) is not a usable mask
		}
		mask := uint64(c)
		if bits&mask != 0 {
			faults = append(faults, items[c])
		}
	}
	if len(faults) == 0 {
		return "OK"
	}
	return strings.Join(faults, ", ")
}

// toInt accepts int, int64, float64 and returns the corresponding int.
// Reader values flow through `any` so an inverter_status register may
// land as an int (raw U16/S16), an int64 (U32/S32 — 64-bit-wide so the
// decoder cannot overflow on a 32-bit platform) or a float64 (after
// scaling).
func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// findRegisterByOutputKey reverses the "MQTT key, Name fallback" rule
// the Reader uses for its map keys. Linear scan — the catalog has
// ~100 entries and this runs once per publish cycle.
func findRegisterByOutputKey(catalog *registers.Map, key string) *registers.Register {
	for _, r := range catalog.All {
		if r.MQTT == key {
			return r
		}
	}
	for _, r := range catalog.All {
		if r.MQTT == "" && r.Name == key {
			return r
		}
	}
	return nil
}

// formatValue renders one published value to a payload string. The
// shape matches Python's coordinator: floats go through the
// MQTT_FLOAT_FORMAT spec, booleans become "1" / "0", everything else
// is Sprintf-stringified.
//
// Integers deliberately share the default branch: %v renders int, int32
// and the int64 an unscaled U32/S32 register decodes to identically
// (no exponent, no thousands separator), so a wide register publishes
// its full value without a width-specific case here.
func formatValue(v any, floatFmt string) string {
	switch x := v.(type) {
	case float64:
		return fmt.Sprintf(floatFmt, x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

// sortInts is a 6-line insertion sort. Pulled out so the body of
// convertCode stays focused on the bit-decoding logic.
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
