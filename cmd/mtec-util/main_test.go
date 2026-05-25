// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"bytes"
	"strings"
	"testing"
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
