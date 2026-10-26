// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
)

// i18nFS holds this bridge's labels — sections, groups, status and
// configuration tiles — merged over go-bridgeui's built-in bundles.
//
//go:embed i18n
var i18nFS embed.FS

// Config parameterises the web server.
type Config struct {
	// Bind is the listen address "host:port".
	Bind string
	// User / Password enable HTTP Basic auth when either is non-empty.
	User     string
	Password string
	// Language is LANGUAGE: "de" or English.
	Language string
	// Logger is optional; nil → slog.Default().
	Logger *slog.Logger
}

// layout is the M-TEC page: the live groups, the day/total counters, and
// the evcc profile while it is on; configuration and device info under
// System.
var layout = bridgeui.Layout{
	Brand:   "mtec2mqtt",
	Product: "M-TEC Energybutler",
	Icon:    "⚡",
	Sections: []bridgeui.Section{
		{ID: "live", Title: "section.live", Nav: "nav.live", Groups: []string{
			"now_base", "now_grid", "now_inverter", "now_backup", "now_battery", "now_pv",
		}},
		{ID: "energy", Title: "section.energy", Nav: "nav.energy", Groups: []string{"day", "total"}},
		{ID: "evcc", Title: "section.evcc", Nav: "nav.evcc", Groups: []string{"evcc"}, Optional: true},
	},
	SystemGroups: []string{"config", "static"},
}

// New builds the dashboard over backend. It does not bind a socket — call
// Run on the result.
func New(cfg Config, backend Backend) *bridgeui.Server {
	tr, err := bridgeui.LoadTranslations(i18nFS, "i18n")
	if err != nil {
		// The bundles are embedded and covered by a test.
		panic("web: " + err.Error())
	}
	return bridgeui.New(bridgeui.Options{
		Bind:         cfg.Bind,
		User:         cfg.User,
		Password:     cfg.Password,
		Realm:        "mtec2mqtt",
		Logger:       cfg.Logger,
		Language:     cfg.Language,
		Layout:       layout,
		Translations: tr,
	}, &adapter{b: backend})
}

// adapter presents the coordinator's state as go-bridgeui's contract: one
// device, the inverter.
type adapter struct {
	b Backend
}

// deviceID names the inverter in the UI until its serial is known.
const deviceID = "inverter"

// registers indexes the catalog by the key snapshot values carry: the MQTT
// suffix, or the name for a register without one.
func (a *adapter) registers() map[string]state.RegisterInfo {
	regs := a.b.Registers()
	out := make(map[string]state.RegisterInfo, len(regs))
	for i := range regs {
		out[outputKey(&regs[i])] = regs[i]
	}
	return out
}

func outputKey(r *state.RegisterInfo) string {
	if r.MQTT != "" {
		return r.MQTT
	}
	return r.Name
}

func (a *adapter) Snapshot() bridgeui.Snapshot {
	snap := a.b.Snapshot()
	regs := a.registers()
	dev := bridgeui.Device{
		ID:             firstNonEmpty(snap.Serial, deviceID),
		Title:          "M-TEC Inverter",
		Subtitle:       subtitle(snap.Serial, snap.Firmware),
		GroupUpdatedAt: make(map[string]time.Time, len(snap.Groups)),
		Entries:        []bridgeui.Entry{},
	}
	if snap.Equipment != "" {
		dev.Title = "M-TEC " + snap.Equipment
	}
	for group, view := range snap.Groups {
		dev.GroupUpdatedAt[group] = view.UpdatedAt
		if view.UpdatedAt.After(dev.UpdatedAt) {
			dev.UpdatedAt = view.UpdatedAt
		}
		for key, value := range view.Values {
			reg := regs[key]
			dev.Entries = append(dev.Entries, bridgeui.Entry{
				Group: group, Key: key, Name: reg.Name, Unit: reg.Unit, Value: value, Tone: valueTone(value, reg),
			})
		}
	}
	return bridgeui.Snapshot{Devices: []bridgeui.Device{dev}}
}

// valueTone colours the inverter's status enums: "OK" green, any other
// known state amber.
func valueTone(value any, reg state.RegisterInfo) string {
	s, ok := value.(string)
	switch {
	case !ok:
		return ""
	case s == "OK":
		return "ok"
	case len(reg.ValueItems) > 0 && s != "Unknown":
		return "warn"
	}
	return ""
}

func (a *adapter) Health() bridgeui.Health {
	h := a.b.Health()
	modbusTile := bridgeui.Tile{Label: "health.modbus", Text: "val.disconnected", Tone: "err", Sub: h.ModbusAddr}
	if h.ModbusConnected {
		modbusTile.Text, modbusTile.Tone = "val.connected", "ok"
	}
	hass := bridgeui.Tile{Label: "health.hass", Text: "val.off"}
	if h.HASSEnabled {
		hass.Text, hass.Tone = "val.active", "ok"
	}
	return bridgeui.Health{
		Status:    h.Status,
		Version:   h.Version,
		StartedAt: h.StartedAt,
		Tiles: []bridgeui.Tile{
			modbusTile,
			{Label: "health.mqtt", Value: firstNonEmpty(h.MQTTServer, "—")},
			{Label: "health.topic", Value: firstNonEmpty(h.MQTTTopic, "—")},
			hass,
			{Label: "health.serial", Value: firstNonEmpty(h.Serial, "—")},
			{Label: "health.firmware", Value: firstNonEmpty(h.Firmware, "—")},
		},
	}
}

func (a *adapter) Config() []bridgeui.Tile {
	c := a.b.Config()
	seconds := func(label string, s int) bridgeui.Tile {
		return bridgeui.Tile{Label: label, Value: strconv.Itoa(s) + " s"}
	}
	onOff := func(label string, on bool, text string) bridgeui.Tile {
		if on {
			return bridgeui.Tile{Label: label, Text: text}
		}
		return bridgeui.Tile{Label: label, Text: "val.off"}
	}
	hass := onOff("cfg.hass", c.HASSEnable, "val.active")
	hass.Sub = c.HASSBaseTopic
	return []bridgeui.Tile{
		{Label: "cfg.modbus", Value: c.ModbusIP + ":" + strconv.Itoa(c.ModbusPort), Sub: "Slave " + strconv.Itoa(c.ModbusSlave)},
		{Label: "cfg.mqtt", Value: c.MQTTServer + ":" + strconv.Itoa(c.MQTTPort), Sub: c.MQTTTopic},
		hass,
		seconds("cfg.refresh_now", c.RefreshNow),
		seconds("cfg.refresh_config", c.RefreshConfig),
		seconds("cfg.refresh_day", c.RefreshDay),
		seconds("cfg.refresh_total", c.RefreshTotal),
		seconds("cfg.refresh_static", c.RefreshStatic),
		onOff("cfg.evcc", c.EVCCEnable, "val.active"),
		onOff("cfg.debug", c.Debug, "val.on"),
	}
}

// Items lists the writable registers and the virtual switches. A select
// writes its label: the write path maps it back to the code.
func (a *adapter) Items() []bridgeui.Item {
	var out []bridgeui.Item
	regs := a.b.Registers()
	for i := range regs {
		r := &regs[i]
		if !r.Writable {
			continue
		}
		it := bridgeui.Item{Key: outputKey(r), Name: r.Name, Unit: r.Unit, Component: r.Component}
		if len(r.ValueItems) > 0 {
			it.Component = "select"
			codes := make([]int, 0, len(r.ValueItems))
			for code := range r.ValueItems {
				codes = append(codes, code)
			}
			slices.Sort(codes)
			for _, code := range codes {
				label := r.ValueItems[code]
				it.Options = append(it.Options, bridgeui.Option{Value: label, Label: label})
			}
		} else if it.Component != "switch" {
			it.Component = "number"
		}
		out = append(out, it)
	}
	return out
}

// Write ignores the device — there is one — and classes the transport's
// errors for go-bridgeui.
func (a *adapter) Write(ctx context.Context, _, key, value string) error {
	err := a.b.Write(ctx, key, value)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, modbus.ErrUnknownRegister), errors.Is(err, modbus.ErrNotWritable):
		return fmt.Errorf("%w: %w", bridgeui.ErrNotWritable, err)
	case errors.Is(err, modbus.ErrValueParse), errors.Is(err, modbus.ErrPseudoUnsupported):
		return fmt.Errorf("%w: %w", bridgeui.ErrInvalidValue, err)
	case errors.Is(err, modbus.ErrNotConnected):
		return fmt.Errorf("%w: %w", bridgeui.ErrNotReady, err)
	}
	return err
}

func (a *adapter) Changes() (events <-chan struct{}, cancel func()) { return a.b.Changes() }

// subtitle is "SN … · FW …" for what is known.
func subtitle(serial, firmware string) string {
	var parts []string
	if serial != "" {
		parts = append(parts, "SN "+serial)
	}
	if firmware != "" {
		parts = append(parts, "FW "+firmware)
	}
	return strings.Join(parts, "  ·  ")
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
