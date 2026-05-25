// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package protocol

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Goldenfiles are validated bit-for-bit against pymodbus by the
// testdata/cross_check.py script. The Go codec is correct iff it
// reproduces and parses those same bytes.
const fixturesDir = "../testdata"

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, name+".bin"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// --- encoding round-trips ---------------------------------------------------

func TestEncodeFC03Request(t *testing.T) {
	cases := []struct {
		name string
		tid  uint16
		uid  byte
		addr uint16
		cnt  uint16
		fix  string
	}{
		{"addr10000_count8", 0x0001, 0xF7, 10000, 8, "fc03_req__addr10000_count8_tid0001_uidf7"},
		{"addr11000_count2", 0x0002, 0xF7, 11000, 2, "fc03_req__addr11000_count2_tid0002_uidf7"},
		{"addr31000_count6", 0x0003, 0xF7, 31000, 6, "fc03_req__addr31000_count6_tid0003_uidf7"},
		{"addr0_count125_max", 0x0004, 0xF7, 0, 125, "fc03_req__addr00000_count125_tid0004_uidf7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pdu, err := EncodeReadHoldingRegisters(tc.addr, tc.cnt)
			if err != nil {
				t.Fatalf("encode pdu: %v", err)
			}
			frame, err := EncodeFrame(tc.tid, tc.uid, pdu)
			if err != nil {
				t.Fatalf("encode frame: %v", err)
			}
			want := loadFixture(t, tc.fix)
			if !bytes.Equal(frame, want) {
				t.Fatalf("wire mismatch\n got: % x\nwant: % x", frame, want)
			}
		})
	}
}

func TestEncodeFC06Request(t *testing.T) {
	pdu := EncodeWriteSingleRegister(52000, 1)
	frame, err := EncodeFrame(0x0010, 0xF7, pdu)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	want := loadFixture(t, "fc06_req__addr52000_val0001_tid0010_uidf7")
	if !bytes.Equal(frame, want) {
		t.Fatalf("wire mismatch\n got: % x\nwant: % x", frame, want)
	}
}

func TestFC03CountOutOfRange(t *testing.T) {
	for _, n := range []uint16{0, 126, 1000} {
		if _, err := EncodeReadHoldingRegisters(0, n); !errors.Is(err, ErrCountOutOfRange) {
			t.Fatalf("count=%d: expected ErrCountOutOfRange, got %v", n, err)
		}
	}
}

func TestEncodeFrameEmptyPDU(t *testing.T) {
	if _, err := EncodeFrame(1, 1, nil); !errors.Is(err, ErrEmptyPDU) {
		t.Fatalf("want ErrEmptyPDU, got %v", err)
	}
}

// --- header decoding --------------------------------------------------------

func TestDecodeHeader(t *testing.T) {
	frame := loadFixture(t, "fc03_req__addr11000_count2_tid0002_uidf7")
	h, err := DecodeHeader(frame)
	if err != nil {
		t.Fatal(err)
	}
	if h.TransactionID != 0x0002 || h.UnitID != 0xF7 || h.Length() != 5 {
		t.Fatalf("unexpected header: %+v len=%d", h, h.Length())
	}
}

func TestDecodeHeaderShort(t *testing.T) {
	if _, err := DecodeHeader([]byte{0x00, 0x01, 0x00}); !errors.Is(err, ErrShortFrame) {
		t.Fatalf("want ErrShortFrame, got %v", err)
	}
}

func TestDecodeHeaderBadProtocolID(t *testing.T) {
	bad := []byte{0x00, 0x01, 0x00, 0x01 /* nonzero */, 0x00, 0x06, 0xF7}
	if _, err := DecodeHeader(bad); !errors.Is(err, ErrBadProtocolID) {
		t.Fatalf("want ErrBadProtocolID, got %v", err)
	}
}

// --- response decoding ------------------------------------------------------

func TestDecodeFC03Response(t *testing.T) {
	cases := []struct {
		name string
		fix  string
		want []uint16
	}{
		{
			"i32_minus500",
			"fc03_resp_addr11000_count2_tid0002_uidf7",
			[]uint16{0xFFFF, 0xFE0C},
		},
		{
			"str_mtec_sn",
			"fc03_resp_addr10000_count8_tid0001_uidf7",
			[]uint16{0x4D2D, 0x5445, 0x432D, 0x534E, 0x0000, 0x0000, 0x0000, 0x0000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := loadFixture(t, tc.fix)
			h, err := DecodeHeader(frame)
			if err != nil {
				t.Fatal(err)
			}
			pdu := frame[HeaderLen : HeaderLen+h.Length()]
			regs, err := DecodeReadHoldingResponse(pdu)
			if err != nil {
				t.Fatal(err)
			}
			if len(regs) != len(tc.want) {
				t.Fatalf("len: got %d, want %d", len(regs), len(tc.want))
			}
			for i := range regs {
				if regs[i] != tc.want[i] {
					t.Fatalf("reg[%d]: got 0x%04x, want 0x%04x", i, regs[i], tc.want[i])
				}
			}
		})
	}
}

func TestDecodeFC06Response(t *testing.T) {
	frame := loadFixture(t, "fc06_resp_addr52000_val0001_tid0010_uidf7")
	h, err := DecodeHeader(frame)
	if err != nil {
		t.Fatal(err)
	}
	addr, val, err := DecodeWriteSingleResponse(frame[HeaderLen : HeaderLen+h.Length()])
	if err != nil {
		t.Fatal(err)
	}
	if addr != 52000 || val != 1 {
		t.Fatalf("got addr=%d val=%d, want 52000/1", addr, val)
	}
}

func TestDecodeFC03Exception(t *testing.T) {
	cases := []struct {
		fix  string
		code byte
	}{
		{"fc03_exc__code1_illegal_function_tid0005_uidf7", ExceptionIllegalFunction},
		{"fc03_exc__code2_illegal_addr_tid0005_uidf7", ExceptionIllegalDataAddress},
		{"fc03_exc__code3_illegal_value_tid0005_uidf7", ExceptionIllegalDataValue},
		{"fc03_exc__code4_slave_failure_tid0005_uidf7", ExceptionSlaveDeviceFailure},
	}
	for _, tc := range cases {
		t.Run(tc.fix, func(t *testing.T) {
			frame := loadFixture(t, tc.fix)
			h, err := DecodeHeader(frame)
			if err != nil {
				t.Fatal(err)
			}
			pdu := frame[HeaderLen : HeaderLen+h.Length()]
			_, err = DecodeReadHoldingResponse(pdu)
			var exc *ExceptionError
			if !errors.As(err, &exc) {
				t.Fatalf("want *ExceptionError, got %T (%v)", err, err)
			}
			if exc.Function != FCReadHoldingRegisters || exc.ExceptionCode != tc.code {
				t.Fatalf("unexpected exception: %+v", exc)
			}
		})
	}
}

func TestDecodeFC06Exception(t *testing.T) {
	frame := loadFixture(t, "fc06_exc__code2_illegal_addr_tid0010_uidf7")
	h, err := DecodeHeader(frame)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = DecodeWriteSingleResponse(frame[HeaderLen : HeaderLen+h.Length()])
	var exc *ExceptionError
	if !errors.As(err, &exc) {
		t.Fatalf("want *ExceptionError, got %T (%v)", err, err)
	}
	if exc.Function != FCWriteSingleRegister || exc.ExceptionCode != ExceptionIllegalDataAddress {
		t.Fatalf("unexpected exception: %+v", exc)
	}
}

// --- malformed payloads -----------------------------------------------------

func TestDecodeFC03BadByteCount(t *testing.T) {
	// Function-code byte then a deliberately odd byte-count
	pdu := []byte{0x03, 0x03, 0xAA, 0xBB, 0xCC}
	if _, err := DecodeReadHoldingResponse(pdu); !errors.Is(err, ErrOddByteCount) {
		t.Fatalf("want ErrOddByteCount, got %v", err)
	}
}

func TestDecodeFC03ShortPDU(t *testing.T) {
	if _, err := DecodeReadHoldingResponse([]byte{0x03}); !errors.Is(err, ErrShortPDU) {
		t.Fatalf("want ErrShortPDU, got %v", err)
	}
}

func TestDecodeFC03WrongFunction(t *testing.T) {
	pdu := []byte{0x04, 0x02, 0x00, 0x01}
	err := requireFunction(pdu, FCReadHoldingRegisters)
	if !errors.Is(err, ErrWrongFunction) {
		t.Fatalf("want ErrWrongFunction, got %v", err)
	}
}

// --- round-trip via encoder + decoder --------------------------------------

func TestRoundTripFC03(t *testing.T) {
	req, err := EncodeReadHoldingRegisters(31000, 6)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := EncodeFrame(42, 247, req)
	if err != nil {
		t.Fatal(err)
	}
	h, err := DecodeHeader(frame)
	if err != nil {
		t.Fatal(err)
	}
	if h.TransactionID != 42 || h.UnitID != 247 || h.Length() != len(req) {
		t.Fatalf("header mismatch: %+v", h)
	}
}
