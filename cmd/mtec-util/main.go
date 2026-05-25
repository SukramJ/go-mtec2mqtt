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
)

const registersFilename = "registers.yaml"

func main() {
	configPath := flag.String("config", "",
		"explicit config.yaml path (defaults to the standard search order)")
	registersPath := flag.String("registers", "",
		"explicit registers.yaml path (defaults next to the binary)")
	flag.Parse()

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

	// Modbus is optional: list/help options work without it. Only
	// the read/write paths need a live connection.
	client := openModbus(configPath, out)
	if client != nil {
		defer func() { _ = client.Close() }()
	}

	app := &session{
		out:     bufio.NewWriter(out),
		in:      bufio.NewScanner(in),
		catalog: catalog,
		client:  client,
		reader:  optionalReader(client, catalog),
	}
	app.in.Buffer(make([]byte, 0, 8*1024), 64*1024)
	return app.loop()
}

// session bundles the per-invocation state for the interactive menu.
type session struct {
	out     *bufio.Writer
	in      *bufio.Scanner
	catalog *registers.Map
	client  *modbus.Client // may be nil
	reader  *modbus.Reader // may be nil
}

// loop runs the menu until the user picks "x" or EOF on stdin.
func (s *session) loop() error {
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
			return nil
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
			return nil
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
	if s.reader == nil {
		return errNoConnection
	}
	groups := append([]string{}, secondaryGroupNames(s.catalog)...)
	s.printf("Groups: %s, all\n", strings.Join(groups, ", "))
	choice, ok := s.prompt("Register group (or RETURN for all): ")
	if !ok {
		return nil
	}
	ctx := context.Background()
	if choice == "" || choice == "all" {
		for _, g := range s.catalog.Groups {
			s.dumpGroup(ctx, registers.Group(g))
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
	if s.reader == nil {
		return errNoConnection
	}
	key, ok := s.prompt("Register: ")
	if !ok || key == "" {
		return nil
	}
	val, err := s.reader.ReadRegister(context.Background(), key)
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
	if s.reader == nil || s.client == nil {
		return errNoConnection
	}

	s.println("-------------------------------------")
	s.println("Current values of writable registers:")
	s.println("Reg   ; Name                          ; Value  ; Unit")
	s.println("------;-------------------------------;--------;-----")

	ctx := context.Background()
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
	s.out.Flush()

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
		v, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return fmt.Errorf("invalid uint16 %q: %v", value, err)
		}
		if err := s.client.WriteSingleRegister(ctx, reg.Address, uint16(v)); err != nil {
			return err
		}
	}
	s.println("OK — value written.")
	return nil
}

// --- I/O helpers ------------------------------------------------------------

func (s *session) prompt(label string) (string, bool) {
	s.printf("%s", label)
	s.out.Flush()
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
	"no Modbus connection — pass --config or place a valid config.yaml in the search path",
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

// openModbus tries to load the daemon config and dial the inverter.
// On any failure it returns nil and prints a one-line warning — the
// menu loop runs with reader == nil, locking out the inverter
// options but still serving the offline catalog views.
func openModbus(explicit string, out io.Writer) *modbus.Client {
	path := explicit
	if path == "" {
		var ok bool
		path, ok = config.Locate(config.OSEnv{})
		if !ok {
			fmt.Fprintln(out, "note: no config.yaml found — read/write menu options disabled")
			return nil
		}
	}
	cfg, err := config.LoadFile(path, config.OSEnv{})
	if err != nil {
		fmt.Fprintf(out, "note: config %s: %v — read/write menu options disabled\n", path, err)
		return nil
	}
	client := modbus.New(modbus.Config{
		Host:    cfg.ModbusIP,
		Port:    cfg.ModbusPort,
		UnitID:  cfg.ModbusSlave,
		Timeout: cfg.ModbusTimeoutDuration(),
	})
	if err := client.Connect(context.Background()); err != nil {
		// Don't fail outright — listing options still work. The
		// menu will refuse 3/4/5 with errNoConnection.
		fmt.Fprintf(out, "note: modbus connect %s:%d: %v\n", cfg.ModbusIP, cfg.ModbusPort, err)
		var exc *protocol.ExceptionError
		if errors.As(err, &exc) {
			fmt.Fprintln(out, "  (inverter responded with an exception — connection up, request rejected)")
		}
		return nil
	}
	return client
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
