# go-mtec2mqtt

[![Open your Home Assistant instance and add this add-on repository.](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FSukramJ%2Fgo-mtec2mqtt)

A pure-Go bridge between an **M-TEC Energybutler** (hybrid PV / battery
inverter) and an **MQTT broker**, with optional **Home Assistant**
auto-discovery. It reads register data via Modbus TCP and publishes the
values for consumption by Home Assistant, evcc, or any other MQTT
consumer.

Port of [`aiomtec2mqtt`](https://github.com/sukramj/aiomtec2mqtt)
(Python / asyncio) — same YAML config, same MQTT topic layout, same
Home Assistant entities. Drop-in replacement for the Python daemon.

## Features

- Reads 90+ registers from M-TEC Energybutler GEN3 inverters.
- Round-robin polling of secondary register groups (grid / inverter /
  backup / battery / PV) so the inverter's slow Modbus stack stays
  happy.
- Computed pseudo-registers (`consumption`, `autarky_rate_*`,
  `own_consumption_*`, `api_date`) published alongside the raw values.
- Home Assistant MQTT auto-discovery for sensor / binary_sensor /
  number / select / switch entities.
- Writes back to the inverter on HA select / number / switch
  interactions — value\_items reverse-lookup, scaling and integer
  narrowing all match the Python coordinator.
- Watchdog-driven Modbus reconnect; exponential-backoff MQTT
  reconnect with subscription replay.
- Pure Go, no CGo — single static binary, distroless Docker image.

## Quickstart

### Linux (Ubuntu / Raspberry Pi OS)

One-liner that downloads the latest release, verifies its checksum,
installs the binaries under `/opt/go-mtec2mqtt`, creates a dedicated
`mtec` service user, runs an interactive 3-question wizard for the
config fields with no usable default (`MODBUS_IP`, `MQTT_SERVER`,
`HASS_ENABLE`), and registers a hardened systemd unit:

```bash
curl -sSfL https://raw.githubusercontent.com/SukramJ/go-mtec2mqtt/main/script/install.sh | sudo bash
```

Pin a specific version:

```bash
curl -sSfL https://raw.githubusercontent.com/SukramJ/go-mtec2mqtt/main/script/install.sh | sudo bash -s -- 1.0.0
```

The wizard prompts read from `/dev/tty` so they work fine over the
`curl | bash` pipe. Existing `/etc/go-mtec2mqtt/config.yaml` is never
touched; existing `/opt/go-mtec2mqtt` is moved aside to
`/opt/go-mtec2mqtt.bak.<timestamp>` before the upgrade. Supports
`linux/amd64` and `linux/arm64` (Raspberry Pi).

After install:

```bash
sudo systemctl status go-mtec2mqtt        # check it stayed up
journalctl -u go-mtec2mqtt -f             # follow the logs
sudo nano /etc/go-mtec2mqtt/config.yaml   # edit MQTT credentials, refresh intervals, …
sudo systemctl restart go-mtec2mqtt       # after editing
```

### Docker

```bash
docker run --rm -d \
  --name mtec2mqtt \
  -v /path/to/your/config:/config:ro \
  ghcr.io/sukramj/go-mtec2mqtt:latest
```

The container expects a `config.yaml` at `/config/aiomtec2mqtt/config.yaml`
(matches the XDG path the daemon walks). Start from
[`config-template.yaml`](./config-template.yaml). Alternatively, supply
every setting via `MTEC_*` env vars and mount no file — the daemon falls
back to an environment-only config when no `config.yaml` is found.

### Home Assistant Add-on

Run the daemon as a Home Assistant add-on with a dedicated config UI and a
sidebar panel for the diagnostic dashboard (served via Ingress):

1. **Settings → Add-ons → Add-on Store → ⋮ → Repositories** and add
   `https://github.com/SukramJ/go-mtec2mqtt`.
2. Install **go-mtec2mqtt**, set `modbus_ip` (leave `mqtt_server` empty to
   auto-use the HA MQTT broker), and **Start**.

Full details in [`addon/README.md`](./addon/README.md) and the option
reference in [`addon/DOCS.md`](./addon/DOCS.md).

### Binary

```bash
make build
./bin/mtec2mqtt --config ./config.yaml --registers ./registers.yaml
```

The daemon walks the following paths for `config.yaml` when `--config`
is omitted:

1. `./config.yaml`
2. `$XDG_CONFIG_HOME/aiomtec2mqtt/config.yaml` (or `$APPDATA/...` on
   Windows)
3. `~/.config/aiomtec2mqtt/config.yaml`

`registers.yaml` defaults to the directory of the binary, then the
current working directory.

### Interactive register CLI

```bash
./bin/mtec-util
```

Lists the catalog, reads or writes a single register, or dumps every
register in a group. Works without a Modbus connection for the
listing options.

## Configuration

Every field is documented in [`config-template.yaml`](./config-template.yaml).
At a minimum you need:

```yaml
MODBUS_IP: 192.168.1.50      # inverter / espressif gateway address
MODBUS_PORT: 502             # 502 for firmware ≥ V27.52.4.0, 5743 below
MODBUS_SLAVE: 247
MODBUS_TIMEOUT: 5

MQTT_SERVER: localhost
MQTT_PORT: 1883
MQTT_TOPIC: MTEC

HASS_ENABLE: true            # optional Home Assistant discovery
```

To connect over TLS instead of plain TCP, set `MQTT_SSL: true` (the
daemon then dials `tls://` and defaults to port 8883 unless
`MQTT_PORT` is set explicitly). `MQTT_SSL_INSECURE: true` disables
broker certificate verification — only for a self-signed certificate
on a broker you control; never enable it against an untrusted broker.

Every config key can be overridden at runtime via an `MTEC_<KEY>` env
var — useful in Docker / systemd setups:

```bash
MTEC_MQTT_PASSWORD='change-me' ./bin/mtec2mqtt
```

Bool / int / float values are coerced; everything else stays a string.

## MQTT topic layout

```
MTEC/<serial>/now-base/<key>/state         current power, SOC, status …
MTEC/<serial>/now-grid/<key>/state         per-phase grid voltage / current
MTEC/<serial>/now-inverter/<key>/state     per-phase inverter power
MTEC/<serial>/now-backup/<key>/state       backup-power readings
MTEC/<serial>/now-battery/<key>/state      battery cell readings
MTEC/<serial>/now-pv/<key>/state           PV string voltages / currents
MTEC/<serial>/day/<key>/state              daily energy totals
MTEC/<serial>/total/<key>/state            lifetime energy totals
MTEC/<serial>/config/<key>/state           writable settings (mirror)
MTEC/<serial>/static/<key>/state           serial / firmware / equipment
MTEC/<serial>/<group>/<key>/set            command topic for writables
homeassistant/<platform>/MTEC_<key>/config retained HA discovery payloads
<hass_base>/status/lwt = offline           LWT topic
```

## Development

```bash
make build           # compile both binaries into bin/
make test            # full test suite with race detector
make check           # vet + gofumpt + test (pre-push gate)
make docker          # build the container image
```

The codebase is laid out around the natural seams of the data flow:

```
cmd/mtec2mqtt/       daemon entry point
cmd/mtec-util/       interactive register CLI
internal/config/     YAML loader + MTEC_* env overlay + validation
internal/registers/  catalog loader, type-aware decoders, address clustering
internal/modbus/     Modbus-TCP transport (own MBAP codec, no third-party deps)
internal/hass/       Home Assistant discovery payload builder
internal/coordinator/orchestration: poll loops, pseudo-registers, write queue
internal/version/    build-info package
```

The MQTT transport is no longer part of this tree: it is the external
[`github.com/SukramJ/go-mqtt`](https://github.com/SukramJ/go-mqtt) module
(MIT-licensed, `openccu-loom` provenance — see [Credit](#credit)), pulled in
as a regular `go.mod` dependency.

The Modbus MBAP codec ships with byte-for-byte goldenfile vectors
generated from `pymodbus 3.13`
(`internal/modbus/testdata/cross_check.py`) so wire-format
correctness is anchored to a battle-tested reference.

Parts of go-mtec2mqtt are developed with agentic AI assistance, primarily
[Claude Code](https://www.anthropic.com/claude-code). Submitted issues are
also triaged and analysed with agentic help. Every change is still
reviewed by a human maintainer and has to pass the project's test suite
before it lands — the AI accelerates the work, it does not replace the
review gate.

## Compatibility

| Component  | Status                                                          |
|------------|-----------------------------------------------------------------|
| Inverter   | M-TEC Energybutler GEN3. Potentially Wattsonic / Sunways / Daxtromn. |
| Firmware   | V27.52.4.0 and newer use port **502**; older firmware needs **5743** + a `MODBUS_FRAMER: rtu` switch. |
| MQTT       | MQTT 5.0 by default (3.1.1 selectable via `TCPConfig.ProtocolVersion` for brokers that don't speak 5.0 yet), plain TCP (port 1883) or TLS (port 8883, `MQTT_SSL: true`). |
| Go         | 1.26+                                                           |

## Credit

- Original Python project:
  [SukramJ/aiomtec2mqtt](https://github.com/sukramj/aiomtec2mqtt)
  (LGPL-3.0).
- Upstream Python ancestor:
  [croedel/MTECmqtt](https://github.com/croedel/MTECmqtt) by **Christian
  Rödel** (LGPL-3.0) — the original reverse-engineering work this project
  builds on, including the register map. Thank you!
- Pure-Go MQTT stack: [SukramJ/go-mqtt](https://github.com/SukramJ/go-mqtt)
  (MIT-licensed), a standalone module extracted from
  [SukramJ/openccu-loom](https://github.com/SukramJ/openccu-loom) and pulled
  in as a `go.mod` dependency; its MIT copyright notice lives in that
  module, not in this repo.

## License

LGPL-3.0 — see [LICENSE](./LICENSE).

This project is a Go port of `aiomtec2mqtt`, which in turn derives from
Christian Rödel's [`croedel/MTECmqtt`](https://github.com/croedel/MTECmqtt).
As a derivative of that LGPL-3.0 work it is distributed under the same
license; the copyright notices of both Christian Rödel and SukramJ are
preserved in [LICENSE](./LICENSE).
