// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package registers models the M-TEC inverter's Modbus register catalog.
//
// A "register" here is either a real Modbus holding register (the YAML
// key is a numeric string like "10000") or a pseudo-register computed
// by the coordinator from other readings (the YAML key is a symbolic
// name like "consumption"). The two forms share the same metadata
// surface — group, unit, MQTT topic suffix, Home-Assistant hints —
// because they are published the same way over MQTT.
package registers

// DataType enumerates the Modbus-side encodings the M-TEC inverter
// exposes. The string values are the YAML literals (case-sensitive).
type DataType string

// DataType values used by the registers.yaml catalog.
const (
	DataU16  DataType = "U16"
	DataS16  DataType = "S16"
	DataI16  DataType = "I16" // alias of S16 in the legacy YAML
	DataU32  DataType = "U32"
	DataS32  DataType = "S32"
	DataI32  DataType = "I32" // alias of S32
	DataBYTE DataType = "BYTE"
	DataBIT  DataType = "BIT"
	DataDAT  DataType = "DAT"
	DataSTR  DataType = "STR"
)

// Group enumerates the MQTT publication buckets. The string values are
// embedded in MQTT topic paths, so they must stay in sync with what
// downstream consumers (Home Assistant dashboards, evcc) expect.
type Group string

// Group values used by the registers.yaml catalog.
const (
	GroupBase     Group = "now-base"
	GroupGrid     Group = "now-grid"
	GroupInverter Group = "now-inverter"
	GroupBackup   Group = "now-backup"
	GroupBattery  Group = "now-battery"
	GroupPV       Group = "now-pv"
	GroupDay      Group = "day"
	GroupTotal    Group = "total"
	GroupConfig   Group = "config"
	GroupStatic   Group = "static"
)

// Register describes one entry from registers.yaml. The schema is
// permissive — most fields are optional and only meaningful for
// certain combinations of Type and Group.
type Register struct {
	// Key is the literal YAML key. For Modbus registers it is the
	// decimal address as a string (e.g. "10000"); for pseudo-registers
	// it is a symbolic name (e.g. "consumption").
	Key string `yaml:"-"`

	// Address is the parsed Modbus address; zero for pseudo-registers.
	// Check IsModbus to disambiguate.
	Address uint16 `yaml:"-"`

	// Name is the human-readable label used as the MQTT field name in
	// the coordinator. Mandatory in the legacy YAML.
	Name string `yaml:"name"`

	// Length is the number of 16-bit Modbus registers this entry spans
	// (1 = single register, 2 = U32/S32/I32, N = STR/BYTE/BIT bundles).
	// Default 1 — applied in Load when missing.
	Length int `yaml:"length"`

	// Type selects the decoder; default U16 when empty.
	Type DataType `yaml:"type"`

	Unit  string `yaml:"unit"`
	Scale int    `yaml:"scale"`

	// MQTT is the topic-tail suffix the value is published under.
	MQTT string `yaml:"mqtt"`

	Group Group `yaml:"group"`

	Writable bool `yaml:"writable"`

	// Home-Assistant auto-discovery hints. Empty when the register is
	// not exposed to HA. Their presence in the YAML toggles whether
	// the integration emits a discovery payload.
	HassComponentType string         `yaml:"hass_component_type"`
	HassDeviceClass   string         `yaml:"hass_device_class"`
	HassStateClass    string         `yaml:"hass_state_class"`
	HassValueTemplate string         `yaml:"hass_value_template"`
	HassValueItems    map[int]string `yaml:"hass_value_items"`
	HassPayloadOn     string         `yaml:"hass_payload_on"`
	HassPayloadOff    string         `yaml:"hass_payload_off"`
}

// IsModbus reports whether the register represents a real Modbus
// holding register (true) or a calculated pseudo-register (false).
func (r *Register) IsModbus() bool { return r.Address != 0 || r.Key == "0" }

// HasHassHints reports whether the register carries enough HA-specific
// metadata to be published via Home Assistant auto-discovery.
//
// Mirrors the Python `hass_keys` check in hass_int._build_devices_array:
// any HA-related field present in the YAML opts the register in.
func (r *Register) HasHassHints() bool {
	return r.HassComponentType != "" ||
		r.HassDeviceClass != "" ||
		r.HassStateClass != "" ||
		r.HassValueTemplate != "" ||
		r.HassValueItems != nil ||
		r.HassPayloadOn != "" ||
		r.HassPayloadOff != ""
}

// Map is the parsed register catalog. Lookups by key and by Modbus
// address are O(1); All preserves the YAML order so callers that need
// deterministic iteration (HA discovery, debug dumps) get a stable
// sequence.
type Map struct {
	All    []*Register
	ByKey  map[string]*Register
	ByAddr map[uint16]*Register
	// Groups holds every distinct Group seen during Load, in
	// first-encounter order — matches the Python init_register_map.
	Groups []Group
}

// ByGroup returns all registers belonging to the given group, in YAML
// order. Returns nil (not an empty slice) when no register matches.
func (m *Map) ByGroup(g Group) []*Register {
	var out []*Register
	for _, r := range m.All {
		if r.Group == g {
			out = append(out, r)
		}
	}
	return out
}

// FindByMQTT returns the register whose MQTT suffix matches name, or
// nil. Used by the coordinator's write path to translate an incoming
// `MTEC/<serial>/<group>/<mqtt_key>/set` command back to a register.
func (m *Map) FindByMQTT(name string) *Register {
	for _, r := range m.All {
		if r.MQTT == name {
			return r
		}
	}
	return nil
}
