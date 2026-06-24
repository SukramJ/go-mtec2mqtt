// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
)

// This file implements the synthetic charge/discharge "active" switches.
// They have no register of their own: their on/off state is derived from
// a target register (charge_limit / discharge_limit) on every config
// poll, and toggling one writes that register between a remembered value
// and 0. The last non-zero value seen on the target is remembered so the
// switch restores it on the next "on"; the configured value is the
// fallback when nothing has been seen yet.

// dispatchWrite routes a write either to the synthetic-switch handler or
// to the normal Modbus register write. Used by both the HA command queue
// and the web UI so the two paths behave identically.
func (c *Coordinator) dispatchWrite(ctx context.Context, mqttKey, value string) error {
	if v, ok := c.virtualByKey[mqttKey]; ok {
		return c.handleVirtualWrite(ctx, v, value)
	}
	return c.deps.Reader.WriteRegisterByMQTT(ctx, mqttKey, value)
}

// handleVirtualWrite translates an on/off command for a synthetic switch
// into a write of its target register: "on" restores the last active
// value (or the configured fallback), "off" writes 0.
func (c *Coordinator) handleVirtualWrite(ctx context.Context, v hass.VirtualSwitch, value string) error {
	on, err := parseSwitchPayload(value)
	if err != nil {
		return err
	}
	target := "0"
	if on {
		target = c.activeTargetValue(v)
	}
	return c.deps.Reader.WriteRegisterByMQTT(ctx, v.TargetMQTT, target)
}

// activeTargetValue is the value written to the target register when the
// switch is turned on: the last non-zero value seen on that register, or
// the configured fallback when none has been observed yet.
func (c *Coordinator) activeTargetValue(v hass.VirtualSwitch) string {
	c.lastActiveMu.Lock()
	last := c.lastActive[v.TargetMQTT]
	c.lastActiveMu.Unlock()
	val := last
	if val <= 0 {
		val = float64(v.OnValue)
	}
	return strconv.FormatFloat(val, 'f', -1, 64)
}

// applyVirtualSwitches derives the on/off state of every synthetic
// switch in the given group from the processed register values and adds
// it under the switch's key, so it rides the normal store + MQTT publish
// path. It also remembers the last non-zero target value for restore.
func (c *Coordinator) applyVirtualSwitches(group string, processed map[string]any) {
	for _, v := range c.virtualByKey {
		if v.Group != group {
			continue
		}
		raw, ok := processed[v.TargetMQTT]
		if !ok {
			// Target not part of this read cycle — leave the state alone
			// rather than publish a stale or guessed value.
			continue
		}
		val, ok := toFloat(raw)
		if !ok {
			continue
		}
		if val > 0 {
			c.lastActiveMu.Lock()
			c.lastActive[v.TargetMQTT] = val
			c.lastActiveMu.Unlock()
		}
		processed[v.Key] = val > 0
	}
}

// parseSwitchPayload interprets an HA / web switch command. It accepts
// the canonical "1"/"0" the discovery payload advertises plus the common
// textual and numeric variants; any non-zero number counts as on.
func parseSwitchPayload(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "on", "true":
		return true, nil
	case "0", "off", "false", "":
		return false, nil
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return f != 0, nil
	}
	return false, fmt.Errorf("coordinator: invalid switch payload %q: %w", s, modbus.ErrValueParse)
}

// toFloat coerces the numeric forms the decoder emits to float64.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}
