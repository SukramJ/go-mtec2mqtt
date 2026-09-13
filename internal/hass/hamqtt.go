// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
	"sort"
	"strconv"
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
// "<root>/<serial>/<group>/<key>/state".
//
// One function, three callers — the config builder's `state_topic`, the
// poll loop's publish, and the topic golden — because until this release
// there were two independent expressions for it (an fmt.Sprintf in
// internal/hass and another in internal/coordinator) and nothing compared
// them. That is F5 of the phase-6 measurement, and the failure mode is
// silent in both directions: every entity points at a topic nobody
// publishes to, permanently `unknown`, with nothing in the log.
//
// It renders through [Layout] rather than by concatenation so the shipped
// builder and the go-hamqtt path cannot disagree either.
func StateTopic(root, serial, group, key string) string {
	return Layout{Root: root}.State(stateSlot(serial, group, key))
}

// CommandTopic is the topic Home Assistant writes a writable entity's new
// value to: "<root>/<serial>/<group>/<key>/set". The inbound twin of
// [StateTopic], and one function for the same reason.
func CommandTopic(root, serial, group, key string) string {
	return Layout{Root: root}.Command(stateSlot(serial, group, key))
}

// CommandFilter is the MQTT topic filter this daemon subscribes for
// inbound commands: "<root>/+/+/+/set".
//
// Exported because three readers need the same string and used to hold
// three literals: the subscription itself, the command router's route,
// and publisher.StateConfig.CommandFilters — the guard that refuses a
// state publish which would land inside this process's own command
// subscription and be echoed straight back into its own handler.
func CommandFilter(root string) string { return root + "/+/+/+/set" }

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
