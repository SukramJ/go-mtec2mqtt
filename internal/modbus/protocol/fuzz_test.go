// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package protocol

import (
	"testing"
)

// The fuzz targets pin the hand-rolled codec's invariants on arbitrary
// input: decoders must never panic, and whatever they accept must be
// internally consistent. The seed corpus runs on every `go test`;
// explore further with `go test -fuzz=FuzzDecodeHeader ./internal/modbus/protocol`.

func FuzzDecodeHeader(f *testing.F) {
	f.Add([]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x06, 0xF7}) // valid FC03 request header
	f.Add([]byte{0x00, 0x01, 0x00, 0x01, 0x00, 0x06, 0xF7}) // non-zero protocol-id
	f.Add([]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0xF7}) // declared length too small
	f.Add([]byte{0x00, 0x01, 0x00, 0x00, 0xFF, 0xFF, 0xF7}) // declared length too large
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Fuzz(func(t *testing.T, buf []byte) {
		h, err := DecodeHeader(buf)
		if err != nil {
			return
		}
		if h.Length() < 1 || h.Length() > MaxPDULen {
			t.Fatalf("accepted header with out-of-range PDU length %d", h.Length())
		}
	})
}

func FuzzDecodeReadHoldingResponse(f *testing.F) {
	f.Add([]byte{0x03, 0x04, 0xFF, 0xFF, 0xFE, 0x0C}) // two registers
	f.Add([]byte{0x83, 0x02})                         // exception: illegal address
	f.Add([]byte{0x03, 0x00})                         // zero byte-count
	f.Add([]byte{0x03, 0x03, 0xAA, 0xBB, 0xCC})       // odd byte-count
	f.Add([]byte{0x06, 0xCB, 0x20, 0x00, 0x01})       // wrong function
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, pdu []byte) {
		regs, err := DecodeReadHoldingResponse(pdu)
		if err != nil {
			return
		}
		if len(regs) == 0 {
			t.Fatal("accepted FC03 response with zero registers")
		}
		if len(pdu) != 2+2*len(regs) {
			t.Fatalf("decoded %d registers from a %d-byte PDU", len(regs), len(pdu))
		}
	})
}

func FuzzDecodeWriteSingleResponse(f *testing.F) {
	f.Add([]byte{0x06, 0xCB, 0x20, 0x00, 0x01}) // valid echo
	f.Add([]byte{0x86, 0x02})                   // exception: illegal address
	f.Add([]byte{0x06, 0x00, 0x01})             // truncated
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, pdu []byte) {
		_, _, err := DecodeWriteSingleResponse(pdu)
		if err == nil && len(pdu) != 5 {
			t.Fatalf("accepted FC06 response with %d bytes, want exactly 5", len(pdu))
		}
	})
}

func FuzzFrameRoundTrip(f *testing.F) {
	f.Add(uint16(1), byte(247), uint16(11000), uint16(2))
	f.Add(uint16(0), byte(0), uint16(0), uint16(125))
	f.Add(uint16(0xFFFF), byte(255), uint16(0xFFFF), uint16(0))
	f.Fuzz(func(t *testing.T, tid uint16, uid byte, addr, count uint16) {
		pdu, err := EncodeReadHoldingRegisters(addr, count)
		if err != nil {
			return // invalid count — nothing to round-trip
		}
		frame, err := EncodeFrame(tid, uid, pdu)
		if err != nil {
			t.Fatalf("EncodeFrame rejected a valid PDU: %v", err)
		}
		h, err := DecodeHeader(frame)
		if err != nil {
			t.Fatalf("DecodeHeader rejected our own frame: %v", err)
		}
		if h.TransactionID != tid || h.UnitID != uid || h.Length() != len(pdu) {
			t.Fatalf("round-trip mismatch: got %+v len=%d, want tid=%d uid=%d len=%d",
				h, h.Length(), tid, uid, len(pdu))
		}
	})
}
