// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Command mtec-util is an interactive CLI for poking at M-TEC
// Energybutler Modbus registers.
//
// Use it to inspect the register catalog, read a single register or a
// whole group from a live inverter, or push a value to a writable
// register — handy for diagnosing wiring before the daemon is set up
// and for one-off interventions afterwards.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/SukramJ/go-mtec2mqtt/internal/config"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus/protocol"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
	"github.com/SukramJ/go-mtec2mqtt/internal/version"
)

const registersFilename = "registers.yaml"

func main() {
	configPath := flag.String("config", "",
		"explicit config.yaml path (defaults to the standard search order)")
	registersPath := flag.String("registers", "",
		"explicit registers.yaml path (defaults next to the binary)")
	showVersion := flag.Bool("version", false, "print build info and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	// Quieter slog default — the menu output is the user-facing surface.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn})))

	if err := run(*configPath, *registersPath, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mtec-util:", err)
		os.Exit(1)
	}
}

// run is the testable entry — pass deterministic in/out for unit
// tests of the menu loop. Production main wires up Stdin/Stdout.
func run(configPath, registersPath string, in io.Reader, out io.Writer) error {
	catalog, err := loadCatalog(registersPath)
	if err != nil {
		return err
	}

	// Modbus is optional: list/help options work fully offline. Building
	// the client here is cheap (no I/O — [modbus.New] never dials); the
	// actual TCP connect is deferred until a menu option that needs it
	// (see session.ensureConnected), so the menu itself never blocks.
	// resolveModbusConfig applies the same config.yaml → MTEC_*
	// environment fallback the daemon uses, so mtec-util keeps working
	// as the same image's diagnostic tool even when no config.yaml is
	// shipped (HA add-on / env-only `docker run`).
	modbusCfg := resolveModbusConfig(configPath, out)
	var client *modbus.Client
	if modbusCfg != nil {
		client = modbus.New(*modbusCfg)
	}
	if client != nil {
		defer func() { _ = client.Close() }()
	}

	app := &session{
		out:       bufio.NewWriter(out),
		in:        bufio.NewScanner(in),
		catalog:   catalog,
		modbusCfg: modbusCfg,
		client:    client,
		reader:    optionalReader(client, catalog),
	}
	app.in.Buffer(make([]byte, 0, 8*1024), 64*1024)
	app.loop()
	return nil
}

// session bundles the per-invocation state for the interactive menu.
type session struct {
	out       *bufio.Writer
	in        *bufio.Scanner
	catalog   *registers.Map
	modbusCfg *modbus.Config // nil iff client is nil — no usable Modbus config
	client    *modbus.Client // may be nil; not yet connected — see ensureConnected
	reader    *modbus.Reader // may be nil
}

// loop runs the menu until the user picks "x" or EOF on stdin.
func (s *session) loop() {
	for {
		s.println("=====================================")
		s.println("Menu:")
		s.println("  1: List all known registers")
		s.println("  2: List register configuration by groups")
		s.println("  3: Read register group from inverter")
		s.println("  4: Read single register from inverter")
		s.println("  5: Write register to inverter")
		s.println("  x: Exit")
		choice, ok := s.prompt("Please select: ")
		if !ok {
			s.println("")
			return
		}
		switch strings.ToLower(choice) {
		case "1":
			s.listAll()
		case "2":
			s.listByGroup()
		case "3":
			if err := s.readGroup(); err != nil {
				s.printf("error: %v\n", err)
			}
		case "4":
			if err := s.readSingle(); err != nil {
				s.printf("error: %v\n", err)
			}
		case "5":
			if err := s.writeRegister(); err != nil {
				s.printf("error: %v\n", err)
			}
		case "x", "q", "exit", "quit":
			s.println("Bye!")
			return
		default:
			s.printf("unknown option: %q\n", choice)
		}
	}
}

// listAll prints the entire catalog as a delimited table — easy to
// grep / pipe into spreadsheets. Layout mirrors the Python output so
// scripts that consume one work on the other.
func (s *session) listAll() {
	s.println("-------------------------------------")
	s.println("Reg  ; MQTT                            ; Unit ; Mode; Group           ; Name")
	s.println("-----;---------------------------------;------;-----;-----------------;-----")
	for _, r := range sortedByKey(s.catalog) {
		reg := r.Key
		if !r.IsModbus() {
			reg = "" // pseudo-register: no address
		}
		mode := "R"
		if r.Writable {
			mode = "RW"
		}
		s.printf("%-5s; %-31s ; %-4s ; %-3s ; %-15s ; %s\n",
			reg, r.MQTT, r.Unit, mode, r.Group, r.Name)
	}
}

// listByGroup runs listAll but in catalog-group sections.
func (s *session) listByGroup() {
	for _, g := range s.catalog.Groups {
		s.printf("-------------------------------------\nGroup %s:\n", g)
		s.println("Reg  ; MQTT                            ; Unit ; Mode; Name")
		s.println("-----;---------------------------------;------;-----;-----")
		for _, r := range sortedByKey(s.catalog) {
			if r.Group != g {
				continue
			}
			reg := r.Key
			if !r.IsModbus() {
				reg = ""
			}
			mode := "R"
			if r.Writable {
				mode = "RW"
			}
			s.printf("%-5s; %-31s ; %-4s ; %-3s ; %s\n",
				reg, r.MQTT, r.Unit, mode, r.Name)
		}
		s.println("")
	}
}

// readGroup prompts for a group name (or "all") and prints decoded
// values for every register in that group. Requires a live connection.
func (s *session) readGroup() error {
	if s.client == nil {
		return errNoConnection
	}
	groups := append([]string{}, secondaryGroupNames(s.catalog)...)
	s.printf("Groups: %s, all\n", strings.Join(groups, ", "))
	choice, ok := s.prompt("Register group (or RETURN for all): ")
	if !ok {
		return nil
	}
	ctx := context.Background()
	if err := s.ensureConnected(ctx); err != nil {
		return err
	}
	if choice == "" || choice == "all" {
		for _, g := range s.catalog.Groups {
			s.dumpGroup(ctx, g)
		}
		return nil
	}
	s.dumpGroup(ctx, registers.Group(choice))
	return nil
}

// dumpGroup is the per-group worker for readGroup.
func (s *session) dumpGroup(ctx context.Context, g registers.Group) {
	s.printf("Reading group %s ...\n", g)
	data, err := s.reader.ReadGroup(ctx, g)
	if err != nil {
		s.printf("  group %s: %v\n", g, err)
	}
	// The Reader keys its output by MQTT suffix (Name fallback). To
	// also show the address we walk the catalog's group list and
	// look the value up by the same key the Reader emitted.
	for _, r := range s.catalog.ByGroup(g) {
		key := r.MQTT
		if key == "" {
			key = r.Name
		}
		val, ok := data[key]
		if !ok {
			continue
		}
		reg := r.Key
		if !r.IsModbus() {
			reg = ""
		}
		s.printf("  %-5s ; %-30s ; %v %s\n", reg, r.Name, val, r.Unit)
	}
}

// readSingle prompts for a register address (YAML key) and prints the
// decoded value. Pseudo-registers (non-numeric keys) are rejected
// because they cannot be read from the device.
func (s *session) readSingle() error {
	if s.client == nil {
		return errNoConnection
	}
	key, ok := s.prompt("Register: ")
	if !ok || key == "" {
		return nil
	}
	ctx := context.Background()
	if err := s.ensureConnected(ctx); err != nil {
		return err
	}
	val, err := s.reader.ReadRegister(ctx, key)
	if err != nil {
		return err
	}
	reg, ok := s.catalog.ByKey[key]
	if !ok {
		s.printf("  %s = %v\n", key, val)
		return nil
	}
	s.printf("Register %s (%s): %v %s\n", key, reg.Name, val, reg.Unit)
	return nil
}

// writeRegister lists every writable register's current value, then
// prompts the user for an address + value and confirms before pushing
// it to the device. The confirmation step matches the Python tool —
// poking the inverter's settings without a "really?" prompt is a
// recipe for tears.
func (s *session) writeRegister() error {
	if s.client == nil {
		return errNoConnection
	}
	ctx := context.Background()
	if err := s.ensureConnected(ctx); err != nil {
		return err
	}

	s.println("-------------------------------------")
	s.println("Current values of writable registers:")
	s.println("Reg   ; Name                          ; Value  ; Unit")
	s.println("------;-------------------------------;--------;-----")

	for _, r := range sortedByKey(s.catalog) {
		if !r.Writable || !r.IsModbus() {
			continue
		}
		val, err := s.reader.ReadRegister(ctx, r.Key)
		display := "?"
		if err == nil {
			display = fmt.Sprintf("%v", val)
		}
		s.printf("%-5s ; %-30s ; %-6s ; %s\n", r.Key, r.Name, display, r.Unit)
	}
	_ = s.out.Flush()

	key, ok := s.prompt("Register: ")
	if !ok || key == "" {
		return nil
	}
	value, ok := s.prompt("Value: ")
	if !ok {
		return nil
	}
	reg, exists := s.catalog.ByKey[key]
	if !exists {
		return fmt.Errorf("unknown register %q", key)
	}
	if !reg.Writable {
		return fmt.Errorf("register %s (%s) is read-only", key, reg.Name)
	}

	s.println("WARNING: Be careful when writing registers to your inverter!")
	confirm, _ := s.prompt(fmt.Sprintf(
		"Really set register %s (%s) to %q? (y/N) ", key, reg.Name, value,
	))
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		s.println("Write aborted.")
		return nil
	}
	if reg.MQTT != "" {
		// Use the same write path the daemon uses so value_items
		// reverse lookup and scaling stay consistent.
		if err := s.reader.WriteRegisterByMQTT(ctx, reg.MQTT, value); err != nil {
			return err
		}
	} else {
		// Direct write path for registers without an MQTT suffix.
		raw, err := directWriteValue(reg, value)
		if err != nil {
			return err
		}
		if err := s.client.WriteSingleRegister(ctx, reg.Address, raw); err != nil {
			return err
		}
	}
	s.println("OK — value written.")
	return nil
}

// directWriteValue guards and coerces a value for the direct (no MQTT
// suffix) write branch so it matches the daemon write path:
// pseudo-registers are rejected (their Address is 0, so a raw write
// would hit a real Modbus register), and the catalog Scale is applied
// with the same uint16 range check as the daemon — the writable listing
// prints the scale-divided decoded value, so re-entering that value
// must round-trip to the raw value the device expects.
func directWriteValue(reg *registers.Register, value string) (uint16, error) {
	if !reg.IsModbus() {
		return 0, fmt.Errorf("register %s (%s) is a pseudo-register and cannot be written",
			reg.Key, reg.Name)
	}
	raw, err := modbus.ParseWriteValue(value, reg.Scale)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q for register %s: %w", value, reg.Key, err)
	}
	return raw, nil
}

// --- I/O helpers ------------------------------------------------------------

func (s *session) prompt(label string) (string, bool) {
	s.printf("%s", label)
	_ = s.out.Flush()
	if !s.in.Scan() {
		return "", false
	}
	return strings.TrimSpace(s.in.Text()), true
}

func (s *session) println(a string) {
	_, _ = s.out.WriteString(a)
	_, _ = s.out.WriteString("\n")
	_ = s.out.Flush()
}

func (s *session) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.out, format, args...)
	_ = s.out.Flush()
}

// --- setup ------------------------------------------------------------------

var errNoConnection = errors.New(
	"no Modbus connection — pass --config, place a valid config.yaml in the search path, " +
		"or set MTEC_* environment variables (at least MODBUS_IP, MQTT_SERVER, MQTT_TOPIC)",
)

func loadCatalog(explicit string) (*registers.Map, error) {
	path := explicit
	if path == "" {
		path = locateRegisters()
	}
	if path == "" {
		return nil, fmt.Errorf("no %s found (place it next to the binary or pass --registers)",
			registersFilename)
	}
	m, _, err := registers.Load(path)
	return m, err
}

func locateRegisters() string {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), registersFilename))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, registersFilename))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// resolveModbusConfig locates and loads the daemon config and turns it
// into a [modbus.Config] — no TCP I/O happens here, only file/env
// lookup, so it is safe to call unconditionally before the menu is
// shown. On any failure it returns nil and prints a one-line note; the
// menu loop then runs with client == nil, locking out the inverter
// options but still serving the offline catalog views.
//
// Search order mirrors the daemon's loadConfig (cmd/mtec2mqtt/main.go):
// an explicit path (flag) or a located config.yaml is loaded as a file;
// only when neither exists does it fall back to a pure MTEC_*
// environment config, same as the HA add-on / an env-only `docker run`
// that ships no config.yaml at all — mtec-util rides along as that same
// image's diagnostic tool and must keep working in that mode too.
func resolveModbusConfig(explicit string, out io.Writer) *modbus.Config {
	env := config.OSEnv{}
	path := explicit
	if path == "" {
		if located, ok := config.Locate(env); ok {
			path = located
		}
	}

	var cfg *config.Config
	var err error
	if path != "" {
		cfg, err = config.LoadFile(path, env)
		if err != nil {
			_, _ = fmt.Fprintf(out, "note: config %s: %v — read/write menu options disabled\n", path, err)
			return nil
		}
	} else {
		cfg, err = config.Load(strings.NewReader(""), env)
		if err != nil {
			_, _ = fmt.Fprintf(out,
				"note: no config.yaml found and MTEC_* environment alone is not a usable config: %v — read/write menu options disabled\n",
				err)
			return nil
		}
		_, _ = fmt.Fprintln(out, "note: no config.yaml found — using MTEC_* environment variables only")
	}

	return &modbus.Config{
		Host:    cfg.ModbusIP,
		Port:    cfg.ModbusPort,
		UnitID:  cfg.ModbusSlave,
		Timeout: cfg.ModbusTimeoutDuration(),
	}
}

// ensureConnected dials the inverter the first time a menu option
// actually needs a live connection, and reuses the socket afterwards —
// [modbus.Client.Connect] is idempotent (a no-op once connected), so
// repeated calls across menu choices are cheap. Prints a one-line
// notice immediately before dialing: MODBUS_TIMEOUT is configurable up
// to 600s, and a silent multi-minute pause looks like a hang.
func (s *session) ensureConnected(ctx context.Context) error {
	if s.client == nil {
		return errNoConnection
	}
	if s.client.IsConnected() {
		return nil
	}
	s.printf("connecting to %s:%d (timeout %ds)...\n",
		s.modbusCfg.Host, s.modbusCfg.Port, int(s.modbusCfg.Timeout.Seconds()))
	if err := s.client.Connect(ctx); err != nil {
		var exc *protocol.ExceptionError
		if errors.As(err, &exc) {
			s.println("  (inverter responded with an exception — connection up, request rejected)")
		}
		return fmt.Errorf("modbus connect %s:%d: %w", s.modbusCfg.Host, s.modbusCfg.Port, err)
	}
	return nil
}

func optionalReader(c *modbus.Client, m *registers.Map) *modbus.Reader {
	if c == nil {
		return nil
	}
	return modbus.NewReader(c, m)
}

// --- catalog helpers --------------------------------------------------------

// sortedByKey returns the catalog in numeric-then-alphabetical key
// order so the listing output is deterministic and human-friendly:
// addresses ascend, pseudo-registers come last.
func sortedByKey(m *registers.Map) []*registers.Register {
	out := make([]*registers.Register, 0, len(m.All))
	out = append(out, m.All...)
	sort.Slice(out, func(i, j int) bool {
		ai, _ := strconv.Atoi(out[i].Key)
		aj, _ := strconv.Atoi(out[j].Key)
		isNumI := out[i].IsModbus()
		isNumJ := out[j].IsModbus()
		switch {
		case isNumI && isNumJ:
			return ai < aj
		case isNumI:
			return true
		case isNumJ:
			return false
		default:
			return out[i].Key < out[j].Key
		}
	})
	return out
}

// secondaryGroupNames returns every distinct group seen in the catalog
// — used as the hint string when prompting for a read-group choice.
func secondaryGroupNames(m *registers.Map) []string {
	out := make([]string, 0, len(m.Groups))
	for _, g := range m.Groups {
		out = append(out, string(g))
	}
	sort.Strings(out)
	return out
}
