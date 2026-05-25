// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus/protocol"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// buildCatalog returns a small but representative register Map for
// reader tests: two close-together registers in one group (single
// cluster), a far-away register (separate cluster), a writable enum
// (value_items), and a pseudo-register.
func buildCatalog(t *testing.T) *registers.Map {
	t.Helper()
	const yaml = `
"11000":
  name: Grid power
  length: 2
  type: S32
  unit: W
  mqtt: grid_power
  group: now-base

"11006":
  name: Voltage A/B
  length: 1
  type: U16
  unit: V
  scale: 10
  mqtt: ac_voltage_a_b
  group: now-base

"11100":
  name: Inverter status
  length: 1
  type: U16
  mqtt: inverter_status
  group: now-base

"52000":
  name: Operation mode
  length: 1
  type: U16
  writable: true
  mqtt: mode
  group: config
  hass_value_items:
    0: "General"
    1: "Eco"
    2: "Backup"

"52001":
  name: Grid inject limit
  length: 1
  type: U16
  scale: 10
  writable: true
  mqtt: grid_inject_limit
  group: config

"consumption":
  name: Household
  mqtt: consumption
  group: now-base
`
	m, _, err := loadCatalogString(yaml)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// loadCatalogString is a small wrapper so tests can keep yaml literals
// inline without poking at the unexported parse function.
func loadCatalogString(s string) (*registers.Map, []string, error) {
	return registers.LoadFromString(s)
}

// --- ReadGroup --------------------------------------------------------------

func TestReadGroupHappyPathSingleAndSplitClusters(t *testing.T) {
	catalog := buildCatalog(t)

	// now-base has registers at 11000 (len 2), 11006 (len 1), 11100
	// (len 1). 11000..11006 sit in one cluster (Count = 7); 11100 is
	// 94 words away → its own cluster.
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		addr := binary.BigEndian.Uint16(req[1:3])
		count := binary.BigEndian.Uint16(req[3:5])
		switch addr {
		case 11000:
			if count != 7 {
				t.Errorf("first cluster count: got %d, want 7", count)
			}
			vals := make([]uint16, 7)
			vals[0], vals[1] = 0xFFFF, 0xFE0C // S32 -500
			vals[6] = 2300                    // 230.0 V scaled
			return cannedFC03(vals), nil
		case 11100:
			if count != 1 {
				t.Errorf("second cluster count: got %d, want 1", count)
			}
			return cannedFC03([]uint16{2 /* on-grid */}), nil
		}
		t.Errorf("unexpected read addr=%d count=%d", addr, count)
		return nil, &protocol.ExceptionError{ExceptionCode: 2}
	})

	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewReader(c, catalog)
	out, err := r.ReadGroup(context.Background(), registers.GroupBase)
	if err != nil {
		t.Fatal(err)
	}
	if out["grid_power"] != -500 {
		t.Errorf("grid_power: got %v (%T), want -500", out["grid_power"], out["grid_power"])
	}
	if out["ac_voltage_a_b"] != 230.0 {
		t.Errorf("ac_voltage_a_b: got %v (%T), want 230.0", out["ac_voltage_a_b"], out["ac_voltage_a_b"])
	}
	if out["inverter_status"] != 2 {
		t.Errorf("inverter_status: got %v, want 2", out["inverter_status"])
	}
	if _, ok := out["consumption"]; ok {
		t.Error("pseudo-register should be absent from ReadGroup output")
	}
}

func TestReadGroupPartialResultsOnClusterFailure(t *testing.T) {
	catalog := buildCatalog(t)
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		addr := binary.BigEndian.Uint16(req[1:3])
		switch addr {
		case 11000:
			vals := make([]uint16, 7)
			vals[0], vals[1] = 0x0000, 0x0001 // grid_power = 1 W
			return cannedFC03(vals), nil
		case 11100:
			return nil, &protocol.ExceptionError{
				Function:      protocol.FCReadHoldingRegisters,
				ExceptionCode: protocol.ExceptionSlaveDeviceFailure,
			}
		}
		return nil, nil
	})

	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewReader(c, catalog)
	out, err := r.ReadGroup(context.Background(), registers.GroupBase)
	if err == nil {
		t.Fatal("expected joined error for failed cluster")
	}
	if out["grid_power"] != 1 {
		t.Errorf("first cluster should still publish: got %v", out["grid_power"])
	}
	if _, ok := out["inverter_status"]; ok {
		t.Error("second cluster failed; inverter_status must be absent")
	}
}

func TestReadGroupEmpty(t *testing.T) {
	catalog := buildCatalog(t)
	// Use a group with no registers at all.
	out, err := NewReader(nil, catalog).ReadGroup(context.Background(), registers.GroupBattery)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("empty group should return empty map, got %v", out)
	}
}

// --- ReadRegister -----------------------------------------------------------

func TestReadRegisterHappy(t *testing.T) {
	catalog := buildCatalog(t)
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		if binary.BigEndian.Uint16(req[1:3]) != 52000 {
			t.Errorf("wrong addr: %v", req)
		}
		return cannedFC03([]uint16{1}), nil
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewReader(c, catalog)
	v, err := r.ReadRegister(context.Background(), "52000")
	if err != nil || v != 1 {
		t.Fatalf("got %v / %v, want 1", v, err)
	}
}

func TestReadRegisterRejectsPseudo(t *testing.T) {
	r := NewReader(nil, buildCatalog(t))
	_, err := r.ReadRegister(context.Background(), "consumption")
	if !errors.Is(err, ErrPseudoUnsupported) {
		t.Fatalf("want ErrPseudoUnsupported, got %v", err)
	}
}

func TestReadRegisterUnknownKey(t *testing.T) {
	r := NewReader(nil, buildCatalog(t))
	_, err := r.ReadRegister(context.Background(), "99999")
	if !errors.Is(err, ErrUnknownRegister) {
		t.Fatalf("want ErrUnknownRegister, got %v", err)
	}
}

// --- WriteRegisterByMQTT ---------------------------------------------------

func TestWriteRegisterValueItemsReverseLookup(t *testing.T) {
	catalog := buildCatalog(t)
	var captured atomic.Uint32
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		addr := binary.BigEndian.Uint16(req[1:3])
		val := binary.BigEndian.Uint16(req[3:5])
		captured.Store(uint32(val))
		// FC06 echoes the request body.
		return cannedFC06(addr, val), nil
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewReader(c, catalog)
	// "Eco" must reverse-lookup to code 1.
	if err := r.WriteRegisterByMQTT(context.Background(), "mode", "Eco"); err != nil {
		t.Fatal(err)
	}
	if got := captured.Load(); got != 1 {
		t.Fatalf("expected wire value 1 (code for Eco), got %d", got)
	}
}

func TestWriteRegisterAppliesScale(t *testing.T) {
	catalog := buildCatalog(t)
	var captured atomic.Uint32
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		addr := binary.BigEndian.Uint16(req[1:3])
		val := binary.BigEndian.Uint16(req[3:5])
		captured.Store(uint32(val))
		return cannedFC06(addr, val), nil
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewReader(c, catalog)
	// scale=10 on grid_inject_limit → "230.5" → 2305 on the wire.
	if err := r.WriteRegisterByMQTT(context.Background(), "grid_inject_limit", "230.5"); err != nil {
		t.Fatal(err)
	}
	if got := captured.Load(); got != 2305 {
		t.Fatalf("expected wire value 2305 (230.5 * 10), got %d", got)
	}
}

func TestWriteRegisterRejectsReadOnly(t *testing.T) {
	r := NewReader(nil, buildCatalog(t))
	err := r.WriteRegisterByMQTT(context.Background(), "grid_power", "1")
	if !errors.Is(err, ErrNotWritable) {
		t.Fatalf("want ErrNotWritable, got %v", err)
	}
}

func TestWriteRegisterUnknownMQTT(t *testing.T) {
	r := NewReader(nil, buildCatalog(t))
	err := r.WriteRegisterByMQTT(context.Background(), "nonexistent", "1")
	if !errors.Is(err, ErrUnknownRegister) {
		t.Fatalf("want ErrUnknownRegister, got %v", err)
	}
}

func TestWriteRegisterRejectsBadValue(t *testing.T) {
	r := NewReader(nil, buildCatalog(t))
	err := r.WriteRegisterByMQTT(context.Background(), "mode", "not-a-number")
	if !errors.Is(err, ErrValueParse) {
		t.Fatalf("want ErrValueParse, got %v", err)
	}
}

// --- concurrent ReadGroup serialises on the wire --------------------------

func TestReadGroupConcurrentCallersSerialiseOnClient(t *testing.T) {
	catalog := buildCatalog(t)
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		addr := binary.BigEndian.Uint16(req[1:3])
		count := binary.BigEndian.Uint16(req[3:5])
		vals := make([]uint16, count)
		if addr == 11000 {
			vals[0], vals[1] = 0x0000, 0x000A
		}
		return cannedFC03(vals), nil
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewReader(c, catalog)

	const N = 4
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = r.ReadGroup(context.Background(), registers.GroupBase)
		}()
	}
	wg.Wait()
	// We mainly want to assert "did not deadlock / corrupt the wire".
	// Correctness of the result is covered by the earlier tests.
}
