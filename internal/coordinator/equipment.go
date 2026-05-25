// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

// equipment maps the two-byte payload of the inverter's "Equipment
// info" register (10008) to the human-readable model code printed on
// the device label. The outer key is the high byte, the inner the
// low byte — exactly the layout produced by registers.Decode's
// BYTE-length-1 formatter.
//
// Mirrors the EQUIPMENT table in aiomtec2mqtt/const.py so reported
// model strings stay identical across the two implementations.
var equipment = map[int]map[int]string{
	30: {
		0: "4.0K-25A-3P",
		1: "5.0K-25A-3P",
		2: "6.0K-25A-3P",
		3: "8.0K-25A-3P",
		4: "10K-25A-3P",
		5: "12K-25A-3P",
		6: "10K-40A-3P",
		7: "12K-40A-3P",
		8: "15K-40A-3P",
		9: "20K-40A-3P",
	},
	31: {
		0: "3.0K-30A-1P",
		1: "3.6K-30A-1P",
		2: "4.2K-30A-1P",
		3: "4.6K-30A-1P",
		4: "5.0K-30A-1P",
		5: "6.0K-30A-1P",
		6: "7.0K-30A-1P",
		7: "8.0K-30A-1P",
		8: "3.0K-30A-1P-S",
		9: "3.6K-30A-1P-S",
	},
	32: {
		0: "25K-100A-3P",
		1: "30K-100A-3P",
		2: "36K-100A-3P",
		3: "40K-100A-3P",
		4: "50K-100A-3P",
	},
}

// equipmentLookup resolves a "HH LL" pair (decimal, space-separated)
// into the model string. Returns "unknown" when either byte is out of
// range — matches the Python coordinator's fallback.
func equipmentLookup(high, low int) string {
	if inner, ok := equipment[high]; ok {
		if model, ok := inner[low]; ok {
			return model
		}
	}
	return "unknown"
}

// secondaryGroups is the round-robin order the coordinator cycles
// through on each REFRESH_NOW tick. Index N % len gives the group to
// poll next. Order matches SECONDARY_REGISTER_GROUPS in const.py so
// the on-the-wire pattern is unchanged.
var secondaryGroups = []string{
	"now-grid",
	"now-inverter",
	"now-backup",
	"now-battery",
	"now-pv",
}
