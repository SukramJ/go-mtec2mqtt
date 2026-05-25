// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package modbus

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// Reader pulls register data from a [*Client] through the lens of a
// [registers.Map]: clusters of nearby addresses get folded into single
// Modbus reads, and each response is sliced + decoded into the typed
// Go values the coordinator publishes over MQTT.
//
// The reader holds no mutable state of its own — it can be shared
// across goroutines, but the underlying client serialises every wire
// transaction anyway so concurrent calls just queue up.
type Reader struct {
	client  *Client
	catalog *registers.Map
}

// Reader-level errors. Wrap, do not compare with ==.
var (
	// ErrUnknownRegister is returned when the caller references a
	// register key (numeric address or MQTT name) that the catalog
	// does not define.
	ErrUnknownRegister = errors.New("modbus: unknown register")
	// ErrPseudoUnsupported is returned when the caller asks the
	// reader for a pseudo-register — those are computed by the
	// coordinator, not read from the device.
	ErrPseudoUnsupported = errors.New("modbus: pseudo-register cannot be read from device")
	// ErrNotWritable is returned by Write* when the target register
	// is not marked writable in the YAML catalog. Mirrors the Python
	// guard that prevents accidentally pushing a value to a read-only
	// address.
	ErrNotWritable = errors.New("modbus: register is read-only")
	// ErrValueParse is returned when the string value passed to Write*
	// cannot be coerced into an integer Modbus value.
	ErrValueParse = errors.New("modbus: cannot parse value")
)

// NewReader constructs a Reader. The Client should already be connected
// — the Reader will not attempt to dial.
func NewReader(client *Client, catalog *registers.Map) *Reader {
	return &Reader{client: client, catalog: catalog}
}

// ReadGroup reads every Modbus register that belongs to group g and
// returns the decoded values keyed by their MQTT suffix (falling back
// to Register.Name when MQTT is empty — matches the Python
// coordinator's lookup table).
//
// Partial results are returned even when some clusters or decodes
// fail; the error is the joined view of every per-cluster /
// per-register failure. Callers should treat the map and the error as
// independent signals: a non-nil error never invalidates the
// successfully populated entries.
func (r *Reader) ReadGroup(ctx context.Context, g registers.Group) (map[string]any, error) {
	members := r.catalog.ByGroup(g)
	if len(members) == 0 {
		return map[string]any{}, nil
	}
	clusters := registers.Clusterize(members)
	out := make(map[string]any, len(members))
	var errs []error

	for _, c := range clusters {
		raw, err := r.client.ReadHoldingRegisters(ctx, c.Start, c.Count)
		if err != nil {
			errs = append(errs,
				fmt.Errorf("cluster start=%d count=%d: %w", c.Start, c.Count, err))
			continue
		}
		decodeClusterInto(out, c, raw, &errs)
	}

	// Pseudo-registers in the group (e.g. "consumption") have no
	// Modbus address and therefore no entry here — the coordinator
	// computes them after the read. Don't emit a key for them so the
	// caller can distinguish "missing" from "zero".

	return out, errors.Join(errs...)
}

// decodeClusterInto fans a successful raw read out across the
// cluster's members and writes the decoded values into out. Decode
// errors per register are collected but do not abort the cluster.
func decodeClusterInto(out map[string]any, c registers.Cluster, raw []uint16, errs *[]error) {
	for _, m := range c.Members {
		length := m.Length
		if length <= 0 {
			length = 1
		}
		offset := int(m.Address) - int(c.Start)
		end := offset + length
		if offset < 0 || end > len(raw) {
			*errs = append(*errs,
				fmt.Errorf("register %d: offset %d+%d outside cluster (have %d words)",
					m.Address, offset, length, len(raw)))
			continue
		}
		v, err := registers.Decode(m, raw[offset:end])
		if err != nil {
			*errs = append(*errs, fmt.Errorf("register %d: decode: %w", m.Address, err))
			continue
		}
		out[outputKey(m)] = v
	}
}

// outputKey returns the map key the coordinator will see — MQTT
// suffix when set, register Name as the fallback (Python parity).
func outputKey(r *registers.Register) string {
	if r.MQTT != "" {
		return r.MQTT
	}
	return r.Name
}

// ReadRegister reads a single register identified by its catalog key.
// The key is the YAML key — either a numeric Modbus address as a
// string (e.g. "10000") or a pseudo-register name (e.g.
// "consumption"). Pseudo-registers cannot be read from the device and
// return ErrPseudoUnsupported.
func (r *Reader) ReadRegister(ctx context.Context, key string) (any, error) {
	reg, ok := r.catalog.ByKey[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRegister, key)
	}
	if !reg.IsModbus() {
		return nil, fmt.Errorf("%w: %q", ErrPseudoUnsupported, key)
	}
	length := reg.Length
	if length <= 0 {
		length = 1
	}
	raw, err := r.client.ReadHoldingRegisters(ctx, reg.Address, uint16(length))
	if err != nil {
		return nil, err
	}
	return registers.Decode(reg, raw)
}

// WriteRegisterByMQTT writes value to the register whose MQTT suffix
// matches name. The value goes through the same coercion pipeline as
// the Python coordinator:
//
//  1. If the register has a value_items map and value matches one of
//     the human-readable labels, the corresponding integer code is
//     used instead (Home Assistant select widgets send the label).
//  2. Numeric value strings are parsed (int wins over float; the float
//     branch then casts to int to match Python's silent narrowing).
//  3. The configured Scale is applied as multiplication (the symmetric
//     inverse of the read-side division).
//  4. The final value is range-checked to fit in uint16 before being
//     handed to the wire.
//
// Returns ErrUnknownRegister, ErrNotWritable, or ErrValueParse for the
// well-defined failure modes; other errors come from the transport.
func (r *Reader) WriteRegisterByMQTT(ctx context.Context, name, value string) error {
	reg := r.catalog.FindByMQTT(name)
	if reg == nil {
		return fmt.Errorf("%w: mqtt=%q", ErrUnknownRegister, name)
	}
	if !reg.Writable {
		return fmt.Errorf("%w: mqtt=%q (register %d)", ErrNotWritable, name, reg.Address)
	}
	if !reg.IsModbus() {
		return fmt.Errorf("%w: mqtt=%q is a pseudo-register", ErrPseudoUnsupported, name)
	}

	// 1) value_items reverse lookup (HA select payload → modbus code)
	if reg.HassValueItems != nil {
		for code, label := range reg.HassValueItems {
			if label == value {
				return r.client.WriteSingleRegister(ctx, reg.Address, uint16(code))
			}
		}
		// Fall through — caller may have sent the numeric code directly.
	}

	// 2/3) numeric parse + scale.
	raw, err := parseWriteValue(value, reg.Scale)
	if err != nil {
		return fmt.Errorf("%w: mqtt=%q value=%q: %v", ErrValueParse, name, value, err)
	}
	return r.client.WriteSingleRegister(ctx, reg.Address, raw)
}

// parseWriteValue mirrors the value-coercion logic from
// AsyncModbusClient.write_register: int wins over float, dots flip to
// float-parse, scale multiplies, final cast narrows to uint16.
func parseWriteValue(s string, scale int) (uint16, error) {
	if scale < 1 {
		scale = 1
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		v := i * int64(scale)
		if v < 0 || v > 0xFFFF {
			return 0, fmt.Errorf("scaled value %d out of uint16 range", v)
		}
		return uint16(v), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	scaled := f * float64(scale)
	if scaled < 0 || scaled > 0xFFFF {
		return 0, fmt.Errorf("scaled value %v out of uint16 range", scaled)
	}
	return uint16(scaled), nil
}
