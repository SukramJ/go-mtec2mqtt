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
	"strings"

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

// VirtualSwitch describes a synthetic HA switch entity that is not
// backed by its own Modbus register but instead toggles another
// register between a configured "on" value and 0. The coordinator owns
// the toggle/restore logic; this struct is the shared definition both
// the discovery builder and the coordinator read from, so the entity's
// identity (key, names, group) lives in one place.
type VirtualSwitch struct {
	// Key is the MQTT suffix and the stable unique_id seed — never localised.
	// (The entity_id is seeded from the English Name, like real registers.)
	Key string
	// Name / NameDE are the friendly labels (English / German).
	Name   string
	NameDE string
	// Group is the publication bucket, e.g. "config".
	Group string
	// TargetMQTT is the writable register this switch drives.
	TargetMQTT string
	// OnValue is the value written to the target when switched on with no
	// previously cached value.
	OnValue int
}

// LocalizedName returns the switch's friendly name in lang, falling
// back to the English Name.
func (v VirtualSwitch) LocalizedName(lang string) string {
	if lang == "de" && v.NameDE != "" {
		return v.NameDE
	}
	return v.Name
}

// DefaultVirtualSwitches returns the built-in "charge active" /
// "discharge active" switches wired to the charge/discharge limit
// registers, using the supplied on-values.
func DefaultVirtualSwitches(chargeOnValue, dischargeOnValue int) []VirtualSwitch {
	return []VirtualSwitch{
		{
			Key:        "charge_active",
			Name:       "Charge active",
			NameDE:     "Laden aktiv",
			Group:      "config",
			TargetMQTT: "charge_limit",
			OnValue:    chargeOnValue,
		},
		{
			Key:        "discharge_active",
			Name:       "Discharge active",
			NameDE:     "Entladen aktiv",
			Group:      "config",
			TargetMQTT: "discharge_limit",
			OnValue:    dischargeOnValue,
		},
	}
}

// Discovery walks the register catalog and emits HA discovery entries.
// Zero-value is not usable — construct with [New], then call
// [Discovery.Initialize] once the inverter's serial number and
// firmware are known (those come from the STATIC register read).
type Discovery struct {
	hassBaseTopic string
	mqttTopic     string
	catalog       *registers.Map
	lang          string
	virtual       []VirtualSwitch

	// deviceName is the operator-chosen HA device name (empty = use the
	// generic deviceName constant). deviceSlug is its slugged form, folded
	// into the entity_id seed (object_id/default_entity_id) but never
	// unique_id; empty when no name is configured, preserving the identity.
	deviceName string
	deviceSlug string

	serialNo      string
	firmware      string
	equipmentInfo string
	device        map[string]any
	entries       []Entry
	initialized   bool

	// serialInUniqueID scopes every unique_id (and thus every discovery
	// config topic) by the inverter serial so several daemon instances —
	// one per inverter — can share a single HA installation without
	// overwriting each other's retained discovery configs. Off by
	// default: enabling it changes every unique_id, which orphans the
	// entities an existing installation already tracks (MQTT discovery
	// has no unique_id migration), so it is a deliberate operator opt-in
	// via HASS_UNIQUE_ID_INCLUDE_SERIAL.
	serialInUniqueID bool
}

// New constructs a Discovery for the given topic roots and catalog.
// hassBaseTopic is usually "homeassistant"; mqttTopic is the MTEC
// publish root (e.g. "MTEC"). lang ("en"/"de") localises entity friendly
// names; virtual adds synthetic switch entities (may be nil). deviceName
// is the optional operator-chosen HA device name — when non-empty it
// replaces the generic device name and its slug is folded into every
// entity_id seed (object_id/default_entity_id; unique_id stays stable);
// pass "" to keep the previous generic identity.
func New(hassBaseTopic, mqttTopic string, catalog *registers.Map, lang string, virtual []VirtualSwitch, deviceName string) *Discovery {
	if lang == "" {
		lang = "en"
	}
	deviceName = strings.TrimSpace(deviceName)
	return &Discovery{
		hassBaseTopic: hassBaseTopic,
		mqttTopic:     mqttTopic,
		catalog:       catalog,
		lang:          lang,
		virtual:       virtual,
		deviceName:    deviceName,
		deviceSlug:    slugify(deviceName),
	}
}

// slugify reduces a free-text device name to a lower-case
// [a-z0-9_] token usable inside an HA entity_id. Runs of non-alphanumeric
// characters collapse to a single underscore and leading/trailing
// underscores are trimmed. Returns "" for input that carries no usable
// characters, so callers fall back to the generic identity.
func slugify(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if b.Len() > 0 && !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// entityIDBase returns the language-independent object part of the
// entity_id, derived from a register's ENGLISH name (never the localised
// display name): slug(name), or "<device>_<slug(name)>" when a device
// name is configured. Seeding from the English name — rather than the
// short mqtt topic key — mirrors the Python aiomtec2mqtt reference, which
// sets no object_id and lets HA build the entity_id from the name, so the
// Go port stays a drop-in replacement (same entity_ids). Pinning to the
// English name keeps the id stable across LANGUAGE; only the display
// "name" carries translations. Example: "Grid power phase A" (device
// "MrBurns") → "mrburns_grid_power_phase_a".
func (d *Discovery) entityIDBase(englishName string) string {
	seed := slugify(englishName)
	if d.deviceSlug == "" {
		return seed
	}
	return d.deviceSlug + "_" + seed
}

// objectID is the bare seed HA has historically used to generate the
// entity_id. HA Core deprecated it in 2025.10 (in favour of
// default_entity_id) and removes it in 2026.4, but current releases still
// apply it reliably — whereas default_entity_id is not yet consistently
// honoured (home-assistant/core#157241, where a localised name otherwise
// leaks into the entity_id). We therefore publish both (see
// [Discovery.defaultEntityID]); object_id keeps today's HA correct,
// default_entity_id keeps future HA correct.
func (d *Discovery) objectID(englishName string) string {
	return d.entityIDBase(englishName)
}

// defaultEntityID builds the HA "default_entity_id" discovery option:
// "<domain>.<id>", where <domain> is the platform (sensor, number, …) and
// <id> is [Discovery.entityIDBase]. It is the forward-looking replacement
// for object_id (removed in HA Core 2026.4) and is published alongside it.
// Like object_id it only seeds the initial entity_id: HA tracks entities
// by unique_id, so it never renames an entity that already exists —
// enabling a device name later gives new entities the nicer id without
// disturbing established ones.
func (d *Discovery) defaultEntityID(p Platform, englishName string) string {
	return string(p) + "." + d.entityIDBase(englishName)
}

// IncludeSerialInUniqueIDs opts into serial-scoped unique_ids
// ("MTEC_<serial>_<key>" instead of "MTEC_<key>"). Must be called
// before [Discovery.Initialize]; the serial itself only becomes part of
// the ids once Initialize has supplied it.
func (d *Discovery) IncludeSerialInUniqueIDs(on bool) {
	d.serialInUniqueID = on
}

// uniqueID returns the entity unique_id: the "MTEC_" namespace prefix
// plus the entity key. It deliberately never folds in the device slug —
// unique_id is the identity Home Assistant tracks an entity by, so
// changing it would orphan the old entity and drop all its history,
// customisations and dashboard/automation references (MQTT discovery has
// no clean unique_id migration). A configured device name therefore only
// affects the display name and entity_id seed, never this value. The
// inverter serial is folded in only under the explicit
// HASS_UNIQUE_ID_INCLUDE_SERIAL opt-in (see [Discovery.IncludeSerialInUniqueIDs]).
func (d *Discovery) uniqueID(key string) string {
	if d.serialInUniqueID && d.serialNo != "" {
		return uniqueIDPrefix + d.serialNo + "_" + key
	}
	return uniqueIDPrefix + key
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
	// The HA device name is the operator-chosen one when configured,
	// otherwise the generic constant. Identifiers/serial_number stay
	// keyed on the serial so the device registry entry is stable
	// regardless of any display-name change.
	name := deviceName
	if d.deviceName != "" {
		name = d.deviceName
	}
	d.device = map[string]any{
		"identifiers":   []string{serialNo},
		"manufacturer":  manufacturer,
		"model":         model,
		"model_id":      equipmentInfo,
		"name":          name,
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

// ConfigFilter is the MQTT topic filter matching this daemon's discovery
// config topics (e.g. "homeassistant/+/+/config", one '+' for the platform
// and one for the unique_id). Orphan reconciliation subscribes to it to
// collect the retained configs the broker replays, then compares them
// against the freshly published set.
func (d *Discovery) ConfigFilter() string {
	return d.hassBaseTopic + "/+/+/config"
}

// IsOwnConfig reports whether a retained HA discovery config payload was
// published by this daemon: its unique_id sits in our "MTEC_" namespace and
// its state_topic (when present) is under our MQTT publish root. Orphan
// cleanup uses this as a guard so it never clears the discovery configs of
// another integration that happens to share the discovery prefix.
//
// With serial-scoped unique_ids enabled the state_topic must additionally
// sit under this inverter's serial ("<root>/<serial>/…") — every entity this
// daemon ever published carries that prefix (in the legacy and the scoped
// id format alike), while a sibling instance's entities carry a different
// serial and must never be treated as ours.
func (d *Discovery) IsOwnConfig(payload []byte) bool {
	var cfg struct {
		UniqueID   string `json:"unique_id"`
		StateTopic string `json:"state_topic"`
	}
	if json.Unmarshal(payload, &cfg) != nil {
		return false
	}
	if !strings.HasPrefix(cfg.UniqueID, uniqueIDPrefix) {
		return false
	}
	root := d.mqttTopic + "/"
	if d.serialInUniqueID && d.serialNo != "" {
		root += d.serialNo + "/"
		return strings.HasPrefix(cfg.StateTopic, root)
	}
	return cfg.StateTopic == "" || strings.HasPrefix(cfg.StateTopic, root)
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
		case PlatformButton:
			// Button has no read path and no builder yet — registers
			// declaring it are intentionally not published as entities.
		}
	}
	// Synthetic switches come after the catalog so their output stays
	// deterministic and grouped at the tail.
	for _, v := range d.virtual {
		d.appendVirtualSwitch(v)
	}
}

// appendVirtualSwitch emits a switch entity for a [VirtualSwitch]. Its
// state/command topics live under the configured group keyed by the
// stable Key (also the entity_id seed), and its friendly name is localised.
// payload_on/off are "1"/"0" to match the coordinator-published state.
func (d *Discovery) appendVirtualSwitch(v VirtualSwitch) {
	uid := d.uniqueID(v.Key)
	command := fmt.Sprintf("%s/%s/%s/%s/set", d.mqttTopic, d.serialNo, v.Group, v.Key)
	payload := map[string]any{
		"command_topic":      command,
		"device":             d.device,
		"enabled_by_default": true,
		"default_entity_id":  d.defaultEntityID(PlatformSwitch, v.Name),
		"name":               v.LocalizedName(d.lang),
		"object_id":          d.objectID(v.Name),
		"payload_off":        "0",
		"payload_on":         "1",
		"state_topic":        fmt.Sprintf("%s/%s/%s/%s/state", d.mqttTopic, d.serialNo, v.Group, v.Key),
		"unique_id":          uid,
	}
	d.appendEntry(PlatformSwitch, uid, payload, command)
}

// --- per-platform builders --------------------------------------------------

func (d *Discovery) appendSensor(r *registers.Register) {
	uid := d.uniqueID(r.MQTT)
	payload := map[string]any{
		"default_entity_id":   d.defaultEntityID(PlatformSensor, r.Name),
		"device":              d.device,
		"enabled_by_default":  true,
		"name":                r.LocalizedName(d.lang),
		"object_id":           d.objectID(r.Name),
		"state_topic":         d.stateTopic(r),
		"unique_id":           uid,
		"unit_of_measurement": r.Unit,
	}
	if r.HassDeviceClass != "" {
		payload["device_class"] = r.HassDeviceClass
	}
	// HA requires the full state-value list for enum sensors and rejects
	// the entity without it. The coordinator publishes the localized
	// labels plus "Unknown" for unmapped codes, so the option list is
	// exactly that set. options excludes unit_of_measurement, so the
	// (always empty for enums) unit key is dropped alongside.
	if r.HassDeviceClass == "enum" && len(r.HassValueItems) > 0 {
		payload["options"] = append(valueItemsValues(r.LocalizedValueItems(d.lang)), "Unknown")
		if r.Unit == "" {
			delete(payload, "unit_of_measurement")
		}
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
	uid := d.uniqueID(r.MQTT)
	payload := map[string]any{
		"default_entity_id":  d.defaultEntityID(PlatformBinarySensor, r.Name),
		"device":             d.device,
		"enabled_by_default": true,
		"name":               r.LocalizedName(d.lang),
		"object_id":          d.objectID(r.Name),
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
	uid := d.uniqueID(r.MQTT)
	command := d.commandTopic(r)
	payload := map[string]any{
		"command_topic":       command,
		"device":              d.device,
		"enabled_by_default":  false,
		"mode":                "box",
		"name":                r.LocalizedName(d.lang),
		"default_entity_id":   d.defaultEntityID(PlatformNumber, r.Name),
		"object_id":           d.objectID(r.Name),
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
	uid := d.uniqueID(r.MQTT)
	command := d.commandTopic(r)
	payload := map[string]any{
		"command_topic":      command,
		"device":             d.device,
		"enabled_by_default": false,
		"default_entity_id":  d.defaultEntityID(PlatformSelect, r.Name),
		"name":               r.LocalizedName(d.lang),
		"object_id":          d.objectID(r.Name),
		"options":            valueItemsValues(r.LocalizedValueItems(d.lang)),
		"state_topic":        d.stateTopic(r),
		"unique_id":          uid,
	}
	d.appendEntry(PlatformSelect, uid, payload, command)
}

func (d *Discovery) appendSwitch(r *registers.Register) {
	uid := d.uniqueID(r.MQTT)
	command := d.commandTopic(r)
	payload := map[string]any{
		"command_topic":      command,
		"device":             d.device,
		"enabled_by_default": false,
		"default_entity_id":  d.defaultEntityID(PlatformSwitch, r.Name),
		"name":               r.LocalizedName(d.lang),
		"object_id":          d.objectID(r.Name),
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
