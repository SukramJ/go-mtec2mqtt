// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package protocol implements the Modbus-TCP (MBAP) wire format that
// the M-TEC Energybutler speaks on its espressif gateway.
//
// Scope is intentionally narrow: only the function codes the inverter
// actually uses (FC03 Read-Holding-Registers, FC06 Write-Single-Register)
// plus their exception responses. Everything else is out of scope.
//
// The encoders/decoders are pure functions that operate on byte slices;
// I/O lives one layer up. The complete byte layout is validated against
// pymodbus-generated fixtures in mbap_test.go.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// MBAP wire constants.
const (
	HeaderLen     = 7   // tid(2) + protoID(2) + length(2) + unitID(1)
	MaxPDULen     = 253 // Modbus spec §4.1, leaves room for header
	MaxFrameLen   = HeaderLen + MaxPDULen
	maxReadCount  = 125  // FC03 max quantity per spec §6.3
	exceptionMask = 0x80 // function code OR'd with this in exception responses
)

// Function codes the inverter uses.
const (
	FCReadHoldingRegisters byte = 0x03
	FCWriteSingleRegister  byte = 0x06
)

// Exception codes the inverter may return (spec §7).
const (
	ExceptionIllegalFunction        byte = 0x01
	ExceptionIllegalDataAddress     byte = 0x02
	ExceptionIllegalDataValue       byte = 0x03
	ExceptionSlaveDeviceFailure     byte = 0x04
	ExceptionAcknowledge            byte = 0x05
	ExceptionSlaveDeviceBusy        byte = 0x06
	ExceptionMemoryParityError      byte = 0x08
	ExceptionGatewayPathUnavailable byte = 0x0A
	ExceptionGatewayTargetFailed    byte = 0x0B
)

// Codec-level errors. Wrap, don't compare with == — use errors.Is.
var (
	ErrShortFrame      = errors.New("modbus/protocol: frame shorter than MBAP header")
	ErrBadProtocolID   = errors.New("modbus/protocol: protocol-id must be 0")
	ErrLengthMismatch  = errors.New("modbus/protocol: declared length does not match buffer")
	ErrEmptyPDU        = errors.New("modbus/protocol: PDU is empty")
	ErrCountOutOfRange = errors.New("modbus/protocol: register count out of range")
	ErrShortPDU        = errors.New("modbus/protocol: PDU shorter than function-code requires")
	ErrOddByteCount    = errors.New("modbus/protocol: FC03 byte-count must be even")
	ErrWrongFunction   = errors.New("modbus/protocol: PDU function code mismatch")
)

// Header is the parsed MBAP fixed header.
type Header struct {
	TransactionID uint16
	UnitID        byte
	// pduLen is the length the header advertises minus the unit-id byte,
	// i.e. exactly len(pdu). Exposed via Length() for callers that want it.
	pduLen uint16
}

// Length returns the PDU length the header advertises.
func (h Header) Length() int { return int(h.pduLen) }

// ExceptionError represents an exception PDU returned by the device.
// It satisfies the error interface so call sites can `errors.As` it.
type ExceptionError struct {
	Function      byte // original function code (without 0x80 mask)
	ExceptionCode byte
}

// Error implements error.
func (e *ExceptionError) Error() string {
	return fmt.Sprintf("modbus exception: fc=0x%02x code=0x%02x (%s)",
		e.Function, e.ExceptionCode, exceptionName(e.ExceptionCode))
}

func exceptionName(c byte) string {
	switch c {
	case ExceptionIllegalFunction:
		return "illegal function"
	case ExceptionIllegalDataAddress:
		return "illegal data address"
	case ExceptionIllegalDataValue:
		return "illegal data value"
	case ExceptionSlaveDeviceFailure:
		return "slave device failure"
	case ExceptionAcknowledge:
		return "acknowledge"
	case ExceptionSlaveDeviceBusy:
		return "slave device busy"
	case ExceptionMemoryParityError:
		return "memory parity error"
	case ExceptionGatewayPathUnavailable:
		return "gateway path unavailable"
	case ExceptionGatewayTargetFailed:
		return "gateway target failed to respond"
	default:
		return "unknown"
	}
}

// EncodeFrame wraps a PDU in an MBAP header. Returns a freshly allocated
// slice the caller may write directly to a connection.
func EncodeFrame(transactionID uint16, unitID byte, pdu []byte) ([]byte, error) {
	if len(pdu) == 0 {
		return nil, ErrEmptyPDU
	}
	if len(pdu) > MaxPDULen {
		return nil, fmt.Errorf("modbus/protocol: PDU too long (%d > %d)", len(pdu), MaxPDULen)
	}
	out := make([]byte, HeaderLen+len(pdu))
	binary.BigEndian.PutUint16(out[0:2], transactionID)
	// out[2:4] protocol-id stays 0
	binary.BigEndian.PutUint16(out[4:6], uint16(len(pdu)+1)) // +1 for unitID
	out[6] = unitID
	copy(out[HeaderLen:], pdu)
	return out, nil
}

// DecodeHeader parses the 7-byte MBAP header. Does NOT consume the PDU.
func DecodeHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderLen {
		return Header{}, ErrShortFrame
	}
	if binary.BigEndian.Uint16(buf[2:4]) != 0 {
		return Header{}, ErrBadProtocolID
	}
	declared := binary.BigEndian.Uint16(buf[4:6])
	if declared < 2 || declared > MaxPDULen+1 {
		return Header{}, fmt.Errorf("modbus/protocol: declared length %d out of range", declared)
	}
	return Header{
		TransactionID: binary.BigEndian.Uint16(buf[0:2]),
		UnitID:        buf[6],
		pduLen:        declared - 1, // header advertises length INCLUDING unit-id
	}, nil
}

// EncodeReadHoldingRegisters builds an FC03 request PDU.
func EncodeReadHoldingRegisters(address, count uint16) ([]byte, error) {
	if count == 0 || count > maxReadCount {
		return nil, fmt.Errorf("%w: count=%d (must be 1..%d)",
			ErrCountOutOfRange, count, maxReadCount)
	}
	pdu := make([]byte, 5)
	pdu[0] = FCReadHoldingRegisters
	binary.BigEndian.PutUint16(pdu[1:3], address)
	binary.BigEndian.PutUint16(pdu[3:5], count)
	return pdu, nil
}

// EncodeWriteSingleRegister builds an FC06 request PDU.
func EncodeWriteSingleRegister(address, value uint16) []byte {
	pdu := make([]byte, 5)
	pdu[0] = FCWriteSingleRegister
	binary.BigEndian.PutUint16(pdu[1:3], address)
	binary.BigEndian.PutUint16(pdu[3:5], value)
	return pdu
}

// DecodeReadHoldingResponse parses an FC03 response PDU into the
// returned 16-bit register values.
//
// If the PDU is an exception response (function code 0x83), the function
// returns nil and an *ExceptionError that callers can unwrap.
func DecodeReadHoldingResponse(pdu []byte) ([]uint16, error) {
	if err := requireFunction(pdu, FCReadHoldingRegisters); err != nil {
		return nil, err
	}
	if len(pdu) < 2 {
		return nil, ErrShortPDU
	}
	byteCount := int(pdu[1])
	if byteCount%2 != 0 {
		return nil, ErrOddByteCount
	}
	if len(pdu) != 2+byteCount {
		return nil, fmt.Errorf("%w: pdu=%d, byteCount=%d",
			ErrLengthMismatch, len(pdu), byteCount)
	}
	regs := make([]uint16, byteCount/2)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(pdu[2+i*2 : 4+i*2])
	}
	return regs, nil
}

// DecodeWriteSingleResponse parses an FC06 response PDU. Returns the
// echoed address and value or an *ExceptionError.
func DecodeWriteSingleResponse(pdu []byte) (address, value uint16, err error) {
	if err = requireFunction(pdu, FCWriteSingleRegister); err != nil {
		return 0, 0, err
	}
	if len(pdu) != 5 {
		return 0, 0, fmt.Errorf("%w: expected 5, got %d", ErrLengthMismatch, len(pdu))
	}
	return binary.BigEndian.Uint16(pdu[1:3]), binary.BigEndian.Uint16(pdu[3:5]), nil
}

// requireFunction guards a response PDU. Returns an *ExceptionError when
// the high bit of the function code is set, ErrWrongFunction otherwise.
func requireFunction(pdu []byte, want byte) error {
	if len(pdu) == 0 {
		return ErrEmptyPDU
	}
	got := pdu[0]
	switch {
	case got == want:
		return nil
	case got == want|exceptionMask:
		if len(pdu) < 2 {
			return ErrShortPDU
		}
		return &ExceptionError{Function: want, ExceptionCode: pdu[1]}
	default:
		return fmt.Errorf("%w: got 0x%02x, want 0x%02x", ErrWrongFunction, got, want)
	}
}
