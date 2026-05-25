// SPDX-License-Identifier: MIT
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
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("registers: open %s: %w", path, err)
	}
	defer f.Close()
	return parse(f, path)
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

	// MappingNode.Content is a flat [key, value, key, value, ...] list.
	for i := 0; i+1 < len(top.Content); i += 2 {
		keyNode := top.Content[i]
		valNode := top.Content[i+1]
		key := keyNode.Value

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
		if addr, err := strconv.ParseUint(key, 10, 16); err == nil {
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
