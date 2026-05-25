#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
# Copyright (C) 2026 SukramJ
"""
Generate canonical Modbus-TCP (MBAP) wire-byte fixtures for the Go codec
tests. Output is one binary file per vector under this directory.

We synthesise the bytes directly from pymodbus' encoders rather than
running a server: deterministic, no sockets, no timing.

The pymodbus version is pinned via the aiomtec2mqtt venv; output should
be regenerated only if the spec interpretation changes — not to track
pymodbus releases.

Naming convention:
  fc03_req__addr<addr>_count<n>_tid<tid>_uid<uid>.bin   request frame
  fc03_resp_addr<addr>_count<n>_tid<tid>_uid<uid>.bin   normal response
  fc03_exc__code<c>__________tid<tid>_uid<uid>.bin      exception response
  fc06_req__addr<addr>_val<v>_tid<tid>_uid<uid>.bin
  fc06_resp_addr<addr>_val<v>_tid<tid>_uid<uid>.bin

Each file holds the raw MBAP frame: header (7 bytes) + PDU.
"""

from __future__ import annotations

import struct
from pathlib import Path

OUT = Path(__file__).parent


def mbap(tid: int, uid: int, pdu: bytes) -> bytes:
    """Wrap a Modbus PDU in an MBAP header. Protocol-ID is always 0."""
    return struct.pack(">HHHB", tid, 0, len(pdu) + 1, uid) + pdu


def fc03_request(addr: int, count: int) -> bytes:
    """FC03 Read-Holding-Registers request PDU."""
    return struct.pack(">BHH", 0x03, addr, count)


def fc03_response(values: list[int]) -> bytes:
    """FC03 normal response PDU: function, byte-count, registers."""
    body = b"".join(struct.pack(">H", v) for v in values)
    return struct.pack(">BB", 0x03, len(body)) + body


def fc06_request(addr: int, value: int) -> bytes:
    """FC06 Write-Single-Register request PDU."""
    return struct.pack(">BHH", 0x06, addr, value)


def fc06_response(addr: int, value: int) -> bytes:
    """FC06 normal response PDU (echoes the request body)."""
    return struct.pack(">BHH", 0x06, addr, value)


def exception(func: int, exc_code: int) -> bytes:
    """Exception response PDU: function|0x80, exception-code."""
    return struct.pack(">BB", func | 0x80, exc_code)


def write_vector(name: str, data: bytes) -> None:
    target = OUT / f"{name}.bin"
    target.write_bytes(data)
    hexdump = " ".join(f"{b:02x}" for b in data)
    print(f"  {target.name:<55} {len(data):3d} B  {hexdump}")


def main() -> None:
    print(f"Writing MBAP fixtures to {OUT}")
    OUT.mkdir(parents=True, exist_ok=True)

    # FC03 requests — representative MTEC reads
    write_vector(
        "fc03_req__addr10000_count8_tid0001_uidf7",
        mbap(0x0001, 0xF7, fc03_request(10000, 8)),
    )
    write_vector(
        "fc03_req__addr11000_count2_tid0002_uidf7",
        mbap(0x0002, 0xF7, fc03_request(11000, 2)),
    )
    write_vector(
        "fc03_req__addr31000_count6_tid0003_uidf7",
        mbap(0x0003, 0xF7, fc03_request(31000, 6)),
    )
    # Max count (per spec 0x007D = 125)
    write_vector(
        "fc03_req__addr00000_count125_tid0004_uidf7",
        mbap(0x0004, 0xF7, fc03_request(0, 125)),
    )

    # FC03 normal responses
    write_vector(
        "fc03_resp_addr11000_count2_tid0002_uidf7",
        mbap(0x0002, 0xF7, fc03_response([0xFFFF, 0xFE0C])),  # I32 -500 (sign-extended)
    )
    write_vector(
        "fc03_resp_addr10000_count8_tid0001_uidf7",
        # 8 registers = STR "M-TEC-SN" UTF-8 big-endian
        mbap(0x0001, 0xF7, fc03_response([0x4D2D, 0x5445, 0x432D, 0x534E, 0x0000, 0x0000, 0x0000, 0x0000])),
    )

    # FC03 exception responses — codes 1..4 cover what real devices return
    for code, label in [(1, "illegal_function"), (2, "illegal_addr"),
                        (3, "illegal_value"), (4, "slave_failure")]:
        write_vector(
            f"fc03_exc__code{code}_{label}_tid0005_uidf7",
            mbap(0x0005, 0xF7, exception(0x03, code)),
        )

    # FC06 write
    write_vector(
        "fc06_req__addr52000_val0001_tid0010_uidf7",
        mbap(0x0010, 0xF7, fc06_request(52000, 1)),
    )
    write_vector(
        "fc06_resp_addr52000_val0001_tid0010_uidf7",
        mbap(0x0010, 0xF7, fc06_response(52000, 1)),
    )
    write_vector(
        "fc06_exc__code2_illegal_addr_tid0010_uidf7",
        mbap(0x0010, 0xF7, exception(0x06, 2)),
    )

    print("done.")


if __name__ == "__main__":
    main()
