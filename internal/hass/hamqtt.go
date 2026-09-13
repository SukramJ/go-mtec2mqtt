// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
	"sort"
	"strconv"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	"github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// This file is ADR 0070 phase 6, step 3: a parallel rendering path that
// produces the SAME discovery payloads as [Discovery] above, through
// github.com/SukramJ/go-hamqtt's model instead of this package's
// hand-built map[string]any.
//
// Nothing here is wired into the publish path, the coordinator or the MQTT
// bootstrap, and nothing here reaches a broker. It exists so the question
// "can the shared library reproduce this bridge's published bytes?" is
// answered by a test against the pinned goldens (hamqtt_test.go) rather
// than by reading the library. Step 4 onwards switches the real path over,
// one plane at a time, with that test behind it.
//
// The three identity strings are all preserved by overriding the library's
// defaults, because Home Assistant has no migration path for any of them:
//
//   - unique_id          — [RenderContext.UniqueID], "MTEC_<mqtt key>"
//   - default_entity_id  — [RenderContext.ObjectID], slugify(ENGLISH name)
//   - device.identifiers — model.Identifier{Namespace: "", Value: serial},
//     the bare serial with no namespace at all
//
// and the topics by an own [Layout] rather than topic.Default: the latter
// renders a model.Bucket segment whose vocabulary is paramset names
// (values/master/calculated/custom) while this bridge's level is a poll
// group (now-base/day/config/…). The two do not mean the same thing, so
// the group rides in Slot.Path and Bucket stays unset. This is the second
// consumer to record that Bucket does not fit; see
// notes/adr0070-phase6-measurement.md §5.2.

// LegacyConfigTopicForm names the [publisher.LegacyTopicFunc] that
// reproduces this bridge's retained per-entity config topics, and is
// therefore the one a later step must set in
// publisher.Config.LegacyEntityTopics.
//
// It is publisher.LegacyTopicByUniqueID — the FOUR-segment form
// "<prefix>/<platform>/<unique_id>/config" — and not the five-segment
// publisher.LegacyTopicWithNodeID that a consumer gets by saying nothing.
// Verified against the pinned topics rather than assumed: every one of the
// 100 config topics in internal/hass/testdata/discovery_en.json has four
// levels and its third level is the payload's unique_id, with no node-id
// level anywhere. TestLegacyTopicFormMatchesThePinnedTopics asserts
// exactly that, by rendering both forms over the real catalog and
// comparing them to the pins.
//
// Getting it wrong is silent and total: the five-segment default would
// retract none of the 100 retained configs, the device bundle would be
// published while they were still retained, and Home Assistant refuses
// that with a single "WARNING [mqtt.entity] Received a conflicting MQTT
// discovery message" — the entities simply would not appear.
//
// It is a documented constant rather than a wired-up setting because
// nothing publishes a bundle yet; step 6 is what consumes it.
const LegacyConfigTopicForm = "publisher.LegacyTopicByUniqueID"

// Layout renders this bridge's topic schema for the go-hamqtt hamodel.
//
// Root is the MQTT publish root (config.MQTTTopic, "MTEC" by default),
// never the Home Assistant discovery prefix — the two are separate roots
// in this bridge and the identity namespace is a third, constant one.
type Layout struct {
	Root string
}

var _ topic.Layout = Layout{}

// State implements [topic.Layout]: "<root>/<serial>/<group>/<key>/state".
func (l Layout) State(s hamodel.Slot) string {
	return topic.Join(append(l.base(s), "state")...)
}

// Command implements [topic.Layout]: "<root>/<serial>/<group>/<key>/set".
func (l Layout) Command(s hamodel.Slot) string {
	return topic.Join(append(l.base(s), "set")...)
}

// Availability implements [topic.Layout], the device-level reachability
// topic: "<root>/<serial>/availability".
//
// Nothing publishes it and nothing references it: every entity this bridge
// renders declares hamodel.BridgeOnly(), so hamodel.LevelDevice never
// resolves. It is spelled out anyway because the interface requires it and
// because an entity that acquired LevelDevice by accident would otherwise
// render an empty topic — which Home Assistant greys out forever with
// nothing in the log to say why.
func (l Layout) Availability(s hamodel.Slot) string {
	return topic.Join(l.Root, s.Address, "availability")
}

// Bridge implements [topic.Layout]: the daemon's own status topic, the
// same string [BridgeStatusTopic] produces for the publishing side.
func (l Layout) Bridge() string { return BridgeStatusTopic(l.Root) }

func (l Layout) base(s hamodel.Slot) []string {
	parts := make([]string, 0, len(s.Path)+2)
	parts = append(parts, l.Root, s.Address)
	return append(parts, s.Path...)
}

// Entity is one rendered entity: a [hamodel.Basic] plus the two seeds this
// bridge's identity strings are derived from.
//
// They are fields rather than derivations of Key() because the two are
// derived from two DIFFERENT catalog columns — unique_id from the short
// mqtt key, default_entity_id from the English display name — which is
// this bridge's own peculiarity among the six consumers and the reason
// [RenderContext] overrides both.
type Entity struct {
	hamodel.Basic

	// UniqueIDSeed is the register's mqtt key. It is NOT Key(), because
	// nine mqtt keys are published twice under two platforms each and a
	// bundle refuses duplicate component keys.
	UniqueIDSeed string
	// EntityIDSeed is the register's ENGLISH name, never the localised
	// one: the entity id must not move when LANGUAGE changes.
	EntityIDSeed string
	// PlatformFields carries the platform-specific keys that have no
	// typed home on discovery.Component — mode, payload_on, payload_off.
	PlatformFields any
}

// BuildDiscovery implements discovery.Builder, attaching [PlatformFields].
func (e *Entity) BuildDiscovery(_ discovery.Context, comp *discovery.Component) error {
	if e.PlatformFields != nil {
		comp.Fields = e.PlatformFields
	}
	return nil
}

// RenderContext is [discovery.StdContext] with this bridge's three
// identity strings pinned to the spellings its installed fleet already
// carries.
type RenderContext struct {
	discovery.StdContext

	// DeviceSlug is slugify(DEVICE_NAME), folded into every entity-id
	// seed. Empty keeps the generic identity.
	DeviceSlug string
	// SerialNo is folded into unique_id only under the
	// HASS_UNIQUE_ID_INCLUDE_SERIAL opt-in.
	SerialNo string
	// SerialInUniqueID mirrors Discovery.serialInUniqueID.
	SerialInUniqueID bool
}

var _ discovery.Context = RenderContext{}

// UniqueID implements discovery.Context with this bridge's namespace:
// "MTEC_<mqtt key>", or "MTEC_<serial>_<mqtt key>" under the opt-in.
//
// The namespace is the compile-time uniqueIDPrefix constant and never the
// configurable MQTT root, so an operator who changes MQTT_TOPIC keeps
// every entity's history.
func (c RenderContext) UniqueID(_ *hamodel.Device, e hamodel.Entity) string {
	ent, ok := e.(*Entity)
	if !ok {
		return c.StdContext.UniqueID(nil, e)
	}
	if c.SerialInUniqueID && c.SerialNo != "" {
		return uniqueIDPrefix + c.SerialNo + "_" + ent.UniqueIDSeed
	}
	return uniqueIDPrefix + ent.UniqueIDSeed
}

// ObjectID implements discovery.Context, returning the entity-id seed the
// library prefixes with "<platform>.": slugify of the ENGLISH register
// name, with the device slug folded in when one is configured.
//
// Deliberately this package's slugify and not topic.Slug: the two
// disagree on the hyphen (8 of 100 seeds) and on an input that reduces to
// nothing (all 100). See notes/adr0070-phase6-measurement.md §5.3.
func (c RenderContext) ObjectID(_ *hamodel.Device, e hamodel.Entity) string {
	ent, ok := e.(*Entity)
	if !ok {
		return ""
	}
	seed := slugify(ent.EntityIDSeed)
	if c.DeviceSlug == "" {
		return seed
	}
	return c.DeviceSlug + "_" + seed
}

// NewRenderContext builds the context for a [Discovery] that has already
// been initialised, so the parallel path renders from exactly the same
// inputs as the shipped builder.
func NewRenderContext(d *Discovery) RenderContext {
	return RenderContext{
		StdContext: discovery.StdContext{
			Layout: Layout{Root: d.mqttTopic},
			Lang:   d.lang,
			// This bridge publishes a bare scalar, not an envelope, so a
			// value template is whatever the catalog states and nothing
			// more.
			Enc: discovery.RawEncoding,
		},
		DeviceSlug:       d.deviceSlug,
		SerialNo:         d.serialNo,
		SerialInUniqueID: d.serialInUniqueID,
	}
}

// NewDevice builds the one flat Home Assistant device this daemon owns.
//
// The identifier carries NO namespace: Home Assistant keys its device
// registry on the rendered string, and this fleet is registered under the
// bare serial. A namespaced identifier would leave the old device behind
// with its area and its name override while the entities moved to a new
// one.
func NewDevice(d *Discovery) *hamodel.Device {
	name := deviceName
	if d.deviceName != "" {
		name = d.deviceName
	}
	return &hamodel.Device{
		Identity: hamodel.Identity{
			IDs: []hamodel.Identifier{{Namespace: "", Value: d.serialNo}},
		},
		Name:         hamodel.L(name),
		Manufacturer: manufacturer,
		Model:        model,
		ModelID:      d.equipmentInfo,
		SerialNumber: d.serialNo,
		SWVersion:    d.firmware,
	}
}

// NewEntities builds the model entities for an initialised [Discovery],
// in the same order [Discovery.Entries] emits them.
//
// The dual emission is reproduced, not resolved: a writable register
// yields both its control and a read-only sensor view, so nine unique_ids
// appear twice under two platforms each. Whether a device bundle may carry
// that is unmeasured and needs a live Home Assistant; see
// notes/adr0070-phase6-measurement.md §3.3. Component keys are
// "<platform>.<mqtt key>" precisely so the duplication reaches the bundle
// as two components rather than being swallowed by discovery.Render's
// duplicate-key check.
func NewEntities(d *Discovery) []hamodel.Entity {
	out := make([]hamodel.Entity, 0, len(d.entries))
	for _, r := range d.catalog.All {
		if r.Group == "" || !r.HasHassHints() {
			continue
		}
		platform := PlatformSensor
		if r.HassComponentType != "" {
			platform = Platform(r.HassComponentType)
		}
		switch platform {
		case PlatformSensor:
			out = append(out, sensorEntity(d.serialNo, r))
		case PlatformBinarySensor:
			out = append(out, binarySensorEntity(d.serialNo, r))
		case PlatformNumber:
			out = append(out, numberEntity(d.serialNo, r), sensorEntity(d.serialNo, r))
		case PlatformSelect:
			out = append(out, selectEntity(d.serialNo, r), sensorEntity(d.serialNo, r))
		case PlatformSwitch:
			out = append(out, switchEntity(d.serialNo, r), binarySensorEntity(d.serialNo, r))
		default:
			// Unsupported hass_component_type: the shipped builder emits a
			// diagnostic and no entity, and so does this path.
		}
	}
	for _, v := range d.virtual {
		out = append(out, virtualSwitchEntity(d.serialNo, v))
	}
	return out
}

// Render renders the per-entity discovery bodies for an initialised
// [Discovery] through go-hamqtt, keyed by config topic.
//
// Origin is deliberately empty: the shipped payloads carry no `origin`
// block, and adding one would be a payload diff that is not this step.
// [RenderBundle] is where an origin belongs, because Home Assistant
// requires one on a device bundle.
func Render(d *Discovery) (map[string][]byte, error) {
	ctx := NewRenderContext(d)
	dev := NewDevice(d)
	out := make(map[string][]byte, len(d.entries))
	for _, e := range NewEntities(d) {
		comp, err := discovery.RenderComponent(ctx, dev, e, discovery.Origin{})
		if err != nil {
			return nil, fmt.Errorf("hass: render %q: %w", e.Key(), err)
		}
		body, err := comp.EntityJSON()
		if err != nil {
			return nil, fmt.Errorf("hass: encode %q: %w", e.Key(), err)
		}
		cfgTopic := d.configTopic(Platform(comp.Platform), comp.UniqueID)
		if _, dup := out[cfgTopic]; dup {
			return nil, fmt.Errorf("hass: duplicate config topic %q", cfgTopic)
		}
		out[cfgTopic] = body
	}
	return out, nil
}

// RenderBundle renders the same entities as one device bundle — the shape
// step 6 would publish. Nothing publishes it; it exists so
// discovery.Validate has a bundle to check and so the duplicate-unique_id
// question can be asked of the library in code.
func RenderBundle(d *Discovery, origin discovery.Origin) (*discovery.Bundle, error) {
	return discovery.Render(NewRenderContext(d), NewDevice(d), NewEntities(d), origin)
}

// --- per-platform entity builders -------------------------------------------
//
// Each mirrors the map[string]any its appendX counterpart above builds,
// key for key. The pairing is asserted rather than trusted: hamqtt_test.go
// compares the rendered bytes against the pinned goldens.

func slot(serial string, r *registers.Register) hamodel.Slot {
	return hamodel.Slot{Address: serial, Path: []string{string(r.Group), r.MQTT}}
}

// bind builds the state (and optionally command) bindings for a register.
// Address is filled by [withDevice] once the serial is known.
func readBinds(s hamodel.Slot) []hamodel.Binding {
	return []hamodel.Binding{{Role: hamodel.RoleState, Slot: s, Mode: hamodel.Read}}
}

func readWriteBinds(s hamodel.Slot) []hamodel.Binding {
	return []hamodel.Binding{
		{Role: hamodel.RoleState, Slot: s, Mode: hamodel.Read},
		{Role: hamodel.RoleCommand, Slot: s, Mode: hamodel.Write},
	}
}

func localized(en, de string) hamodel.Localized {
	l := hamodel.Localized{Default: en}
	if de != "" {
		l.Lang = map[string]string{"de": de}
	}
	return l
}

// valueItemsEnum turns a register's value items into a hamodel.Enum whose
// codes are ordered the way the shipped builder orders them: by numeric
// code, ascending. extra is appended verbatim (the literal "Unknown" an
// enum sensor's option list ends with, in every language).
func valueItemsEnum(r *registers.Register, extra ...string) *hamodel.Enum {
	codes := make([]int, 0, len(r.HassValueItems))
	for c := range r.HassValueItems {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	enum := &hamodel.Enum{Labels: map[string]hamodel.Localized{}}
	de := r.LocalizedValueItems("de")
	for _, c := range codes {
		key := strconv.Itoa(c)
		enum.Codes = append(enum.Codes, key)
		enum.Labels[key] = localized(r.HassValueItems[c], de[c])
	}
	enum.Codes = append(enum.Codes, extra...)
	return enum
}

func sensorEntity(serial string, r *registers.Register) *Entity {
	desc := hamodel.Description{
		Name:         localized(r.Name, r.NameDE),
		DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
		StateClass:   hacatalog.StateClass(r.HassStateClass),
		Unit:         hamodel.Unit(r.Unit),
		Enabled:      hamodel.Ptr(true),
		Availability: hamodel.BridgeOnly(),
	}
	if r.HassValueTemplate != "" {
		desc.ValueTemplate = r.HassValueTemplate
	}
	if r.HassDeviceClass == "enum" && len(r.HassValueItems) > 0 {
		desc.Options = valueItemsEnum(r, "Unknown")
		if r.Unit == "" {
			desc.Unit = ""
		}
	} else if r.Unit == "" {
		// The shipped builder publishes `"unit_of_measurement": ""` on
		// five sensors — an empty string, not an absent key, because the
		// key is set unconditionally from the catalog column and only the
		// enum branch deletes it. discovery.Component's typed field is
		// `omitempty`, so the library cannot express an empty unit at all
		// and Extra is the only route. Reproduced rather than fixed: the
		// goldens pin it as current behaviour and dropping the key here
		// would be a payload change that is not this step.
		desc.Extra = map[string]any{"unit_of_measurement": ""}
	}
	return &Entity{
		Basic: hamodel.Basic{
			EntityKey:      string(PlatformSensor) + "." + r.MQTT,
			EntityPlatform: hacatalog.Platform(PlatformSensor),
			Description:    desc,
			Binds:          readBinds(slot(serial, r)),
		},
		UniqueIDSeed: r.MQTT,
		EntityIDSeed: r.Name,
	}
}

func binarySensorEntity(serial string, r *registers.Register) *Entity {
	return &Entity{
		Basic: hamodel.Basic{
			EntityKey:      string(PlatformBinarySensor) + "." + r.MQTT,
			EntityPlatform: hacatalog.Platform(PlatformBinarySensor),
			Description: hamodel.Description{
				Name:         localized(r.Name, r.NameDE),
				DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
				Enabled:      hamodel.Ptr(true),
				Availability: hamodel.BridgeOnly(),
			},
			Binds: readBinds(slot(serial, r)),
		},
		UniqueIDSeed: r.MQTT,
		EntityIDSeed: r.Name,
		PlatformFields: discovery.BinarySensorFields{
			PayloadOn:  r.HassPayloadOn,
			PayloadOff: r.HassPayloadOff,
		},
	}
}

func numberEntity(serial string, r *registers.Register) *Entity {
	return &Entity{
		Basic: hamodel.Basic{
			EntityKey:      string(PlatformNumber) + "." + r.MQTT,
			EntityPlatform: hacatalog.Platform(PlatformNumber),
			Description: hamodel.Description{
				Name:         localized(r.Name, r.NameDE),
				DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
				Unit:         hamodel.Unit(r.Unit),
				Enabled:      hamodel.Ptr(false),
				Availability: hamodel.BridgeOnly(),
			},
			Binds: readWriteBinds(slot(serial, r)),
		},
		UniqueIDSeed:   r.MQTT,
		EntityIDSeed:   r.Name,
		PlatformFields: discovery.NumberFields{Mode: "box"},
	}
}

func selectEntity(serial string, r *registers.Register) *Entity {
	return &Entity{
		Basic: hamodel.Basic{
			EntityKey:      string(PlatformSelect) + "." + r.MQTT,
			EntityPlatform: hacatalog.Platform(PlatformSelect),
			Description: hamodel.Description{
				Name:         localized(r.Name, r.NameDE),
				Options:      valueItemsEnum(r),
				Enabled:      hamodel.Ptr(false),
				Availability: hamodel.BridgeOnly(),
			},
			Binds: readWriteBinds(slot(serial, r)),
		},
		UniqueIDSeed: r.MQTT,
		EntityIDSeed: r.Name,
	}
}

func switchEntity(serial string, r *registers.Register) *Entity {
	return &Entity{
		Basic: hamodel.Basic{
			EntityKey:      string(PlatformSwitch) + "." + r.MQTT,
			EntityPlatform: hacatalog.Platform(PlatformSwitch),
			Description: hamodel.Description{
				Name:         localized(r.Name, r.NameDE),
				DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
				Enabled:      hamodel.Ptr(false),
				Availability: hamodel.BridgeOnly(),
			},
			Binds: readWriteBinds(slot(serial, r)),
		},
		UniqueIDSeed: r.MQTT,
		EntityIDSeed: r.Name,
		PlatformFields: discovery.SwitchFields{
			PayloadOn:  r.HassPayloadOn,
			PayloadOff: r.HassPayloadOff,
		},
	}
}

func virtualSwitchEntity(serial string, v VirtualSwitch) *Entity {
	s := hamodel.Slot{Address: serial, Path: []string{v.Group, v.Key}}
	return &Entity{
		Basic: hamodel.Basic{
			EntityKey:      string(PlatformSwitch) + "." + v.Key,
			EntityPlatform: hacatalog.Platform(PlatformSwitch),
			Description: hamodel.Description{
				Name: localized(v.Name, v.NameDE),
				// F7: the synthetic switches ship enabled while every real
				// control ships disabled. Reproduced, not fixed — it is
				// pinned as current behaviour and must not move inside a
				// migration step.
				Enabled:      hamodel.Ptr(true),
				Availability: hamodel.BridgeOnly(),
			},
			Binds: readWriteBinds(s),
		},
		UniqueIDSeed:   v.Key,
		EntityIDSeed:   v.Name,
		PlatformFields: discovery.SwitchFields{PayloadOn: "1", PayloadOff: "0"},
	}
}
