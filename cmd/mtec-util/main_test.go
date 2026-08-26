// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"bytes"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// closedTCPPort returns a TCP port on 127.0.0.1 that is not being
// listened on, so a subsequent Dial to it fails immediately with
// "connection refused" instead of blocking for the full Modbus
// timeout. Reserving then releasing a real listener keeps the port
// grab honest without hard-coding a number that might already be in
// use on the test host.
func closedTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve tcp port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release tcp port: %v", err)
	}
	return port
}

// setEnvOnlyModbusConfig sets the minimal MTEC_* environment variables
// needed for config.Validate to accept a pure environment config (no
// config.yaml on disk anywhere in the search path) — mirrors what the
// HA add-on / an env-only `docker run` provides. host/port/timeout
// drive the Modbus side that mtec-util actually dials; the MQTT_*
// values only exist to satisfy Validate (mtec-util never touches MQTT).
func setEnvOnlyModbusConfig(t *testing.T, host string, port, timeoutSeconds int) {
	t.Helper()
	t.Setenv("MTEC_MODBUS_IP", host)
	t.Setenv("MTEC_MODBUS_PORT", strconv.Itoa(port))
	t.Setenv("MTEC_MODBUS_TIMEOUT", strconv.Itoa(timeoutSeconds))
	t.Setenv("MTEC_MQTT_SERVER", "localhost")
	t.Setenv("MTEC_MQTT_PORT", "1883")
	t.Setenv("MTEC_MQTT_TOPIC", "mtec")
}

// TestEnvOnlyConfigEnablesMenuWithoutConfigFile pins the fix for the
// "mtec-util has no env-only fallback" finding: when no config.yaml is
// found anywhere in the search path, mtec-util must fall back to a
// pure MTEC_*-environment config — like the daemon's loadConfig — and
// keep the read/write menu options enabled, instead of disabling them
// permanently just because no file exists. Only option "1" (catalog
// listing) is exercised here, and it must stay dial-free even though a
// usable Modbus config exists — pinning the lazy-connect fix at the
// same time.
func TestEnvOnlyConfigEnablesMenuWithoutConfigFile(t *testing.T) {
	isolate(t)
	setEnvOnlyModbusConfig(t, "127.0.0.1", closedTCPPort(t), 2)

	in := strings.NewReader("1\nx\n")
	var out bytes.Buffer
	if err := run("", "../../registers.yaml", in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "using MTEC_* environment variables only") {
		t.Errorf("expected env-only fallback notice\n----\n%s\n----", body)
	}
	if strings.Contains(body, "no Modbus connection") {
		t.Errorf("env-only config should have enabled the read/write menu\n----\n%s\n----", body)
	}
	if strings.Contains(body, "connecting to") {
		t.Errorf("catalog-only option must stay fully offline\n----\n%s\n----", body)
	}
}

// TestEnvOnlyConfigLazilyConnectsOnReadOption pins the fix for the
// "blocking connect before the menu is shown" finding: the Modbus dial
// must not happen until a menu option that actually needs it runs, and
// a one-line notice must appear right before it — MODBUS_TIMEOUT is
// configurable up to 600s, and a silent multi-minute pause looks like
// a hang otherwise. The reserved-then-released port guarantees a fast
// "connection refused" instead of a real timeout wait.
func TestEnvOnlyConfigLazilyConnectsOnReadOption(t *testing.T) {
	isolate(t)
	port := closedTCPPort(t)
	setEnvOnlyModbusConfig(t, "127.0.0.1", port, 2)

	// "4" (read single register) → any register key → "x" to exit; the
	// connect attempt happens before the key is even looked up in the
	// catalog, so the key value itself does not matter here.
	in := strings.NewReader("4\n1\nx\n")
	var out bytes.Buffer
	if err := run("", "../../registers.yaml", in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	body := out.String()
	wantConnecting := "connecting to 127.0.0.1:" + strconv.Itoa(port) + " (timeout 2s)"
	if !strings.Contains(body, wantConnecting) {
		t.Errorf("expected connect notice %q\n----\n%s\n----", wantConnecting, body)
	}
	if !strings.Contains(body, "error: modbus connect 127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("expected the connect failure to surface as a menu error\n----\n%s\n----", body)
	}
	if strings.Contains(body, "no Modbus connection") {
		t.Errorf("env-only config should have enabled the read/write menu\n----\n%s\n----", body)
	}
}

// TestVersionFlagPrintsBannerAndExitsZero builds the mtec-util binary
// and runs it with --version. The daemon (cmd/mtec2mqtt) already has a
// --version flag that prints internal/version.String() and exits 0;
// mtec-util previously had none at all ("flag provided but not
// defined"). The flag is parsed straight off os.Args by the top-level
// flag package in main(), so it can only be exercised as a real
// subprocess rather than through the run() test seam.
func TestVersionFlagPrintsBannerAndExitsZero(t *testing.T) {
	// Windows' CreateProcess only resolves executables by their .exe
	// suffix; `go build -o` writes the file verbatim, so the suffix must
	// be part of the name or exec fails with "not found in %PATH%".
	name := "mtec-util-versiontest"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mtec-util --version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "go-mtec2mqtt") {
		t.Errorf("expected version banner containing %q, got %q", "go-mtec2mqtt", out)
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
