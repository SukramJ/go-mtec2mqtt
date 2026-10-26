// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package hass builds the Home Assistant MQTT discovery document from the
// M-TEC register catalog and owns every topic this bridge derives.
//
// Each register that carries HA metadata in registers.yaml becomes one or
// more entities — sensors expose values, number/select/switch entities
// also send commands back through MQTT. They are rendered through
// github.com/SukramJ/go-hamqtt into ONE retained device document
// ([RenderBundle]). The package owns no I/O: the coordinator publishes.
package hass

import (
	"fmt"
	"strings"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// Manufacturer/model strings the inverter is announced under, and the
// unique_id namespace. Home Assistant has no migration path for an
// identity, so these are API.
const (
	manufacturer   = "M-TEC"
	model          = "Energy-Butler"
	deviceName     = "MTEC EnergyButler"
	uniqueIDPrefix = "MTEC_"
)

// Platform is the HA discovery platform of an entity.
type Platform string

// Platform values.
const (
	PlatformSensor       Platform = "sensor"
	PlatformBinarySensor Platform = "binary_sensor"
	PlatformNumber       Platform = "number"
	PlatformSelect       Platform = "select"
	PlatformSwitch       Platform = "switch"
)

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

// Discovery holds the inputs of the device document: the catalog, the
// language, the synthetic switches and — after [Discovery.Initialize] —
// the inverter's identity. Zero value is not usable; construct with [New].
type Discovery struct {
	mqttTopic string
	catalog   *registers.Map
	lang      string
	virtual   []VirtualSwitch

	// deviceName is the operator-chosen HA device name (empty = use the
	// generic deviceName constant). deviceSlug is its slugged form, folded
	// into the entity_id seed (default_entity_id) but never unique_id;
	// empty when no name is configured.
	deviceName string
	deviceSlug string

	serialNo      string
	firmware      string
	equipmentInfo string
	diagnostics   []string
	initialized   bool

	// serialInUniqueID scopes every unique_id by the inverter serial so
	// several daemon instances — one per inverter — can share a single HA
	// installation. Off by default: enabling it changes every unique_id,
	// which orphans the entities an existing installation already tracks,
	// so it is a deliberate operator opt-in via HASS_UNIQUE_ID_INCLUDE_SERIAL.
	serialInUniqueID bool
}

// New constructs a Discovery. mqttTopic is the instance name (the topic
// root), lang ("en"/"de") localises entity friendly names, virtual adds
// synthetic switch entities (may be nil). deviceName is the optional
// operator-chosen HA device name — when non-empty it replaces the generic
// device name and its slug is folded into every entity_id seed
// (default_entity_id; unique_id stays stable).
func New(mqttTopic string, catalog *registers.Map, lang string, virtual []VirtualSwitch, deviceName string) *Discovery {
	if lang == "" {
		lang = "en"
	}
	deviceName = strings.TrimSpace(deviceName)
	return &Discovery{
		mqttTopic:  mqttTopic,
		catalog:    catalog,
		lang:       lang,
		virtual:    virtual,
		deviceName: deviceName,
		deviceSlug: slugify(deviceName),
	}
}

// slugify reduces a free-text name to a lower-case [a-z0-9_] token usable
// inside an HA entity_id. Runs of non-alphanumeric characters collapse to a
// single underscore and leading/trailing underscores are trimmed. Returns ""
// for input that carries no usable characters.
//
// Deliberately not go-hamqtt's topic.Slug: the two disagree on the hyphen
// and on an input that reduces to nothing, and every installed entity_id
// was seeded by this one.
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

// IncludeSerialInUniqueIDs opts into serial-scoped unique_ids
// ("MTEC_<serial>_<key>" instead of "MTEC_<key>"). Must be called before
// [Discovery.Initialize].
func (d *Discovery) IncludeSerialInUniqueIDs(on bool) {
	d.serialInUniqueID = on
}

// IsInitialized reports whether [Discovery.Initialize] has been called.
func (d *Discovery) IsInitialized() bool { return d.initialized }

// Initialize stores the device-identifying values from the STATIC read
// and checks the catalog. Safe to call again.
func (d *Discovery) Initialize(serialNo, firmware, equipmentInfo string) {
	d.serialNo = serialNo
	d.firmware = firmware
	d.equipmentInfo = equipmentInfo
	d.diagnostics = d.diagnostics[:0]
	for _, r := range d.catalog.All {
		if r.Group == "" || !r.HasHassHints() || r.HassComponentType == "" {
			continue
		}
		switch Platform(r.HassComponentType) {
		case PlatformSensor, PlatformBinarySensor, PlatformNumber, PlatformSelect, PlatformSwitch:
		default:
			d.diagnostics = append(d.diagnostics, fmt.Sprintf(
				"skip %q: hass_component_type %q is not a platform this builder emits "+
					"(supported: binary_sensor, number, select, sensor, switch)",
				r.MQTT, r.HassComponentType,
			))
		}
	}
	d.initialized = true
}

// Diagnostics returns the catalog complaints [Discovery.Initialize]
// collected — registers whose hass_component_type names a platform this
// builder cannot emit, which would otherwise be polled and published with
// no entity and nothing in the log. Returned rather than logged because
// the package owns no I/O.
func (d *Discovery) Diagnostics() []string { return d.diagnostics }
