# Notice — provenance and licensing

go-mtec2mqtt distributes two separately authored works under different
licences: the **program** (MIT, [`LICENSE`](./LICENSE)) and the **register
catalogue** `registers.yaml` (LGPL-3.0-or-later,
[`LICENSE.registers`](./LICENSE.registers)). This file records where each
came from and how that was established.

## Lineage

The project traces back to Christian Rödel's
[`croedel/MTECmqtt`](https://github.com/croedel/MTECmqtt) (LGPL-3.0), by way
of [`sukramj/aiomtec2mqtt`](https://github.com/sukramj/aiomtec2mqtt). Christian
Rödel did the original reverse engineering of the inverter's Modbus interface,
including the register catalogue. That work is the foundation this project
stands on, and it is gratefully acknowledged.

## The program is an independent implementation

The Go source is not a translation of the Python original. Measured against
`croedel/MTECmqtt`:

| | croedel/MTECmqtt | go-mtec2mqtt |
| --- | ---: | ---: |
| Files / lines of code | 9 Python / 1 166 | 28 Go / 6 737 |
| Functions | 38 | 177 |
| **Identically named functions** | | **5** |

The five shared names are `connect`, `initialize`, `readRegister`,
`appendSensor` and `appendBinarySensor` — generic names for generic
operations. The architectures do not correspond: this project separates
`coordinator/`, `modbus/protocol/`, `state/`, `web/` and `hass/`, where the
original has nine flat modules. No source, comment or structural sequence was
carried across.

Under § 69a(2) UrhG the ideas and principles underlying a program — including
those underlying its interfaces — are not protected. What was reused is
knowledge about the device, not expression. The program is therefore licensed
on its own terms (MIT), by its own author.

## The register catalogue is not independent

`registers.yaml` is a different matter, and it is stated plainly rather than
glossed over. Measured against `src/mtecmqtt/registers.yaml`:

| | |
| --- | --- |
| Register keys present in both | **86 of 86** — none missing |
| Display names identical word for word | **86 of 86** |
| Ordering of the shared keys | **identical** |

This project adds 8 further entries. Everything else — the selection, the
arrangement, and the display names Christian Rödel chose — comes from the
original. Those names are his wording, not the device's.

The catalogue therefore stays under LGPL-3.0-or-later with his copyright
notice intact, and it is treated as a work in its own right rather than as
part of the program.

## How the two are kept apart

`registers.yaml` is read from the filesystem at run time. It is **not**
compiled into the binary: the single `go:embed` directive in this repository
covers the web UI's static assets (`internal/web/web.go`). A different
catalogue can be supplied without rebuilding, and the compiled binary contains
none of the catalogue's content.

## Third-party components

- **Pure-Go MQTT stack**: [`SukramJ/go-mqtt`](https://github.com/SukramJ/go-mqtt)
  (MIT), a standalone module extracted from
  [`SukramJ/openccu-loom`](https://github.com/SukramJ/openccu-loom) and pulled
  in as a `go.mod` dependency. Its copyright notice lives in that module.
- All further Go dependencies are permissive (MIT / BSD / Apache-2.0); see
  [`go.mod`](./go.mod).

## Trademarks

"M-TEC" and product names are trademarks of their respective owners. This
project is independent and not endorsed by or affiliated with any of them.
