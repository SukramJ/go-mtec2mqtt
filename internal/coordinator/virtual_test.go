// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// newVirtualCoord builds a minimal coordinator wired with the default
// charge/discharge "active" switches and a recording reader.
func newVirtualCoord(t *testing.T) (*Coordinator, *stubReader) {
	t.Helper()
	reader := newStubReader()
	c := New(Deps{
		Cfg: &config.Config{
			HASSBaseTopic:        "homeassistant",
			Language:             "en",
			ChargeActiveValue:    50,
			DischargeActiveValue: 40,
		},
		Reader:  reader,
		Virtual: hass.DefaultVirtualSwitches(50, 40),
		Now:     func() time.Time { return time.Unix(0, 0) },
	})
	return c, reader
}

func TestApplyVirtualSwitchesDerivesState(t *testing.T) {
	c, _ := newVirtualCoord(t)
	processed := map[string]any{"charge_limit": 50.0, "discharge_limit": 0.0}
	c.applyVirtualSwitches("config", processed)

	if processed["charge_active"] != true {
		t.Errorf("charge_active = %v, want true", processed["charge_active"])
	}
	if processed["discharge_active"] != false {
		t.Errorf("discharge_active = %v, want false", processed["discharge_active"])
	}
	// The non-zero charge limit must be remembered for restore.
	c.lastActiveMu.Lock()
	last := c.lastActive["charge_limit"]
	c.lastActiveMu.Unlock()
	if last != 50 {
		t.Errorf("lastActive[charge_limit] = %v, want 50", last)
	}
}

func TestApplyVirtualSwitchesIgnoresMissingTarget(t *testing.T) {
	c, _ := newVirtualCoord(t)
	processed := map[string]any{} // no limits in this read cycle
	c.applyVirtualSwitches("config", processed)
	if _, ok := processed["charge_active"]; ok {
		t.Error("must not publish a derived state when target is absent")
	}
}

func TestApplyVirtualSwitchesWrongGroupSkipped(t *testing.T) {
	c, _ := newVirtualCoord(t)
	processed := map[string]any{"charge_limit": 50.0}
	c.applyVirtualSwitches("now-base", processed) // switches live in "config"
	if _, ok := processed["charge_active"]; ok {
		t.Error("switch state must only be derived for its own group")
	}
}

func TestVirtualWriteOffThenRestore(t *testing.T) {
	c, reader := newVirtualCoord(t)
	ctx := context.Background()

	// A poll observes an active 50 A charge limit → remembered.
	c.applyVirtualSwitches("config", map[string]any{"charge_limit": 50.0})

	// Turn off: writes 0 to the target.
	if err := c.dispatchWrite(ctx, "charge_active", "0"); err != nil {
		t.Fatal(err)
	}
	// Turn back on: restores the remembered 50.
	if err := c.dispatchWrite(ctx, "charge_active", "1"); err != nil {
		t.Fatal(err)
	}

	writes := reader.snapshotWrites()
	want := []writeCall{{"charge_limit", "0"}, {"charge_limit", "50"}}
	if len(writes) != 2 || writes[0] != want[0] || writes[1] != want[1] {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
}

func TestVirtualWriteOnUsesConfiguredDefault(t *testing.T) {
	c, reader := newVirtualCoord(t)
	ctx := context.Background()

	// No prior poll → fall back to the configured on-values.
	if err := c.dispatchWrite(ctx, "charge_active", "ON"); err != nil {
		t.Fatal(err)
	}
	if err := c.dispatchWrite(ctx, "discharge_active", "true"); err != nil {
		t.Fatal(err)
	}
	writes := reader.snapshotWrites()
	want := []writeCall{{"charge_limit", "50"}, {"discharge_limit", "40"}}
	if len(writes) != 2 || writes[0] != want[0] || writes[1] != want[1] {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
}

func TestDispatchWriteRoutesNormalRegister(t *testing.T) {
	c, reader := newVirtualCoord(t)
	if err := c.dispatchWrite(context.Background(), "mode", "Eco"); err != nil {
		t.Fatal(err)
	}
	writes := reader.snapshotWrites()
	if len(writes) != 1 || writes[0] != (writeCall{"mode", "Eco"}) {
		t.Fatalf("normal write not passed through: %v", writes)
	}
}

func TestRegistersLocalizedWithVirtual(t *testing.T) {
	cat, _, err := registers.LoadFromString(`
"52601":
  name: Charge limit
  name_de: "Ladestrombegrenzung"
  type: U16
  unit: "A"
  writable: true
  mqtt: charge_limit
  group: config
  hass_component_type: number
`)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Deps{
		Cfg: &config.Config{
			HASSBaseTopic:        "homeassistant",
			Language:             "de",
			ChargeActiveValue:    50,
			DischargeActiveValue: 40,
		},
		Catalog: cat,
		Reader:  newStubReader(),
		Virtual: hass.DefaultVirtualSwitches(50, 40),
		Now:     func() time.Time { return time.Unix(0, 0) },
	})

	regs := c.Registers()
	if len(regs) != 3 { // 1 catalog + 2 virtual switches
		t.Fatalf("registers = %d, want 3", len(regs))
	}
	var foundCharge, foundActive bool
	for _, r := range regs {
		if r.MQTT == "charge_limit" {
			foundCharge = true
			if r.Name != "Ladestrombegrenzung" {
				t.Errorf("de catalog name = %q", r.Name)
			}
		}
		if r.MQTT == "charge_active" {
			foundActive = true
			if r.Name != "Laden aktiv" {
				t.Errorf("de virtual name = %q", r.Name)
			}
			if r.Component != "switch" || !r.Writable {
				t.Errorf("virtual switch meta wrong: %+v", r)
			}
		}
	}
	if !foundCharge || !foundActive {
		t.Errorf("missing entries: charge=%v active=%v", foundCharge, foundActive)
	}
}

func TestParseSwitchPayload(t *testing.T) {
	on := []string{"1", "ON", "on", "true", "5", "50"}
	off := []string{"0", "OFF", "off", "false", ""}
	for _, s := range on {
		if v, err := parseSwitchPayload(s); err != nil || !v {
			t.Errorf("parse %q = (%v,%v), want (true,nil)", s, v, err)
		}
	}
	for _, s := range off {
		if v, err := parseSwitchPayload(s); err != nil || v {
			t.Errorf("parse %q = (%v,%v), want (false,nil)", s, v, err)
		}
	}
	if _, err := parseSwitchPayload("banana"); err == nil {
		t.Error("garbage payload must error")
	}
}
