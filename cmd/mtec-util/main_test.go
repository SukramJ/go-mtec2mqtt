// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// isolate steers config.Locate at the empty temp dir so the test
// does not pick up the developer's real ~/.config/aiomtec2mqtt/config.yaml
// and dial the actual inverter. Call from every test that does NOT
// explicitly want a Modbus connection.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
}

// TestListAllOptionExitsCleanly drives the menu with stdin scripted
// to pick "1" then "x", confirms the catalog listing showed up and
// the loop exited. The interactive parts of mtec-util are otherwise
// hard to cover end-to-end; this pins the I/O wiring.
func TestListAllOptionExitsCleanly(t *testing.T) {
	isolate(t)
	in := strings.NewReader("1\nx\n")
	var out bytes.Buffer
	if err := run("", "../../registers.yaml", in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		"List all known registers",
		"Inverter serial number",
		"Grid power",
		"Bye!",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q\n----\n%s\n----", want, body)
		}
	}
}

func TestListByGroupOption(t *testing.T) {
	isolate(t)
	in := strings.NewReader("2\nx\n")
	var out bytes.Buffer
	if err := run("", "../../registers.yaml", in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		"Group static:",
		"Group now-base:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q\n----\n%s\n----", want, body)
		}
	}
}

// TestReadGroupWithoutConnectionReportsError exercises option 3 with
// no config (Modbus unavailable) and asserts a graceful error rather
// than a crash.
func TestReadGroupWithoutConnectionReportsError(t *testing.T) {
	isolate(t)
	in := strings.NewReader("3\nx\n")
	var out bytes.Buffer
	if err := run("", "../../registers.yaml", in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "no Modbus connection") {
		t.Errorf("expected no-connection notice in output\n----\n%s\n----", body)
	}
}

func TestUnknownChoiceLoopsBack(t *testing.T) {
	isolate(t)
	in := strings.NewReader("?\nx\n")
	var out bytes.Buffer
	if err := run("", "../../registers.yaml", in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, `unknown option: "?"`) {
		t.Errorf("expected unknown-option warning\n----\n%s\n----", body)
	}
}

func TestEOFOnStdinExitsCleanly(t *testing.T) {
	isolate(t)
	// No newline → Scan returns false on first prompt.
	in := strings.NewReader("")
	var out bytes.Buffer
	if err := run("", "../../registers.yaml", in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestDirectWriteValueRejectsPseudoRegister pins the guard on the
// direct (no MQTT suffix) write branch: a pseudo-register has Address 0,
// so an unguarded write would target a real Modbus register on the
// device. Reachable when an operator marks a pseudo-register writable
// in the operator-editable registers.yaml.
func TestDirectWriteValueRejectsPseudoRegister(t *testing.T) {
	reg := &registers.Register{
		Key:      "consumption",
		Name:     "Consumption",
		Writable: true, // operator-edited catalog
	}
	if _, err := directWriteValue(reg, "42"); err == nil {
		t.Fatal("expected pseudo-register write to be rejected")
	} else if !strings.Contains(err.Error(), "pseudo-register") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDirectWriteValueAppliesScale pins the display/write round-trip:
// the writable listing shows the scale-divided decoded value, so
// re-entering the displayed value must be multiplied by Scale before
// hitting the wire — exactly like the daemon's WriteRegisterByMQTT path.
func TestDirectWriteValueAppliesScale(t *testing.T) {
	cases := []struct {
		name    string
		scale   int
		value   string
		want    uint16
		wantErr bool
	}{
		{"unscaled", 0, "42", 42, false},
		{"scaled int", 10, "25", 250, false},
		{"scaled float", 10, "23.5", 235, false},
		{"scaled out of range", 10, "6554", 0, true},
		{"garbage", 1, "not-a-number", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &registers.Register{
				Key:      "52605",
				Address:  52605,
				Name:     "Experimental limit",
				Scale:    tc.scale,
				Writable: true,
			}
			got, err := directWriteValue(reg, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("directWriteValue(%q) = %d, want error", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("directWriteValue(%q): %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("directWriteValue(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestSortedByKeyPutsPseudoRegistersLast(t *testing.T) {
	catalog, err := loadCatalog("../../registers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := sortedByKey(catalog)
	// Find the boundary between modbus and pseudo registers.
	seenPseudo := false
	for _, r := range out {
		if r.IsModbus() {
			if seenPseudo {
				t.Fatal("modbus register appeared after a pseudo register — sort order broken")
			}
		} else {
			seenPseudo = true
		}
	}
	// And the addresses inside the modbus segment must be ascending.
	prev := -1
	for _, r := range out {
		if !r.IsModbus() {
			break
		}
		if int(r.Address) < prev {
			t.Fatalf("addresses not ascending: %d after %d", r.Address, prev)
		}
		prev = int(r.Address)
	}
}
