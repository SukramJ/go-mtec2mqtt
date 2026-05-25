#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
# Copyright (C) 2026 SukramJ
"""
Cross-check: feed identical requests through pymodbus' real codec and
assert it produces the same MBAP bytes as our hand-rolled fixtures in
generate_vectors.py.

If this script passes, the goldenfiles are spec-conformant by virtue of
matching a battle-tested reference implementation.
"""

from __future__ import annotations

import struct
import sys
from pathlib import Path

from pymodbus.framer import FramerType
from pymodbus.framer.socket import FramerSocket
from pymodbus.pdu.decoders import DecodePDU
from pymodbus.pdu.register_message import (
    ReadHoldingRegistersRequest,
    ReadHoldingRegistersResponse,
    WriteSingleRegisterRequest,
    WriteSingleRegisterResponse,
)
from pymodbus.pdu import ExceptionResponse

OUT = Path(__file__).parent


def build_socket_framer() -> FramerSocket:
    """pymodbus' MBAP/TCP framer (the 'socket' framer in config)."""
    # DecodePDU(is_server=False) means: we'll decode responses (server-to-client).
    return FramerSocket(DecodePDU(False))


def encode_via_pymodbus(pdu, tid: int, uid: int) -> bytes:
    """Build a complete MBAP frame the way pymodbus' client would."""
    framer = build_socket_framer()
    pdu.transaction_id = tid
    pdu.dev_id = uid
    return framer.buildFrame(pdu)


def assert_eq(label: str, got: bytes, want_path: Path) -> None:
    want = want_path.read_bytes()
    if got != want:
        sys.stderr.write(f"MISMATCH: {label}\n")
        sys.stderr.write(f"  got:  {got.hex(' ')}\n")
        sys.stderr.write(f"  want: {want.hex(' ')}\n")
        sys.exit(1)
    print(f"  OK  {label}")


def main() -> None:
    print("Cross-checking goldenfiles against pymodbus encoders")

    # FC03 requests
    req = ReadHoldingRegistersRequest(address=10000, count=8)
    assert_eq(
        "fc03_req addr=10000 count=8",
        encode_via_pymodbus(req, tid=0x0001, uid=0xF7),
        OUT / "fc03_req__addr10000_count8_tid0001_uidf7.bin",
    )
    req = ReadHoldingRegistersRequest(address=11000, count=2)
    assert_eq(
        "fc03_req addr=11000 count=2",
        encode_via_pymodbus(req, tid=0x0002, uid=0xF7),
        OUT / "fc03_req__addr11000_count2_tid0002_uidf7.bin",
    )
    req = ReadHoldingRegistersRequest(address=31000, count=6)
    assert_eq(
        "fc03_req addr=31000 count=6",
        encode_via_pymodbus(req, tid=0x0003, uid=0xF7),
        OUT / "fc03_req__addr31000_count6_tid0003_uidf7.bin",
    )
    req = ReadHoldingRegistersRequest(address=0, count=125)
    assert_eq(
        "fc03_req addr=0 count=125",
        encode_via_pymodbus(req, tid=0x0004, uid=0xF7),
        OUT / "fc03_req__addr00000_count125_tid0004_uidf7.bin",
    )

    # FC03 responses
    resp = ReadHoldingRegistersResponse(registers=[0xFFFF, 0xFE0C])
    assert_eq(
        "fc03_resp count=2 I32(-500)",
        encode_via_pymodbus(resp, tid=0x0002, uid=0xF7),
        OUT / "fc03_resp_addr11000_count2_tid0002_uidf7.bin",
    )
    resp = ReadHoldingRegistersResponse(
        registers=[0x4D2D, 0x5445, 0x432D, 0x534E, 0x0000, 0x0000, 0x0000, 0x0000]
    )
    assert_eq(
        "fc03_resp count=8 STR",
        encode_via_pymodbus(resp, tid=0x0001, uid=0xF7),
        OUT / "fc03_resp_addr10000_count8_tid0001_uidf7.bin",
    )

    # FC03 exceptions
    for code, label in [(1, "illegal_function"), (2, "illegal_addr"),
                        (3, "illegal_value"), (4, "slave_failure")]:
        exc = ExceptionResponse(function_code=0x03, exception_code=code)
        assert_eq(
            f"fc03_exc code={code}",
            encode_via_pymodbus(exc, tid=0x0005, uid=0xF7),
            OUT / f"fc03_exc__code{code}_{label}_tid0005_uidf7.bin",
        )

    # FC06 write request + response + exception. In pymodbus 3.13 the
    # single-register write reuses the multi-register PDU and passes
    # the value via ``registers=[v]``.
    req = WriteSingleRegisterRequest(address=52000, registers=[1])
    assert_eq(
        "fc06_req addr=52000 val=1",
        encode_via_pymodbus(req, tid=0x0010, uid=0xF7),
        OUT / "fc06_req__addr52000_val0001_tid0010_uidf7.bin",
    )
    resp = WriteSingleRegisterResponse(address=52000, registers=[1])
    assert_eq(
        "fc06_resp addr=52000 val=1",
        encode_via_pymodbus(resp, tid=0x0010, uid=0xF7),
        OUT / "fc06_resp_addr52000_val0001_tid0010_uidf7.bin",
    )
    exc = ExceptionResponse(function_code=0x06, exception_code=2)
    assert_eq(
        "fc06_exc code=2",
        encode_via_pymodbus(exc, tid=0x0010, uid=0xF7),
        OUT / "fc06_exc__code2_illegal_addr_tid0010_uidf7.bin",
    )

    print("all goldenfiles match pymodbus output")


if __name__ == "__main__":
    main()
