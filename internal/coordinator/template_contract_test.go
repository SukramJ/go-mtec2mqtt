// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// The device document paired with the bytes it reads.
//
// Every other pin in this repository looks at one side: the document byte
// for byte (internal/hass/testdata/bundle_*.json), or the topic tree
// (testdata/topics.json). Nothing asked what Home Assistant makes of the one
// when it reads the other. 2.0.0 was reviewed that way by hand, once; this
// is the permanent form of that review.
//
// The test runs the production publish path — the shipped registers.yaml,
// registers.Decode over raw Modbus words (what modbus.Reader hands the
// coordinator), processValues, the pseudo-registers, the virtual switches,
// wireValue and the state plane, plus updateUpstream and the reconnect hook
// that drive `<name>/connected` and `<name>/status/<serial>/online` —
// through a scripted sequence of register values, in English and German.
// It then evaluates every component of the document it published against
// every payload written to that component's topics, with the strict Jinja
// subset in jinja_test.go, which fails on a shape it cannot evaluate rather
// than skipping it. What Home Assistant does with the rendered string is
// modelled from core 2026.10, homeassistant/components/mqtt/:
//
//   - sensor.py `_update_state`: "None" sets the value unknown; a sensor
//     with a unit, a state class or a non-enum device class must render a
//     number; an enum sensor ignores a value outside its options with a
//     warning. An empty unit_of_measurement is dropped first (sensor.py
//     validate_sensor_state_and_device_class_config), so it is no unit.
//   - number.py `_message_received`: payload_reset (default "None") is
//     unknown; anything else must parse as a number within min..max, which
//     default to 0..100 (number/const.py DEFAULT_MIN_VALUE/MAX_VALUE).
//   - select.py `_message_received`: "none" in any case is unknown;
//     anything else must be one of the options or is logged "Invalid
//     option" at error and ignored.
//   - switch.py `_is_on_map` and binary_sensor.py `_state_message_received`:
//     payload_on, payload_off or "None".
//   - entity.py `_availability_message_received`: the rendered value is
//     compared with payload_available / payload_not_available.
//   - a template that raises is logged at ERROR and the entity keeps its
//     previous state (helpers/template/__init__.py
//     async_render_with_possible_json_value); rendering or testing an
//     undefined logs a WARNING (make_logging_undefined). Both fail here.
//
// Commands go the other way through the real router and set handler: each
// select option through its command_template to the register code the write
// path resolves it to (Register.CodeForLabel), each number value Home
// Assistant would send back (number.py async_set_native_value: an integral
// float as an int) to the raw word it was read from (modbus.ParseWriteValue),
// and each switch's payload_on/off to two distinct writes.
//
// What it cannot evaluate: Home Assistant's own state machinery beyond the
// rendered string (expire_after, force_update, the recorder), templates of a
// shape the subset refuses (it fails then, it does not skip), and values the
// Modbus transport itself never yields. An absent or cleared value is not a
// payload this bridge writes: a register missing from a read is skipped,
// never published empty or null, which the test asserts for every state
// topic rather than renders.

// contractVariant maps a register to the raw words one step reads for it.
type contractVariant func(r *registers.Register) []uint16

// contractStep is one poll of every group with the words of one variant,
// and the rendered states it must leave behind, keyed by component key.
type contractStep struct {
	name  string
	words contractVariant
	want  map[string]string
}

func contractLength(r *registers.Register) int {
	if r.Length <= 0 {
		return 1
	}
	return r.Length
}

// enumCodes is a value-mapped register's codes (BIT: masks), ascending.
func enumCodes(r *registers.Register) []int {
	codes := make([]int, 0, len(r.HassValueItems))
	for c := range r.HassValueItems {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	return codes
}

// bitWords places the mask bits in the low word of a BIT field and the
// high bits in the word before it, as the inverter's big-endian field does.
func bitWords(r *registers.Register, bits uint64) []uint16 {
	n := contractLength(r)
	w := make([]uint16, n)
	for i := n - 1; i >= 0; i-- {
		w[i] = uint16(bits & 0xFFFF) //nolint:gosec // masked to 16 bits
		bits >>= 16
	}
	return w
}

func strWords(s string, n int) []uint16 {
	b := []byte(s)
	w := make([]uint16, n)
	for i := range w {
		var hi, lo byte
		if 2*i < len(b) {
			hi = b[2*i]
		}
		if 2*i+1 < len(b) {
			lo = b[2*i+1]
		}
		w[i] = uint16(hi)<<8 | uint16(lo)
	}
	return w
}

// percentNumber reports a writable percentage: an inverter never reports
// one above 100, so the variants keep those within range.
func percentNumber(r *registers.Register) bool {
	return r.Unit == "%" && hass.Platform(r.HassComponentType) == hass.PlatformNumber
}

func scaleOf(r *registers.Register) int {
	if r.Scale < 1 {
		return 1
	}
	return r.Scale
}

// typicalWords is a plausible reading of every register: a mid-range
// number, a switch on, the first enum code, one named fault bit.
func typicalWords(r *registers.Register) []uint16 {
	n := contractLength(r)
	switch r.Type {
	case registers.DataBIT:
		if codes := enumCodes(r); len(codes) > 0 {
			return bitWords(r, uint64(codes[0])) //nolint:gosec // masks are positive
		}
		return make([]uint16, n)
	case registers.DataSTR:
		return strWords("MT1234567890", n)
	case registers.DataBYTE:
		return []uint16{0x011B, 0x3414, 0x0304, 0x0506}[:n]
	case registers.DataDAT:
		return []uint16{0x1A05, 0x190E, 0x1E2D}
	case registers.DataU32, registers.DataS32, registers.DataI32:
		return []uint16{0x0001, 0x86A0} // 100000
	case registers.DataS16, registers.DataI16:
		return []uint16{0xFFF6} // -10
	case registers.DataU16:
	}
	switch {
	case r.HassValueItems != nil:
		return []uint16{uint16(enumCodes(r)[0])} //nolint:gosec // catalog codes fit a word
	case isBoolRegister(r):
		return []uint16{1}
	case percentNumber(r):
		return []uint16{uint16(50*scaleOf(r) + scaleOf(r)/2)} //nolint:gosec // 50.5 %
	}
	return []uint16{1234}
}

// withWords overrides typicalWords for the registers f answers.
func withWords(f func(r *registers.Register) ([]uint16, bool)) contractVariant {
	return func(r *registers.Register) []uint16 {
		if w, ok := f(r); ok {
			return w
		}
		return typicalWords(r)
	}
}

// nthCode puts the i-th code of every enum, the i-th named mask alone in
// every BIT field, and alternates the switches.
func nthCode(i int) contractVariant {
	return withWords(func(r *registers.Register) ([]uint16, bool) {
		switch {
		case r.Type == registers.DataBIT && r.HassValueItems != nil:
			codes := enumCodes(r)
			return bitWords(r, uint64(codes[i%len(codes)])), true //nolint:gosec // masks are positive
		case r.HassValueItems != nil:
			codes := enumCodes(r)
			return []uint16{uint16(codes[i%len(codes)])}, true //nolint:gosec // catalog codes fit a word
		case isBoolRegister(r):
			return []uint16{uint16(i % 2)}, true //nolint:gosec // 0 or 1
		}
		return nil, false
	})
}

func contractSteps(catalog *registers.Map) []contractStep {
	maxCodes := 0
	for _, r := range catalog.All {
		maxCodes = max(maxCodes, len(r.HassValueItems))
	}
	steps := make([]contractStep, 0, maxCodes+6)
	steps = append(steps, contractStep{name: "typical", words: typicalWords})
	for i := range maxCodes {
		steps = append(steps, contractStep{name: fmt.Sprintf("code %d", i), words: nthCode(i)})
	}
	steps = append(steps,
		contractStep{
			name: "all zero",
			words: func(r *registers.Register) []uint16 {
				return make([]uint16, contractLength(r))
			},
			// Code 0 of `mode` is not in the catalog (257/258/259/512):
			// the select and the enum sensor read unknown, not the last
			// valid option. A field with no bit set is "OK".
			want: map[string]string{
				"select.mode": "None", "sensor.mode": "None",
				"sensor.fault_flag_1": "OK", "sensor.bms_error_code": "OK",
				"switch.grid_inject_switch": "false", "switch.charge_active": "false",
			},
		},
		contractStep{
			name: "all ones",
			words: func(r *registers.Register) []uint16 {
				if percentNumber(r) {
					return []uint16{uint16(100 * scaleOf(r))} //nolint:gosec // 100 %
				}
				w := make([]uint16, contractLength(r))
				for i := range w {
					w[i] = 0xFFFF
				}
				return w
			},
			// 6553.5 A: above Home Assistant's default maximum of 100,
			// inside the register's own range the number now declares.
			want: map[string]string{
				"number.charge_limit": "6553.5", "sensor.inverter_status": "None",
				"switch.grid_inject_switch": "false",
			},
		},
		contractStep{
			name: "unmapped codes and bits",
			words: withWords(func(r *registers.Register) ([]uint16, bool) {
				switch {
				case r.Type == registers.DataBIT && r.HassValueItems != nil:
					return bitWords(r, 1<<16), true // a high-word bit no mask names
				case r.HassValueItems != nil:
					return []uint16{99}, true
				case isBoolRegister(r):
					return []uint16{2}, true // neither on (1) nor off (0)
				}
				return nil, false
			}),
			want: map[string]string{
				"select.mode": "None", "sensor.mode": "None", "sensor.inverter_status": "None",
				"sensor.bms_status": "None", "sensor.battery_mode": "None",
				"sensor.fault_flag_1": "None", "sensor.bms_alarm_code": "None",
				"binary_sensor.grid_inject_switch": "false",
			},
		},
		contractStep{
			name: "several bits, named and unnamed",
			words: withWords(func(r *registers.Register) ([]uint16, bool) {
				if r.Type != registers.DataBIT || r.HassValueItems == nil {
					return nil, false
				}
				bits := uint64(1 << 16)
				for _, c := range enumCodes(r) {
					bits |= uint64(c) //nolint:gosec // masks are positive
				}
				return bitWords(r, bits), true
			}),
		},
		contractStep{name: "typical again", words: typicalWords},
	)
	return steps
}

// TestTemplatesAcceptWhatThePublishPathWrites is the contract test. On
// 2.0.0 it fails: the select rendered "Unknown" for code 0 of `mode` (an
// invalid option, ignored with an error), the enum sensors showed the
// English word "Unknown" instead of unknown, a fault field whose only set
// bit has no name read "OK", and charge_limit at 6553.5 A was refused as
// outside 0..100.
func TestTemplatesAcceptWhatThePublishPathWrites(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) { runContract(t, lang) })
	}
}

func contractCoordinator(t *testing.T, lang string) (*Coordinator, *stubReader, *stubMQTT, *stubModbus) {
	t.Helper()
	catalog, _, err := registers.Load("../../registers.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cfg, err := config.Load(strings.NewReader(`
MODBUS_IP: 127.0.0.1
MODBUS_PORT: 502
MODBUS_SLAVE: 247
MODBUS_TIMEOUT: 5
MQTT_SERVER: localhost
MQTT_PORT: 1883
MQTT_TOPIC: MTEC
HASS_ENABLE: true
LANGUAGE: `+lang+`
`), nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	stub := newStubMQTT()
	link := &stubModbus{}
	link.connected.Store(true)
	reader := newStubReader()
	virtual := hass.DefaultVirtualSwitches(cfg.ChargeActiveValue, cfg.DischargeActiveValue)
	discovery := hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, catalog, cfg.Language, virtual, cfg.DeviceName)
	discovery.Initialize(goldenSerial, goldenFirmware, goldenEquipment)
	tick := time.Date(2026, 5, 25, 14, 30, 45, 0, time.UTC)
	deps := Deps{
		Cfg:     cfg,
		Catalog: catalog,
		Modbus:  link,
		Reader:  reader,
		MQTT:    stub,
		HASS:    discovery,
		Virtual: virtual,
		Logger:  slog.New(slog.DiscardHandler),
		Now: func() time.Time {
			tick = tick.Add(time.Second)
			return tick
		},
	}
	wirePlanes(t, &deps, stub)
	c := New(deps)
	c.topicBase.Store(topicParts{root: cfg.MQTTTopic, serial: goldenSerial})
	c.buildBundle()
	if c.haBundle == nil {
		t.Fatal("the shipped catalogue does not render a device document")
	}
	return c, reader, stub, link
}

// pollAll reads every group with the words variant gives each register —
// decoded by registers.Decode, keyed as modbus.Reader keys them — and runs
// the production publish path over it.
func pollAll(c *Coordinator, reader *stubReader, words contractVariant) {
	reader.mu.Lock()
	for _, g := range c.deps.Catalog.Groups {
		data := map[string]any{}
		for _, r := range c.deps.Catalog.ByGroup(g) {
			if !r.IsModbus() {
				continue // a pseudo-register: computed, never read
			}
			if v, err := registers.Decode(r, words(r)); err == nil {
				key := r.MQTT
				if key == "" {
					key = r.Name
				}
				data[key] = v
			}
		}
		reader.groupData[g] = data
	}
	reader.mu.Unlock()
	log := slog.New(slog.DiscardHandler)
	for _, g := range c.deps.Catalog.Groups {
		c.publishGroupOnce(context.Background(), log, g)
	}
}

func runContract(t *testing.T, lang string) {
	t.Helper()
	ctx := context.Background()
	c, reader, stub, link := contractCoordinator(t, lang)
	if err := c.installInboundHandler(ctx); err != nil {
		t.Fatalf("installInboundHandler: %v", err)
	}
	t.Cleanup(func() { c.stopCommands(context.Background()) })

	c.PublishOnline(ctx) // connected 2, online true
	c.publishDiscovery(ctx)

	steps := contractSteps(c.deps.Catalog)
	stepEnds := make([]int, 0, len(steps))
	for _, s := range steps {
		pollAll(c, reader, s.words)
		stepEnds = append(stepEnds, len(stub.snapshotPublishes()))
	}

	// The inverter drops (connected 1, online false), the broker connection
	// is re-established while it is away, it comes back, and the daemon
	// shuts down (connected 0).
	link.connected.Store(false)
	pollAll(c, reader, typicalWords)
	c.PublishOnline(ctx)
	link.connected.Store(true)
	pollAll(c, reader, typicalWords)
	c.PublishOffline(ctx)

	wire := stub.snapshotPublishes()
	assertNoEmptyStatus(t, wire)
	doc := contractDocument(t, wire)
	if len(doc) != 100 {
		t.Fatalf("the published document holds %d components, want 100", len(doc))
	}
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		comp := contractComponent{t: t, key: key, body: doc[key]}
		comp.checkState(wire, steps, stepEnds)
		comp.checkAvailability(wire)
		comp.checkCommand(c, reader, stub, wire)
	}
}

// assertNoEmptyStatus holds the publish path to what it claims: a value
// that is absent from a read is skipped, never written as an empty retained
// payload (which would clear the item) or as a null `val`.
func assertNoEmptyStatus(t *testing.T, wire []publishCall) {
	t.Helper()
	for _, rec := range wire {
		if !strings.HasPrefix(rec.topic, "MTEC/status/") {
			continue
		}
		if len(rec.payload) == 0 {
			t.Errorf("%s: an empty payload, which clears the retained item", rec.topic)
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(rec.payload, &item); err != nil {
			t.Errorf("%s: %q is not a status object: %v", rec.topic, rec.payload, err)
			continue
		}
		if v, ok := item["val"]; !ok || v == nil {
			t.Errorf("%s: %q carries no val", rec.topic, rec.payload)
		}
	}
}

// contractDocument decodes the last device document on the wire.
func contractDocument(t *testing.T, wire []publishCall) map[string]map[string]any {
	t.Helper()
	var out map[string]map[string]any
	for _, rec := range wire {
		if !strings.HasPrefix(rec.topic, "homeassistant/device/") || len(rec.payload) == 0 {
			continue
		}
		var doc struct {
			Components map[string]map[string]any `json:"components"`
		}
		if err := json.Unmarshal(rec.payload, &doc); err != nil {
			t.Fatalf("%s: %v", rec.topic, err)
		}
		out = doc.Components
	}
	if out == nil {
		t.Fatal("the publish path wrote no device document")
	}
	return out
}

type contractComponent struct {
	t    *testing.T
	key  string
	body map[string]any
}

func (c contractComponent) str(field string) string {
	s, _ := c.body[field].(string)
	return s
}

func (c contractComponent) errorf(format string, args ...any) {
	c.t.Helper()
	c.t.Errorf("%s: %s", c.key, fmt.Sprintf(format, args...))
}

func (c contractComponent) options() []string {
	raw, _ := c.body["options"].([]any)
	out := make([]string, 0, len(raw))
	for _, o := range raw {
		s, _ := o.(string)
		out = append(out, s)
	}
	return out
}

// mqttKey is the register (or virtual switch) the component reads.
func (c contractComponent) mqttKey() string {
	_, key, _ := strings.Cut(c.key, ".")
	return key
}

// render applies a template the way Home Assistant does, or passes the
// payload through when there is none.
func (c contractComponent) render(field string, payload []byte) (string, error) {
	tmpl := c.str(field)
	if tmpl == "" {
		return strings.TrimSpace(string(payload)), nil
	}
	return renderJinja(tmpl, string(payload))
}

func (c contractComponent) bounds() (lo, hi float64) {
	lo, hi = 0, 100 // number/const.py DEFAULT_MIN_VALUE, DEFAULT_MAX_VALUE
	if v, ok := c.body["min"].(float64); ok {
		lo = v
	}
	if v, ok := c.body["max"].(float64); ok {
		hi = v
	}
	return lo, hi
}

// accepts reports whether the platform takes the rendered state, per the
// Home Assistant handlers named at the top of this file.
func (c contractComponent) accepts(state string) error {
	platform := c.str("platform")
	switch platform {
	case "sensor":
		if state == "None" {
			return nil
		}
		if opts := c.options(); len(opts) > 0 {
			if !slices.Contains(opts, state) {
				return fmt.Errorf("enum sensor state %q is not in %v", state, opts)
			}
			return nil
		}
		dc := c.str("device_class")
		if strings.TrimSpace(c.str("unit_of_measurement")) != "" || c.str("state_class") != "" || (dc != "" && dc != "enum") {
			if _, err := strconv.ParseFloat(state, 64); err != nil {
				return fmt.Errorf("numeric sensor state %q is not a number", state)
			}
		}
		if state == "" {
			return fmt.Errorf("empty state")
		}
		return nil
	case "number":
		if state == "None" {
			return nil
		}
		f, err := strconv.ParseFloat(state, 64)
		if err != nil {
			return fmt.Errorf("number state %q is not a number", state)
		}
		if lo, hi := c.bounds(); f < lo || f > hi {
			return fmt.Errorf("number state %v outside %v..%v", f, lo, hi)
		}
		return nil
	case "select":
		if strings.EqualFold(state, "none") {
			return nil
		}
		if !slices.Contains(c.options(), state) {
			return fmt.Errorf("select state %q is not in %v", state, c.options())
		}
		return nil
	case "switch", "binary_sensor":
		on, off := c.str("payload_on"), c.str("payload_off")
		if on == "" || off == "" {
			return fmt.Errorf("%s without payload_on/payload_off", platform)
		}
		if state != on && state != off && state != "None" {
			return fmt.Errorf("%s state %q is neither %q nor %q", platform, state, on, off)
		}
		return nil
	}
	return fmt.Errorf("platform %q is not modelled by this test", platform)
}

// checkState renders every write to the state topic and checks the scripted
// expectations against the last write of each step.
func (c contractComponent) checkState(wire []publishCall, steps []contractStep, stepEnds []int) {
	c.t.Helper()
	stateTopic := c.str("state_topic")
	if stateTopic == "" {
		c.errorf("no state_topic")
		return
	}
	writes := 0
	var last *publishCall
	start := 0
	for i := 0; i <= len(steps); i++ {
		end := len(wire)
		if i < len(steps) {
			end = stepEnds[i]
		}
		name := "after the steps"
		if i < len(steps) {
			name = steps[i].name
		}
		for j := start; j < end; j++ {
			if wire[j].topic != stateTopic {
				continue
			}
			writes++
			last = &wire[j]
			state, err := c.render("value_template", wire[j].payload)
			if err != nil {
				c.errorf("step %q: value_template on %s: %v", name, wire[j].payload, err)
				continue
			}
			if err := c.accepts(state); err != nil {
				c.errorf("step %q: %s renders %q: %v", name, wire[j].payload, state, err)
			}
		}
		start = end
		if i == len(steps) {
			break
		}
		want, scripted := steps[i].want[c.key]
		if !scripted {
			continue
		}
		if last == nil {
			c.errorf("step %q: nothing written, want the state %q", name, want)
			continue
		}
		if got, err := c.render("value_template", last.payload); err != nil || got != want {
			c.errorf("step %q: state after %s = %q (%v), want %q", name, last.payload, got, err, want)
		}
	}
	if writes == 0 {
		c.errorf("the publish path never wrote %s", stateTopic)
	}
}

// checkAvailability renders every write to each availability topic and
// requires the payload that means what was published — and both of them
// over the scenario.
func (c contractComponent) checkAvailability(wire []publishCall) {
	c.t.Helper()
	entries, _ := c.body["availability"].([]any)
	if len(entries) == 0 {
		c.errorf("no availability list")
		return
	}
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		topic, _ := e["topic"].(string)
		tmpl, _ := e["value_template"].(string)
		yes, _ := e["payload_available"].(string)
		no, _ := e["payload_not_available"].(string)
		if yes == "" {
			yes = "online" // DEFAULT_PAYLOAD_AVAILABLE
		}
		if no == "" {
			no = "offline"
		}
		seen := map[bool]bool{}
		for _, rec := range wire {
			if rec.topic != topic {
				continue
			}
			got := strings.TrimSpace(string(rec.payload))
			if tmpl != "" {
				var err error
				if got, err = renderJinja(tmpl, string(rec.payload)); err != nil {
					c.errorf("availability %s on %q: %v", topic, rec.payload, err)
					continue
				}
			}
			want := availableFor(topic, rec.payload)
			switch {
			case got == yes && want:
				seen[true] = true
			case got == no && !want:
				seen[false] = true
			default:
				c.errorf("availability %s on %q renders %q, want %q", topic, rec.payload, got, map[bool]string{true: yes, false: no}[want])
			}
		}
		if !seen[true] || !seen[false] {
			c.errorf("availability %s: the scenario reached available=%v unavailable=%v, want both", topic, seen[true], seen[false])
		}
	}
}

// availableFor is what an availability payload means by the convention:
// `<name>/connected` at 2, or an `online` status object carrying true.
func availableFor(topic string, payload []byte) bool {
	if strings.HasSuffix(topic, "/connected") {
		return strings.TrimSpace(string(payload)) == "2"
	}
	var item struct {
		Val any `json:"val"`
	}
	_ = json.Unmarshal(payload, &item)
	return item.Val == true
}

// command sends what Home Assistant would publish for state through the
// command_template, the real router and the set handler, and returns the
// writes the write path then made.
func (c contractComponent) command(coord *Coordinator, reader *stubReader, stub *stubMQTT, state string) []writeCall {
	c.t.Helper()
	payload := state
	if tmpl := c.str("command_template"); tmpl != "" {
		var err error
		if payload, err = renderJinja(tmpl, state); err != nil {
			c.errorf("command_template on %q: %v", state, err)
			return nil
		}
	}
	before := len(reader.snapshotWrites())
	stub.deliver(c.str("command_topic"), []byte(payload))
	coord.commands.WaitIdle()
	for {
		select {
		case req := <-coord.writeQueue:
			if err := coord.dispatchWrite(context.Background(), req.mqttKey, req.value); err != nil {
				c.errorf("write of %q (from %q): %v", req.value, payload, err)
			}
			continue
		default:
		}
		break
	}
	return reader.snapshotWrites()[before:]
}

// checkCommand round-trips what the entity shows back to the register.
func (c contractComponent) checkCommand(coord *Coordinator, reader *stubReader, stub *stubMQTT, wire []publishCall) {
	c.t.Helper()
	if c.str("command_topic") == "" {
		return
	}
	key := c.mqttKey()
	switch c.str("platform") {
	case "select":
		reg := coord.deps.Catalog.FindByMQTT(key)
		if reg == nil {
			c.errorf("no register")
			return
		}
		for _, code := range enumCodes(reg) {
			token := wireValue(reg, code, nil, coord.deps.Cfg.RoundFloat)
			obj, _ := json.Marshal(map[string]any{"val": token})
			option, err := c.render("value_template", obj)
			if err != nil || !slices.Contains(c.options(), option) {
				c.errorf("code %d (token %v) renders %q (%v), not an option", code, token, option, err)
				continue
			}
			writes := c.command(coord, reader, stub, option)
			if len(writes) != 1 {
				c.errorf("option %q made %d writes, want 1", option, len(writes))
				continue
			}
			got, ok := reg.CodeForLabel(writes[0].value)
			if writes[0].mqttKey != key || !ok || got != code {
				c.errorf("option %q wrote %+v (code %d, %v), want code %d", option, writes[0], got, ok, code)
			}
		}
	case "switch":
		on := c.command(coord, reader, stub, c.str("payload_on"))
		off := c.command(coord, reader, stub, c.str("payload_off"))
		if len(on) != 1 || len(off) != 1 || on[0].mqttKey != off[0].mqttKey || off[0].value != "0" || on[0].value == "0" {
			c.errorf("payload_on wrote %+v, payload_off wrote %+v; want one write each, off 0 and on not", on, off)
		}
	case "number":
		reg := coord.deps.Catalog.FindByMQTT(key)
		if reg == nil {
			c.errorf("no register")
			return
		}
		checked := 0
		for _, rec := range wire {
			if rec.topic != c.str("state_topic") {
				continue
			}
			shown, err := c.render("value_template", rec.payload)
			if err != nil || shown == "None" {
				continue
			}
			f, err := strconv.ParseFloat(shown, 64)
			if err != nil {
				continue // already reported by checkState
			}
			// number.py async_set_native_value: an integral float goes out
			// as an int.
			sent := strconv.FormatFloat(f, 'f', -1, 64)
			want, err := modbus.ParseWriteValue(sent, reg.Scale)
			if err != nil {
				c.errorf("value %q: the write path refuses %q: %v", shown, sent, err)
				continue
			}
			writes := c.command(coord, reader, stub, sent)
			if len(writes) != 1 || writes[0].mqttKey != key {
				c.errorf("value %q made writes %+v, want one to %s", sent, writes, key)
				continue
			}
			if got, err := modbus.ParseWriteValue(writes[0].value, reg.Scale); err != nil || got != want {
				c.errorf("value %q wrote %q = word %d (%v), want word %d", sent, writes[0].value, got, err, want)
			}
			var item struct {
				Val float64 `json:"val"`
			}
			if json.Unmarshal(rec.payload, &item) == nil {
				if read, _ := modbus.ParseWriteValue(strconv.FormatFloat(item.Val, 'f', -1, 64), reg.Scale); read != want {
					c.errorf("value %q goes back as word %d, but %s was read from word %d", sent, want, rec.payload, read)
				}
			}
			checked++
		}
		if checked == 0 {
			c.errorf("no number value was written to round-trip")
		}
	default:
		c.errorf("command_topic on platform %q is not modelled by this test", c.str("platform"))
	}
}
