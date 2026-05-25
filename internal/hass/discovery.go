// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package hass builds Home Assistant MQTT auto-discovery payloads
// from the M-TEC register catalog.
//
// Discovery turns each Modbus register that carries HA-specific
// metadata in registers.yaml into one or more entity definitions —
// sensors expose values, number/select/switch entities also send
// commands back through MQTT. The package owns no I/O: it produces a
// slice of [Entry] values that the coordinator publishes via its MQTT
// client and (for the writable entities) subscribes to.
package hass

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// Manufacturer/model strings the inverter is announced under. These
// match aiomtec2mqtt so an upgrading user's existing entities keep
// the same provenance in HA's device registry.
const (
	manufacturer   = "M-TEC"
	model          = "Energy-Butler"
	deviceName     = "MTEC EnergyButler"
	uniqueIDPrefix = "MTEC_"
)

// Platform is the HA discovery platform — used as the second path
// segment of the discovery topic and as a switch in the builder.
type Platform string

// Platform values.
const (
	PlatformSensor       Platform = "sensor"
	PlatformBinarySensor Platform = "binary_sensor"
	PlatformNumber       Platform = "number"
	PlatformSelect       Platform = "select"
	PlatformSwitch       Platform = "switch"
	PlatformButton       Platform = "button"
)

// Entry is one discovery payload ready for MQTT publication. The
// coordinator publishes Payload to ConfigTopic (retained), and if
// CommandTopic is non-empty also subscribes to it to handle inbound
// writes from HA.
type Entry struct {
	ConfigTopic  string
	Payload      []byte
	CommandTopic string
}

// Discovery walks the register catalog and emits HA discovery entries.
// Zero-value is not usable — construct with [New], then call
// [Discovery.Initialize] once the inverter's serial number and
// firmware are known (those come from the STATIC register read).
type Discovery struct {
	hassBaseTopic string
	mqttTopic     string
	catalog       *registers.Map

	serialNo      string
	firmware      string
	equipmentInfo string
	device        map[string]any
	entries       []Entry
	initialized   bool
}

// New constructs a Discovery for the given topic roots and catalog.
// hassBaseTopic is usually "homeassistant"; mqttTopic is the MTEC
// publish root (e.g. "MTEC").
func New(hassBaseTopic, mqttTopic string, catalog *registers.Map) *Discovery {
	return &Discovery{
		hassBaseTopic: hassBaseTopic,
		mqttTopic:     mqttTopic,
		catalog:       catalog,
	}
}

// IsInitialized reports whether [Initialize] has been called.
func (d *Discovery) IsInitialized() bool { return d.initialized }

// Initialize stores the device-identifying values and builds the
// entry list. Safe to call again on reconnect — the entry list is
// rebuilt from scratch so any catalog change since the last call
// is reflected.
func (d *Discovery) Initialize(serialNo, firmware, equipmentInfo string) {
	d.serialNo = serialNo
	d.firmware = firmware
	d.equipmentInfo = equipmentInfo
	d.device = map[string]any{
		"identifiers":   []string{serialNo},
		"manufacturer":  manufacturer,
		"model":         model,
		"model_id":      equipmentInfo,
		"name":          deviceName,
		"serial_number": serialNo,
		"sw_version":    firmware,
	}
	d.entries = d.entries[:0]
	d.buildEntries()
	d.initialized = true
}

// Entries returns the discovery payloads built by [Initialize]. The
// slice is shared — callers should iterate, not mutate.
func (d *Discovery) Entries() []Entry { return d.entries }

// UnregisterEntries returns entries that publish an empty payload to
// the same config topics, which tells HA to forget every advertised
// entity. Useful on a clean shutdown when the daemon goes away.
func (d *Discovery) UnregisterEntries() []Entry {
	out := make([]Entry, len(d.entries))
	for i, e := range d.entries {
		out[i] = Entry{ConfigTopic: e.ConfigTopic, Payload: []byte("")}
	}
	return out
}

// buildEntries iterates the catalog and dispatches each
// HA-annotated register to its platform-specific builder. Iteration
// follows YAML order (registers.Map.All) so the output is
// deterministic across runs.
func (d *Discovery) buildEntries() {
	for _, r := range d.catalog.All {
		if r.Group == "" || !r.HasHassHints() {
			continue
		}
		// Default platform is sensor — matches the Python fallback
		// when hass_component_type is omitted.
		platform := PlatformSensor
		if r.HassComponentType != "" {
			platform = Platform(r.HassComponentType)
		}
		switch platform {
		case PlatformSensor:
			d.appendSensor(r)
		case PlatformBinarySensor:
			d.appendBinarySensor(r)
		case PlatformNumber:
			d.appendNumber(r)
			d.appendSensor(r) // also publish a read-only sensor view
		case PlatformSelect:
			d.appendSelect(r)
			d.appendSensor(r)
		case PlatformSwitch:
			d.appendSwitch(r)
			d.appendBinarySensor(r)
		}
	}
}

// --- per-platform builders --------------------------------------------------

func (d *Discovery) appendSensor(r *registers.Register) {
	uid := uniqueIDPrefix + r.MQTT
	payload := map[string]any{
		"device":              d.device,
		"enabled_by_default":  true,
		"name":                r.Name,
		"state_topic":         d.stateTopic(r),
		"unique_id":           uid,
		"unit_of_measurement": r.Unit,
	}
	if r.HassDeviceClass != "" {
		payload["device_class"] = r.HassDeviceClass
	}
	if r.HassValueTemplate != "" {
		payload["value_template"] = r.HassValueTemplate
	}
	if r.HassStateClass != "" {
		payload["state_class"] = r.HassStateClass
	}
	d.appendEntry(PlatformSensor, uid, payload, "")
}

func (d *Discovery) appendBinarySensor(r *registers.Register) {
	uid := uniqueIDPrefix + r.MQTT
	payload := map[string]any{
		"device":             d.device,
		"enabled_by_default": true,
		"name":               r.Name,
		"state_topic":        d.stateTopic(r),
		"unique_id":          uid,
	}
	if r.HassDeviceClass != "" {
		payload["device_class"] = r.HassDeviceClass
	}
	if r.HassPayloadOn != "" {
		payload["payload_on"] = r.HassPayloadOn
	}
	if r.HassPayloadOff != "" {
		payload["payload_off"] = r.HassPayloadOff
	}
	d.appendEntry(PlatformBinarySensor, uid, payload, "")
}

func (d *Discovery) appendNumber(r *registers.Register) {
	uid := uniqueIDPrefix + r.MQTT
	command := d.commandTopic(r)
	payload := map[string]any{
		"command_topic":       command,
		"device":              d.device,
		"enabled_by_default":  false,
		"mode":                "box",
		"name":                r.Name,
		"state_topic":         d.stateTopic(r),
		"unique_id":           uid,
		"unit_of_measurement": r.Unit,
	}
	if r.HassDeviceClass != "" {
		payload["device_class"] = r.HassDeviceClass
	}
	d.appendEntry(PlatformNumber, uid, payload, command)
}

func (d *Discovery) appendSelect(r *registers.Register) {
	uid := uniqueIDPrefix + r.MQTT
	command := d.commandTopic(r)
	payload := map[string]any{
		"command_topic":      command,
		"device":             d.device,
		"enabled_by_default": false,
		"name":               r.Name,
		"options":            valueItemsValues(r.HassValueItems),
		"state_topic":        d.stateTopic(r),
		"unique_id":          uid,
	}
	d.appendEntry(PlatformSelect, uid, payload, command)
}

func (d *Discovery) appendSwitch(r *registers.Register) {
	uid := uniqueIDPrefix + r.MQTT
	command := d.commandTopic(r)
	payload := map[string]any{
		"command_topic":      command,
		"device":             d.device,
		"enabled_by_default": false,
		"name":               r.Name,
		"state_topic":        d.stateTopic(r),
		"unique_id":          uid,
	}
	if r.HassDeviceClass != "" {
		payload["device_class"] = r.HassDeviceClass
	}
	if r.HassPayloadOn != "" {
		payload["payload_on"] = r.HassPayloadOn
	}
	if r.HassPayloadOff != "" {
		payload["payload_off"] = r.HassPayloadOff
	}
	d.appendEntry(PlatformSwitch, uid, payload, command)
}

// appendEntry marshals payload and pushes a new Entry. Marshal errors
// would only happen for non-JSON-serialisable values that the static
// catalog cannot produce — we surface them as a panic to flag a
// programming mistake during development rather than silently
// dropping the entity at runtime.
func (d *Discovery) appendEntry(platform Platform, uid string, payload map[string]any, commandTopic string) {
	bs, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("hass: marshal %s payload for %s: %v", platform, uid, err))
	}
	d.entries = append(d.entries, Entry{
		ConfigTopic:  d.configTopic(platform, uid),
		Payload:      bs,
		CommandTopic: commandTopic,
	})
}

// configTopic returns "<hass_base>/<platform>/<unique_id>/config" —
// HA's auto-discovery topic.
func (d *Discovery) configTopic(p Platform, uniqueID string) string {
	return fmt.Sprintf("%s/%s/%s/config", d.hassBaseTopic, p, uniqueID)
}

// stateTopic returns the topic the coordinator publishes the
// register's current value to: "<mqtt_topic>/<serial>/<group>/<mqtt_key>/state".
func (d *Discovery) stateTopic(r *registers.Register) string {
	return fmt.Sprintf("%s/%s/%s/%s/state",
		d.mqttTopic, d.serialNo, r.Group, r.MQTT)
}

// commandTopic returns the topic HA writes back to for writable
// entities: "<mqtt_topic>/<serial>/<group>/<mqtt_key>/set".
func (d *Discovery) commandTopic(r *registers.Register) string {
	return fmt.Sprintf("%s/%s/%s/%s/set",
		d.mqttTopic, d.serialNo, r.Group, r.MQTT)
}

// valueItemsValues returns the HA select options derived from
// hass_value_items. The map iteration order in Go is randomised; we
// sort by the integer code (the Modbus side) so the option list is
// stable across builds — important for snapshot tests and for
// debouncing config-topic republishes.
func valueItemsValues(items map[int]string) []string {
	if len(items) == 0 {
		return []string{}
	}
	codes := make([]int, 0, len(items))
	for c := range items {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = items[c]
	}
	return out
}
