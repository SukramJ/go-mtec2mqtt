// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
	"sort"
	"strings"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	"github.com/SukramJ/go-hamqtt/publisher"
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
// and the topics by an own [Layout] over go-hamqtt's topic.SmartHome — the
// mqtt-smarthome 2.0 grammar `<name>/<function>/<item…>` this bridge
// follows since 2.0.0 (openccu-loom ADR 0083). The poll group is not a
// model.Bucket — that vocabulary is paramset names
// (values/master/calculated/custom) — so the group rides in Slot.Path and
// Bucket stays unset, which topic.SmartHome renders as no level at all;
// see notes/adr0070-phase6-measurement.md §5.2.
//
// What 2.0.0 changed here is the topics and the payload encoding, never an
// identity: unique_id, default_entity_id, the device identifiers, the node
// id and the discovery topic are what they were, and
// TestSmartHomeMoveKeepsEveryHomeAssistantIdentity proves it against the
// frozen pre-2.0 document.

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

// Layout renders this bridge's topic schema for the go-hamqtt hamodel:
// mqtt-smarthome 2.0 under the instance name (config.MQTTTopic, "mtec" by
// default), which is never the Home Assistant discovery prefix — the two
// are separate roots in this bridge and the identity namespace is a third,
// constant one.
//
//	<name>/status/<serial>/<group>/<key>   State, a {"val","ts","lc"} object
//	<name>/set/<serial>/<group>/<key>      Command, the same item path
//	<name>/status/<serial>/online          Availability, the inverter's reachability
//	<name>/connected                       Bridge, 0/1/2 — the Last Will writes 0
//
// It is topic.SmartHome unchanged: the slot's address is the serial and its
// path is group and key, which is exactly the item order the library
// renders. It is a type of its own so the one constructor below is where
// the name is read.
type Layout struct {
	topic.SmartHome
}

var _ topic.SmartHomeLayout = Layout{}

// NewLayout returns the layout for an instance name.
//
// The name was validated by config.Validate with the same constructor, so
// the error is unreachable for a loaded config; a name that fails anyway
// yields the zero layout, which renders no topic at all rather than a
// wrong one. A multi-level name ("home/mtec") is kept verbatim, as the
// configured root always was; topic.SmartHome.Conformant reports that it
// runs outside spec §3.
func NewLayout(name string) Layout {
	sh, err := topic.NewSmartHomeMultiLevel(name)
	if err != nil {
		return Layout{}
	}
	return Layout{SmartHome: sh}
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
		Layout: NewLayout(d.mqttTopic),
		Lang:   d.lang,
		// mqtt-smarthome 2.0's status object: entities read
		// `value_json.val`, booleans through `| lower`, and an enum whose
		// labels differ from its tokens through the library's mapping
		// pair — tokens on the wire, labels in Home Assistant.
		Enc:              discovery.StatusObjectEncoding,
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
// appear twice under two platforms each. That a device bundle may carry
// it is SETTLED, not open: go-hamqtt v0.32.0 narrowed discovery.Validate's
// duplicate check to key on (platform, unique_id) — Home Assistant's own
// entity_registry (domain, platform, unique_id) index — after step 3 of
// this migration pinned the previous refusal and the bump turned that pin
// red. TestRenderedBundleAcceptsTheDuplicatedUniqueIDs is the assertion;
// notes/adr0070-phase6-measurement.md §3.3 asked the question and
// notes/adr0070-phase6-steps45-results.md answered it. No live Home
// Assistant session is outstanding for it. Component keys are
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
			out = append(out, sensorEntity(d.serialNo, r, d.lang))
		case PlatformBinarySensor:
			out = append(out, binarySensorEntity(d.serialNo, r))
		case PlatformNumber:
			out = append(out, numberEntity(d.serialNo, r), sensorEntity(d.serialNo, r, d.lang))
		case PlatformSelect:
			out = append(out, selectEntity(d.serialNo, r, d.lang), sensorEntity(d.serialNo, r, d.lang))
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
	comps := make([]discovery.Component, 0, len(d.entries))
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
		comps = append(comps, comp)
	}
	if err := discovery.CheckAvailability(PublishesAvailabilityTopic(d.mqttTopic, d.serialNo), comps...); err != nil {
		return nil, fmt.Errorf("hass: %w", err)
	}
	return out, nil
}

// PublishesAvailabilityTopic is this daemon's answer to
// [discovery.CheckAvailability]: the set of availability topics it actually
// writes — `<name>/connected` ([ConnectedTopic], the Last Will and the
// coordinator's 1/2 transitions) and the inverter's own
// `<name>/status/<serial>/online` ([OnlineTopic]).
//
// # Why this exists
//
// Under `availability_mode: all` Home Assistant requires EVERY listed
// source to report available, and a source nobody writes is not neutral:
// an entity that lists one is permanently unavailable, with nothing on the
// wire and nothing in any log to say why. Every entity here declares both
// levels ([bridgeAndDevice]), so the device level now resolves to a topic
// that must exist — which is what this predicate states, once, for both
// render entry points to check against.
//
// # Why it can fail
//
// The predicate is not the function the payload was rendered from. The
// rendering side goes through [Layout.Bridge] and [Layout.Availability];
// this goes through [ConnectedTopic] and [OnlineTopic], the PUBLISHING
// side's functions (the runtime's will and the coordinator's online item).
// A renderer that drifted from what is published fails the render naming
// the entity and the topic instead of greying out the fleet. Before the
// STATIC read the serial is unknown and no online topic is claimed.
func PublishesAvailabilityTopic(mqttTopic, serial string) func(topic string) bool {
	connected := ConnectedTopic(mqttTopic)
	online := ""
	if serial != "" {
		online = OnlineTopic(mqttTopic, serial)
	}
	return func(t string) bool { return t == connected || (online != "" && t == online) }
}

// RenderBundle renders the same entities as one device bundle — the shape
// step 6 would publish. Nothing publishes it; it exists so
// discovery.Validate has a bundle to check and so the duplicate-unique_id
// question can be asked of the library in code.
func RenderBundle(d *Discovery, origin discovery.Origin) (*discovery.Bundle, error) {
	b, err := discovery.Render(NewRenderContext(d), NewDevice(d), NewEntities(d), origin)
	if err != nil {
		return nil, err
	}
	// The same guard [Render] applies, over the document shape: every
	// availability source an entity names is one this daemon publishes.
	if err := discovery.CheckBundleAvailability(b, PublishesAvailabilityTopic(d.mqttTopic, d.serialNo)); err != nil {
		return nil, fmt.Errorf("hass: %w", err)
	}
	return b, nil
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

// ValueItemsEnum turns a register's value items into a hamodel.Enum whose
// codes are ordered the way the shipped builder orders them: by numeric
// code, ascending. extra is appended verbatim (the literal "Unknown" an
// enum sensor's option list ends with, in every language).
//
// Each code is the register's ENGLISH label — the stable token this bridge
// publishes as `val` since 2.0.0 and accepts on `set` — and carries its
// German label as the translation. Under the status-object encoding the
// library therefore lists the labels of the configured language in
// `options` and maps token to label and back in the value and command
// templates; for English the token is the label and no mapping is needed.
// The Modbus integer stays out of the topic tree: it is the device's
// encoding, not a word a consumer should have to look up.
//
// Exported because the coordinator's `set` path resolves a token, a label
// in either language, or a code against the same enum.
func ValueItemsEnum(r *registers.Register, extra ...string) *hamodel.Enum {
	codes := make([]int, 0, len(r.HassValueItems))
	for c := range r.HassValueItems {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	enum := &hamodel.Enum{Labels: map[string]hamodel.Localized{}}
	de := r.LocalizedValueItems("de")
	for _, c := range codes {
		token := r.HassValueItems[c]
		enum.Codes = append(enum.Codes, token)
		enum.Labels[token] = localized(token, de[c])
	}
	enum.Codes = append(enum.Codes, extra...)
	return enum
}

// bridgeAndDevice is the availability every entity declares: the daemon's
// `<name>/connected` (available at 2, the inverter reachable) AND the
// inverter's own `<name>/status/<serial>/online`, under mode `all`.
//
// It is the library's default, spelled out rather than inherited, because
// the default is only right for a consumer that publishes both topics —
// which this one does since 2.0.0, and [PublishesAvailabilityTopic] checks.
func bridgeAndDevice() hamodel.Availability {
	return hamodel.Availability{
		Levels: []hamodel.AvailabilityLevel{hamodel.LevelBridge, hamodel.LevelDevice},
		Mode:   hamodel.AvailabilityAll,
	}
}

// boolFields are the on/off payloads of every switch and binary sensor:
// the plain booleans of mqtt-smarthome §5.1, which is what `val` carries
// and what discovery.StatusBoolValueTemplate renders it as. The catalog's
// hass_payload_on/off ("1"/"0") are the REGISTER's raw on and off values,
// which the coordinator reads and writes; they no longer reach Home
// Assistant.
var (
	boolSensorFields = discovery.BinarySensorFields{PayloadOn: discovery.PayloadTrue, PayloadOff: discovery.PayloadFalse}
	boolSwitchFields = discovery.SwitchFields{PayloadOn: discovery.PayloadTrue, PayloadOff: discovery.PayloadFalse}
)

// StatusTemplate adapts a catalog value template to the status object.
//
// registers.yaml writes its templates against the plain value — `{{ value
// | round(1) }}` — because that is what a reader of the catalog thinks in,
// and an operator's own catalog from an earlier release is written that
// way too. Since 2.0.0 the payload is `{"val": …}`, so every bare `value`
// identifier is read as `value_json.val`; an identifier that already says
// `value_json` is left alone.
func StatusTemplate(t string) string {
	const ident = "value"
	isWord := func(b byte) bool {
		return b == '_' || b == '.' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	var b strings.Builder
	for i := 0; i < len(t); {
		if strings.HasPrefix(t[i:], ident) &&
			(i == 0 || !isWord(t[i-1])) &&
			(i+len(ident) == len(t) || !isWord(t[i+len(ident)])) {
			b.WriteString("value_json.val")
			i += len(ident)
			continue
		}
		b.WriteByte(t[i])
		i++
	}
	return b.String()
}

// EnumValueTemplate is the value_template of a select and of an enum
// sensor: the wire token mapped to the option Home Assistant lists in lang,
// and `None` — unknown — for anything that is not one of the register's
// tokens: the "Unknown" the coordinator publishes for a code the catalog
// does not map, a non-string, or a payload that is not a status object.
//
// Every token is mapped, in English too where token and option are the
// same string, because the miss is the point. The library's pass-through
// (`m.get(val, val)`, or a bare `{{ value_json.val }}` in English) handed
// "Unknown" to Home Assistant as a state: a select logs "Invalid option"
// at error and keeps showing the last valid option as if it were current
// (homeassistant/components/mqtt/select.py `_message_received`), and an
// enum sensor showed the English word as one of its options. "None" sets
// either to unknown (select.py: `payload.lower() == "none"`; sensor.py:
// PAYLOAD_NONE).
func EnumValueTemplate(e *hamodel.Enum, lang string) string {
	if e == nil || len(e.Codes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`{% set m = {`)
	for i, token := range e.Codes {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(discovery.JinjaQuote(token) + ": " + discovery.JinjaQuote(e.Label(token, lang)))
	}
	b.WriteString(`} %}{% if value_json is defined and value_json.val is string %}` +
		`{{ m.get(value_json.val, 'None') }}{% else %}None{% endif %}`)
	return b.String()
}

// BitFieldValueTemplate is the value_template of a BIT (fault/alarm)
// register's sensor. The wire carries "OK", the comma-separated English
// names of the set flags, or "Unknown" for a field the catalog cannot
// name (see coordinator.decodeCode). The names are mapped onto the labels
// of lang, so a German Home Assistant keeps showing German fault names
// while the wire carries the language-independent tokens; "Unknown", a
// non-string and a payload that is not a status object render `None`,
// which the sensor shows as unknown (mqtt/sensor.py PAYLOAD_NONE).
//
// English needs no mapping, and gets the guard alone.
func BitFieldValueTemplate(r *registers.Register, lang string) string {
	if r.Type != registers.DataBIT || len(r.HassValueItems) == 0 {
		return ""
	}
	const guard = `{% if value_json is defined and value_json.val is string and value_json.val != '' and value_json.val != 'Unknown' %}`
	enum := ValueItemsEnum(r)
	var m strings.Builder
	differs := false
	for i, token := range enum.Codes {
		label := enum.Label(token, lang)
		differs = differs || label != token
		if i > 0 {
			m.WriteString(", ")
		}
		m.WriteString(discovery.JinjaQuote(token) + ": " + discovery.JinjaQuote(label))
	}
	if !differs {
		return guard + `{{ value_json.val }}{% else %}None{% endif %}`
	}
	return `{% set m = {` + m.String() + `} %}` + guard +
		`{% set ns = namespace(out=[]) %}` +
		`{% for t in value_json.val.split(', ') %}{% set ns.out = ns.out + [m.get(t, t)] %}{% endfor %}` +
		`{{ ns.out | join(', ') }}{% else %}None{% endif %}`
}

// numberBounds is the min/max of a number entity whose unit is not a
// percentage: the range the write path accepts for the register — a
// uint16 after scaling (modbus.ParseWriteValue), so 0..65535/scale.
//
// Home Assistant otherwise applies 0..100 (number/const.py
// DEFAULT_MIN_VALUE/DEFAULT_MAX_VALUE) and discards a state outside it
// with an error (mqtt/number.py `_message_received`), which is right for
// the percentage limits and wrong for the charge and discharge current
// limits in A: the register holds up to 6553.5, and the charge/discharge
// "active" switches already write up to CHARGE_ACTIVE_VALUE's 6000. The
// bound is the register's own, not a guess at what a given inverter
// model allows. A percentage keeps Home Assistant's 0..100.
func numberBounds(r *registers.Register) (lo, hi *float64) {
	if r.Unit == "%" {
		return nil, nil
	}
	scale := r.Scale
	if scale < 1 {
		scale = 1
	}
	return new(0.0), new(float64(0xFFFF) / float64(scale))
}

func sensorEntity(serial string, r *registers.Register, lang string) *Entity {
	desc := hamodel.Description{
		Name:         localized(r.Name, r.NameDE),
		DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
		StateClass:   hacatalog.StateClass(r.HassStateClass),
		Unit:         hamodel.Unit(r.Unit),
		Enabled:      new(true),
		Availability: bridgeAndDevice(),
	}
	if r.HassValueTemplate != "" {
		desc.ValueTemplate = StatusTemplate(r.HassValueTemplate)
	} else if t := BitFieldValueTemplate(r, lang); t != "" {
		desc.ValueTemplate = t
	}
	if r.HassDeviceClass == "enum" && len(r.HassValueItems) > 0 {
		// The option list keeps its trailing "Unknown", exactly as 2.0.0
		// published it, so nothing keyed on the list moves; the value
		// template never renders it (the token maps to None).
		desc.Options = ValueItemsEnum(r, "Unknown")
		if r.HassValueTemplate == "" {
			desc.ValueTemplate = EnumValueTemplate(ValueItemsEnum(r), lang)
		}
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
		EntityKey:      string(PlatformSensor) + "." + r.MQTT,
		EntityPlatform: hacatalog.Platform(PlatformSensor),
		Description:    desc,
		Binds:          readBinds(slot(serial, r)),
		UniqueIDSeed:   r.MQTT,
		EntityIDSeed:   r.Name,
	}
}

func binarySensorEntity(serial string, r *registers.Register) *Entity {
	return &Entity{
		EntityKey:      string(PlatformBinarySensor) + "." + r.MQTT,
		EntityPlatform: hacatalog.Platform(PlatformBinarySensor),
		Description: hamodel.Description{
			Name:         localized(r.Name, r.NameDE),
			DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
			Enabled:      new(true),
			Availability: bridgeAndDevice(),
		},
		Binds:          readBinds(slot(serial, r)),
		UniqueIDSeed:   r.MQTT,
		EntityIDSeed:   r.Name,
		PlatformFields: boolSensorFields,
	}
}

func numberEntity(serial string, r *registers.Register) *Entity {
	lo, hi := numberBounds(r)
	return &Entity{
		EntityKey:      string(PlatformNumber) + "." + r.MQTT,
		EntityPlatform: hacatalog.Platform(PlatformNumber),
		Description: hamodel.Description{
			Name:         localized(r.Name, r.NameDE),
			DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
			Unit:         hamodel.Unit(r.Unit),
			Min:          lo,
			Max:          hi,
			Enabled:      new(false),
			Availability: bridgeAndDevice(),
		},
		Binds:          readWriteBinds(slot(serial, r)),
		UniqueIDSeed:   r.MQTT,
		EntityIDSeed:   r.Name,
		PlatformFields: discovery.NumberFields{Mode: "box"},
	}
}

func selectEntity(serial string, r *registers.Register, lang string) *Entity {
	options := ValueItemsEnum(r)
	return &Entity{
		EntityKey:      string(PlatformSelect) + "." + r.MQTT,
		EntityPlatform: hacatalog.Platform(PlatformSelect),
		Description: hamodel.Description{
			Name:          localized(r.Name, r.NameDE),
			Options:       options,
			ValueTemplate: EnumValueTemplate(options, lang),
			Enabled:       new(false),
			Availability:  bridgeAndDevice(),
		},
		Binds:        readWriteBinds(slot(serial, r)),
		UniqueIDSeed: r.MQTT,
		EntityIDSeed: r.Name,
	}
}

func switchEntity(serial string, r *registers.Register) *Entity {
	return &Entity{
		EntityKey:      string(PlatformSwitch) + "." + r.MQTT,
		EntityPlatform: hacatalog.Platform(PlatformSwitch),
		Description: hamodel.Description{
			Name:         localized(r.Name, r.NameDE),
			DeviceClass:  hamodel.DeviceClass(r.HassDeviceClass),
			Enabled:      new(false),
			Availability: bridgeAndDevice(),
		},
		Binds:          readWriteBinds(slot(serial, r)),
		UniqueIDSeed:   r.MQTT,
		EntityIDSeed:   r.Name,
		PlatformFields: boolSwitchFields,
	}
}

func virtualSwitchEntity(serial string, v VirtualSwitch) *Entity {
	s := hamodel.Slot{Address: serial, Path: []string{v.Group, v.Key}}
	return &Entity{
		EntityKey:      string(PlatformSwitch) + "." + v.Key,
		EntityPlatform: hacatalog.Platform(PlatformSwitch),
		Description: hamodel.Description{
			Name: localized(v.Name, v.NameDE),
			// F7: the synthetic switches ship enabled while every real
			// control ships disabled. Reproduced, not fixed — it is
			// pinned as current behaviour and must not move inside a
			// migration step.
			Enabled:      new(true),
			Availability: bridgeAndDevice(),
		},
		Binds:          readWriteBinds(s),
		UniqueIDSeed:   v.Key,
		EntityIDSeed:   v.Name,
		PlatformFields: boolSwitchFields,
	}
}

// --- the runtime seams (ADR 0070 phase 6, steps 4 and 5) --------------------
//
// Everything below is consumed by the publish path rather than by the
// parallel rendering experiment above. It exists in this file because it
// is the same [Layout] and the same go-hamqtt vocabulary: the point of
// step 4 is that the topic a value is published to and the topic a config
// advertises are one function, not two expressions that happen to agree.

// LegacyConfigTopic renders one retained per-entity discovery config topic
// through go-hamqtt's own [publisher.LegacyTopicByUniqueID] — the
// four-segment "<prefix>/<platform>/<unique_id>/config" form named by
// [LegacyConfigTopicForm].
//
// It is the single spelling of that topic in this repository: the builder
// publishes to it, the orphan sweep retracts through it, and step 6 will
// hand the same function to publisher.Config.LegacyEntityTopics via
// [LegacyConfigTopicForms]. Three readers, one formula — which is what
// keeps the retract-then-publish ordering of step 6 expressible at all: a
// bundle can only supersede the topics the fleet is actually on, and a
// second spelling here would retract a shape nobody published.
func LegacyConfigTopic(prefix string, platform Platform, uniqueID string) string {
	return publisher.LegacyTopicByUniqueID(publisher.LegacyEntity{
		Prefix:   prefix,
		Platform: string(platform),
		UniqueID: uniqueID,
	})
}

// LegacyAvailabilityTopic is the retained availability marker every
// release up to 1.9.0 wrote, "<hass_base>/status/lwt".
//
// It is a RETRACTION target and nothing else: the marker moved to
// [BridgeStatusTopic] in this bridge's own publish root, because
// "<hass_base>/status/lwt" sits one level under
// [publisher.BirthTopic] — the topic Home Assistant publishes its own
// birth message to, and which this daemon subscribes to. An upgrading
// broker still holds the stale "online" there, so the daemon clears it on
// every connect.
//
// It is a function here rather than a string expression at the
// composition root because it used to be one: cmd/mtec2mqtt spelled
// `cfg.HASSBaseTopic + "/status/lwt"` inline, its test passed the same
// literal in and asserted publication to it, and mutating the production
// spelling to "/status/LWT" left the suite green. One spelling, and the
// test now reads it from here.
//
// The prefix is trimmed of a trailing slash the same way
// [publisher.BirthTopic] trims it, so a HASS_BASE_TOPIC written
// "homeassistant/" retracts the topic the old release actually wrote
// rather than a double-slashed sibling of it.
func LegacyAvailabilityTopic(prefix string) string {
	return publisher.BirthTopic(prefix) + "/lwt"
}

// LegacyConfigTopicForms is what publisher.Config.LegacyEntityTopics must
// be set to for this fleet, and is named here rather than at the
// composition root because [LegacyConfigTopicForm] measured it here.
//
// Stating it replaces the library's default rather than adding to it, and
// that is the intent: this bridge's 100 retained configs are all on the
// four-segment form (100 of 100, measured against the pins; the
// five-segment default matches 0), so retracting the five-segment shape as
// well would reach into a discovery tree this daemon shares with other
// writers for no gain.
//
// It is already wired at the composition root even though nothing
// publishes a bundle yet: publisher.Config.LegacyEntityTopics is read by
// [publisher.Runtime.PublishBundle] alone, so setting it now is inert on
// the wire and puts the form in the boot log
// ("publisher.legacy_forms"), where an operator can see it before the
// migration rather than after it failed silently.
func LegacyConfigTopicForms() []publisher.LegacyTopicFunc {
	return []publisher.LegacyTopicFunc{publisher.LegacyTopicByUniqueID}
}

// StateTopic is the topic a register's current value is published to:
// "<name>/status/<serial>/<group>/<key>".
//
// One function, three callers — the config builder's `state_topic`, the
// poll loop's publish, and the topic golden — because there used to be two
// independent expressions for it (an fmt.Sprintf in internal/hass and
// another in internal/coordinator) and nothing compared them. That is F5
// of the phase-6 measurement, and the failure mode is silent in both
// directions: every entity points at a topic nobody publishes to,
// permanently `unknown`, with nothing in the log.
//
// It renders through [Layout] rather than by concatenation so the shipped
// builder and the go-hamqtt path cannot disagree either.
func StateTopic(root, serial, group, key string) string {
	return NewLayout(root).State(stateSlot(serial, group, key))
}

// CommandTopic is the topic Home Assistant writes a writable entity's new
// value to: "<name>/set/<serial>/<group>/<key>", the same item path as
// [StateTopic]. The inbound twin of it, and one function for the same
// reason.
func CommandTopic(root, serial, group, key string) string {
	return NewLayout(root).Command(stateSlot(serial, group, key))
}

// CommandFilter is the MQTT topic filter this daemon subscribes for
// inbound register commands: "<name>/set/+/+/+" (serial, group, key).
//
// Exported because three readers need the same string and used to hold
// three literals: the subscription itself, the command router's route,
// and publisher.StateConfig.CommandFilters — the guard that refuses a
// state publish which would land inside this process's own command
// subscription and be echoed straight back into its own handler.
func CommandFilter(root string) string {
	return NewLayout(root).Name() + "/" + topic.FunctionSet + "/+/+/+"
}

// ConnectedTopic is `<name>/connected`, the daemon's own 0/1/2 marker:
// 0 from the Last Will and on a graceful stop, 1 while the broker is
// reachable and the inverter is not, 2 while both are. Every entity's
// bridge-level availability reads it.
func ConnectedTopic(root string) string { return NewLayout(root).Connected() }

// OnlineTopic is `<name>/status/<serial>/online`, the inverter's own
// reachability as a boolean status item. Every entity's device-level
// availability reads it.
func OnlineTopic(root, serial string) string {
	return NewLayout(root).Availability(hamodel.Slot{Address: serial})
}

func stateSlot(serial, group, key string) hamodel.Slot {
	return hamodel.Slot{Address: serial, Path: []string{group, key}}
}

// OwnsConfigTopic reports whether a parsed retained discovery config topic
// has the shape this daemon publishes.
//
// It is the only question [publisher.SweepRequest.Owns] can be asked: the
// predicate runs on the transport's read loop with the parsed topic and
// nothing else, before the payload is offered to Inspect. It is therefore
// deliberately the NARROWER of the two ownership checks and not the
// decisive one — [Discovery.IsOwnConfig] still judges the body, and a
// retained config this daemon did not write is never retracted on the
// strength of its topic alone.
//
// Narrow means three conditions, each of which excludes a real population
// of a shared discovery tree:
//
//   - the four-segment per-entity form only. A device document (step 6's
//     shape, and every Tasmota-style writer's), a five-segment node-id
//     config and the node-id-less three-segment form all belong to
//     somebody else today.
//   - a platform this daemon actually emits. Five of Home Assistant's 32.
//   - an object id — which in this form IS the unique_id — inside the
//     compile-time "MTEC_" namespace.
//
// The width of this predicate is what a sweep can destroy: openccu-loom's
// PR #817 found retraction prefixes that owned 100 % of a sibling
// daemon's configs. Widening any of the three conditions is a decision
// about what this daemon is willing to delete from a tree it does not own.
//
// # An upgrade tripwire, recorded because nothing would catch it
//
// Three of the guards below are mutually redundant TODAY, and only because
// of a contract that lives in another module. publisher.ParseConfigTopic
// never returns a topic with Bundle set AND a Platform, and never one with
// an empty ObjectID in the four-segment form — so `t.Bundle`, `t.Platform
// == ""` and `t.ObjectID == ""` each already imply what the others reject,
// and a mutation pass finds all three survivable. They are kept anyway,
// because the redundancy is not this package's to guarantee: a future
// go-hamqtt that let a bundle topic carry a Platform would evaporate it
// silently, and the consequence would be this daemon claiming — and its
// sweep retracting — a sibling instance's ENTIRE device document. If that
// contract ever changes, this is the function to re-derive.
func OwnsConfigTopic(t publisher.ConfigTopic) bool {
	if t.Bundle || t.Platform == "" || t.NodeID != "" || t.ObjectID == "" {
		return false
	}
	if !publishedPlatforms[Platform(t.Platform)] {
		return false
	}
	return strings.HasPrefix(t.ObjectID, uniqueIDPrefix)
}

// publishedPlatforms is the closed set of Home Assistant platforms this
// daemon emits, read by [OwnsConfigTopic]. A platform absent here is a
// platform whose retained configs under the shared discovery prefix are
// not this daemon's business.
var publishedPlatforms = map[Platform]bool{
	PlatformSensor:       true,
	PlatformBinarySensor: true,
	PlatformNumber:       true,
	PlatformSelect:       true,
	PlatformSwitch:       true,
}

// --- the device bundle (ADR 0070 phase 6, step 6) ---------------------------

// OriginName and OriginURL identify this bridge in the `origin` block Home
// Assistant requires on a device bundle. Constants, not configuration:
// the block is what an operator reads on the Home Assistant device page to
// find out which program wrote the document, and a configurable value
// there would name whatever the last writer happened to be called.
const (
	OriginName = "go-mtec2mqtt"
	OriginURL  = "https://github.com/SukramJ/go-mtec2mqtt"
)

// BundleOrigin is the `origin` block this daemon stamps on its device
// bundle. sw is the bridge's own build version (internal/version.Version),
// NOT the inverter firmware — that one is already the device block's
// `sw_version` and the two answer different questions.
//
// It is a parameter rather than a direct read of internal/version so the
// pinned bundle golden does not move on every release: the payload is
// otherwise byte-deterministic, and a link-time variable inside it would
// make the frozen artefact stale the moment a tag is cut.
// TestBundleOriginIsWiredFromTheBuildVersion pins the wiring instead.
func BundleOrigin(sw string) discovery.Origin {
	return discovery.Origin{Name: OriginName, SW: sw, URL: OriginURL}
}

// BundleNodeID is the single topic segment this daemon's device bundle is
// published under.
//
// It is discovery.NodeID over [NewDevice], which is topic.Slug of the
// device's primary identifier — and this bridge's primary identifier is the
// inverter's bare serial number, with no namespace (see [NewDevice]). So
// the node id IS the serial, and that is the property that matters:
//
//   - It is per-inverter. Two daemons against two inverters publish to two
//     different bundle topics and cannot overwrite, retract or fight over
//     each other's document. That is NOT true of the per-entity form this
//     replaces, whose topics carry no serial at all under the shipped
//     defaults — recorded as F16 in
//     notes/adr0070-phase6-steps45-results.md §4.3.
//   - It is stable. It is derived from the STATIC register read and from
//     nothing configurable, so no operator setting can move it. A moved
//     node id would leave the old document retained, announcing the same
//     device from a second topic.
//   - It is never the device name. A renamed device keeps its topic.
//
// It is empty before [Discovery.Initialize] has run, because the serial is
// not known until the first STATIC read.
func BundleNodeID(d *Discovery) string { return discovery.NodeID(NewDevice(d)) }

// BundleConfigTopic is where [BundleNodeID]'s document is retained:
// "<prefix>/device/<node id>/config".
func BundleConfigTopic(prefix string, d *Discovery) string {
	return publisher.BundleConfigTopic(prefix, BundleNodeID(d))
}

// SupersededConfigTopics is the per-entity config topics the bundle
// replaces, exactly as publisher.Runtime.PublishBundle will retract them.
//
// Exported so a test can compare the retraction list against the frozen
// pre-migration pins without re-deriving it — deriving it a second time is
// how two call sites end up disagreeing about which topics get cleared,
// and the cost of a disagreement here is total and silent (see
// [LegacyConfigTopicForm]).
func SupersededConfigTopics(prefix string, b *discovery.Bundle) []string {
	return publisher.SupersededTopics(prefix, b, LegacyConfigTopicForms()...)
}
