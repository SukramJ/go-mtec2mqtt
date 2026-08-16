// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package registers

import (
	"fmt"
	"io"
	"os"
	"reflect"
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

// maxBITWords caps the word count of a BIT register. The coordinator
// turns the decoder's binary string back into a bitmask via
// strconv.ParseUint(..., 2, 64), so anything past 64 bits (4 words)
// would silently degrade to "OK" — no fault would ever be reported.
const maxBITWords = 4

// mergeKey is YAML's merge-key indicator. yaml.Node.Decode resolves it
// itself, so the unknown-field scan must not flag it.
const mergeKey = "<<"

// knownRegisterFields is the set of YAML keys a Register accepts,
// derived from the struct's `yaml:` tags so schema and validation can
// never drift apart. Fields tagged "-" (Key, Address) are computed by
// the loader and not settable from the catalog.
var knownRegisterFields = buildKnownRegisterFields()

func buildKnownRegisterFields() map[string]bool {
	t := reflect.TypeOf(Register{})
	fields := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = true
	}
	return fields
}

// scanFields walks a register's mapping node and reports which schema
// fields the YAML actually spells out, plus any key that is not part
// of the schema at all.
//
// Both results exist because yaml.Node.Decode has no KnownFields
// equivalent: without this, a mistyped `writeable:` (or a mis-cased
// `Scale:`) is dropped without a trace, and an explicit `scale: 0`
// is indistinguishable from an omitted one.
func scanFields(n *yaml.Node) (present map[string]bool, unknown []string) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	present = make(map[string]bool, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch name := n.Content[i].Value; {
		case name == mergeKey:
			// Resolved by Decode; not a schema field of its own.
		case knownRegisterFields[name]:
			present[name] = true
		default:
			unknown = append(unknown, name)
		}
	}
	return present, unknown
}

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
	// A zero or negative length is nonsense for both register kinds —
	// only the upper bound is Modbus-specific (FC03 read limit).
	if reg.Length < 1 {
		return fmt.Sprintf("field 'length' %d must be >= 1", reg.Length)
	}
	if isModbus {
		if reg.Length > maxReadWords {
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
	return validateTypeLength(reg.Type, reg.Length)
}

// validateTypeLength cross-checks the declared word count against what
// the decoder in decode.go actually consumes for that type. Without it
// a mis-declared entry (`type: U32` with the default `length: 1`) is
// accepted at load time and then fails on every single poll — for the
// whole cluster's worth of neighbours, once per refresh interval.
func validateTypeLength(typ DataType, length int) string {
	switch typ {
	case DataU32, DataS32, DataI32:
		// Decode reads raw[0] and raw[1].
		if length < 2 {
			return fmt.Sprintf("type %s requires 'length' >= 2, got %d", typ, length)
		}
	case DataDAT:
		// formatDAT reads raw[0..2] (date, time, seconds).
		if length < 3 {
			return fmt.Sprintf("type %s requires 'length' >= 3, got %d", typ, length)
		}
	case DataBYTE:
		// formatBYTE only implements these three layouts.
		if length != 1 && length != 2 && length != 4 {
			return fmt.Sprintf("type %s supports 'length' 1, 2 or 4, got %d", typ, length)
		}
	case DataBIT:
		if length > maxBITWords {
			return fmt.Sprintf("type %s supports at most %d words (64 bits), got %d",
				typ, maxBITWords, length)
		}
	case DataU16, DataS16, DataI16, DataSTR, "":
		// Single-word types ignore any extra words; STR spans any count.
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
	seenAddr := make(map[uint16]string) // modbus address → first YAML key

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

		present, unknown := scanFields(valNode)

		reg := &Register{Key: key}
		// Defaults match init_register_map's OPTIONAL_PARAMETERS table.
		reg.Length = 1
		reg.Scale = 1

		if err := valNode.Decode(reg); err != nil {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: decode error: %v", key, err))
			continue
		}
		// A field yaml.v3 cannot map is dropped without a word, so a
		// `writeable:` typo silently turns a writable register into a
		// read-only one. The entry itself is still usable — warn and
		// keep it rather than making one typo delete an entity.
		for _, name := range unknown {
			diagnostics = append(diagnostics,
				fmt.Sprintf("keep %q: unknown field %q is not part of the register schema "+
					"and was ignored", key, name))
		}
		if reg.Name == "" {
			diagnostics = append(diagnostics,
				fmt.Sprintf("skip %q: missing mandatory field 'name'", key))
			continue
		}

		// Restore the documented defaults only for fields the YAML does
		// not mention. An explicitly written `length: 0` / `scale: 0` is
		// a catalog bug (a scale of 0 would be a division by zero) and
		// must reach validateEntry instead of being papered over here.
		if !present["length"] && reg.Length == 0 {
			reg.Length = 1
		}
		if !present["scale"] && reg.Scale == 0 {
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

		// Two keys can spell the same address ("010105" and "10105"),
		// which ByKey happily keeps apart while ByAddr/Clusterize keep
		// the first and FindByMQTT-driven writes may land on the last —
		// the poll and write paths would disagree about which register
		// they are talking about. Keep the first definition.
		if isModbus {
			if first, dup := seenAddr[uint16(addr)]; dup {
				diagnostics = append(diagnostics,
					fmt.Sprintf("skip %q: duplicate Modbus address %d (already defined by %q)",
						key, addr, first))
				continue
			}
			seenAddr[uint16(addr)] = key
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
