// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package registers

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadFromReader parses registers.yaml content from r. Useful for
// callers that already hold the file content (tests, embedded
// resources) — file-system access lives in [Load].
func LoadFromReader(r io.Reader, source string) (*Map, []string, error) {
	return parse(r, source)
}

// LoadFromString is a convenience wrapper around [LoadFromReader] for
// inline YAML literals in tests.
func LoadFromString(s string) (*Map, []string, error) {
	return parse(strings.NewReader(s), "<string>")
}

// Load parses registers.yaml from path and returns a populated Map.
//
// Entries with missing or empty `name` are skipped with no error —
// matches the lenient behaviour of init_register_map in Python so a
// half-edited YAML file does not bring the daemon down. Diagnostics
// for skipped entries are returned via the second result; the caller
// can log them.
func Load(path string) (*Map, []string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied registers path
	if err != nil {
		return nil, nil, fmt.Errorf("registers: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return parse(f, path)
}

// knownDataTypes is the set of Type values the decoder understands.
// Anything else would fail on every single poll via Decode, so the
// loader rejects it up front.
var knownDataTypes = map[DataType]bool{
	DataU16:  true,
	DataS16:  true,
	DataI16:  true,
	DataU32:  true,
	DataS32:  true,
	DataI32:  true,
	DataBYTE: true,
	DataBIT:  true,
	DataDAT:  true,
	DataSTR:  true,
}

// mqttWildcards are the characters an MQTT topic segment must never
// contain: publish-side topic names reject '+', '#' and NUL outright,
// so a register carrying one would fail on every poll cycle.
const mqttWildcards = "+#\x00"

// isAllDigits reports whether s is a non-empty run of ASCII digits —
// i.e. a key that was clearly meant as a Modbus address.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// validateEntry checks catalog values that would otherwise surface only
// as repeated runtime errors (broken clusters, per-poll decode failures,
// silently unscaled values). Returns a skip reason, or "" to keep the
// entry.
func validateEntry(reg *Register, isModbus bool, addr uint16) string {
	if isModbus {
		if reg.Length < 1 || reg.Length > maxReadWords {
			return fmt.Sprintf("field 'length' %d out of range 1..%d", reg.Length, maxReadWords)
		}
		if int(addr)+reg.Length > 0x10000 {
			return fmt.Sprintf("address %d + length %d exceeds the Modbus address space",
				addr, reg.Length)
		}
	}
	if reg.Scale < 1 {
		return fmt.Sprintf("field 'scale' %d must be >= 1", reg.Scale)
	}
	if !knownDataTypes[reg.Type] {
		return fmt.Sprintf("unknown data type %q", reg.Type)
	}
	return ""
}

func parse(r io.Reader, source string) (*Map, []string, error) {
	var root yaml.Node
	dec := yaml.NewDecoder(r)
	if err := dec.Decode(&root); err != nil {
		return nil, nil, fmt.Errorf("registers: decode %s: %w", source, err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return nil, nil, fmt.Errorf("registers: %s: expected a single YAML document", source)
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("registers: %s: top-level node is %v, want mapping",
			source, top.Kind)
	}

	m := &Map{
		ByKey:  make(map[string]*Register, len(top.Content)/2),
		ByAddr: make(map[uint16]*Register),
	}
	var diagnostics []string
	seenGroups := make(map[Group]bool)
	seenMQTT := make(map[string]string) // mqtt suffix → first YAML key

	// MappingNode.Content is a flat [key, value, key, value, ...] list.
	for i := 0; i+1 < len(top.Content); i += 2 {
		keyNode := top.Content[i]
		valNode := top.Content[i+1]
		key := keyNode.Value

		// yaml.Node decoding — unlike yaml.v3 map decoding — does not
		// reject duplicate mapping keys, so guard explicitly: otherwise
		// ByKey/ByAddr would keep the last definition while Clusterize
		// keeps the first, splitting the poll and write paths.
		if _, dup := m.ByKey[key]; dup {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: duplicate key, first definition wins", key))
			continue
		}

		reg := &Register{Key: key}
		// Defaults match init_register_map's OPTIONAL_PARAMETERS table.
		reg.Length = 1
		reg.Scale = 1

		if err := valNode.Decode(reg); err != nil {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: decode error: %v", key, err))
			continue
		}
		if reg.Name == "" {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: missing mandatory field 'name'", key))
			continue
		}

		// Re-apply defaults that Decode silently overwrote with zero
		// values when the YAML omits the field. yaml.v3 has no way to
		// distinguish "missing" from "explicit zero", so we restore
		// the documented defaults after the fact.
		if reg.Length == 0 {
			reg.Length = 1
		}
		if reg.Scale == 0 {
			reg.Scale = 1
		}
		if reg.Type == "" {
			reg.Type = DataU16
		}

		// Numeric keys are Modbus addresses; everything else is a
		// pseudo-register. Use ParseUint to reject negatives cleanly.
		addr, addrErr := strconv.ParseUint(key, 10, 16)
		isModbus := addrErr == nil
		if addrErr != nil && isAllDigits(key) {
			// All-digits but unparsable means the address overflows
			// uint16 — almost certainly a typo. Falling through would
			// create a never-polled pseudo-register that HA still
			// advertises, so skip loudly instead.
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: numeric key out of Modbus address range 0..65535", key))
			continue
		}

		if reason := validateEntry(reg, isModbus, uint16(addr)); reason != "" {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: %s", key, reason))
			continue
		}

		// The mqtt suffix (or, when it is empty, the register name) and
		// the group become MQTT topic segments verbatim. Wildcards make
		// every publish fail client-side; a '/' adds topic levels that
		// break the HA command subscription and orphan cleanup, but
		// read-only hierarchical keys do work, so only warn for those.
		topicKey, topicField := reg.MQTT, "mqtt"
		if reg.MQTT == "" && reg.Group != "" {
			topicKey, topicField = reg.Name, "name"
		}
		if strings.ContainsAny(topicKey, mqttWildcards) {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: field %q contains an MQTT-unsafe character", key, topicField))
			continue
		}
		if strings.ContainsAny(string(reg.Group), mqttWildcards) {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: field 'group' contains an MQTT-unsafe character", key))
			continue
		}
		if strings.Contains(topicKey, "/") || strings.Contains(string(reg.Group), "/") {
			diagnostics = append(diagnostics,
				fmt.Sprintf("keep %q: '/' in topic segment adds MQTT topic levels; "+
					"Home Assistant writes and retained-config cleanup will not work", key))
		}

		// Duplicate mqtt suffixes collide on the HA unique_id and on the
		// write path (FindByMQTT returns the first match), which could
		// route a write to the wrong holding register — keep the first.
		if reg.MQTT != "" {
			if first, dup := seenMQTT[reg.MQTT]; dup {
				diagnostics = append(diagnostics,
					fmt.Sprintf("skip %q: duplicate mqtt key %q (already used by %q)",
						key, reg.MQTT, first))
				continue
			}
			seenMQTT[reg.MQTT] = key
		}

		if isModbus {
			reg.Address = uint16(addr)
			m.ByAddr[reg.Address] = reg
		}

		m.All = append(m.All, reg)
		m.ByKey[key] = reg

		if reg.Group != "" && !seenGroups[reg.Group] {
			seenGroups[reg.Group] = true
			m.Groups = append(m.Groups, reg.Group)
		}
	}

	return m, diagnostics, nil
}
