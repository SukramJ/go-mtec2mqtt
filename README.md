# go-mtec2mqtt

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
[`config-template.yaml`](./config-template.yaml).

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
internal/mqtt/       Pure-Go MQTT 3.1.1 client (lifted from openccu-loom)
internal/hass/       Home Assistant discovery payload builder
internal/coordinator/orchestration: poll loops, pseudo-registers, write queue
internal/version/    build-info package
```

The Modbus MBAP codec ships with byte-for-byte goldenfile vectors
generated from `pymodbus 3.13`
(`internal/modbus/testdata/cross_check.py`) so wire-format
correctness is anchored to a battle-tested reference.

## Compatibility

| Component  | Status                                                          |
|------------|-----------------------------------------------------------------|
| Inverter   | M-TEC Energybutler GEN3. Potentially Wattsonic / Sunways / Daxtromn. |
| Firmware   | V27.52.4.0 and newer use port **502**; older firmware needs **5743** + a `MODBUS_FRAMER: rtu` switch. |
| MQTT       | Plain TCP MQTT 3.1.1 (port 1883). TLS not wired up yet.          |
| Go         | 1.26+                                                           |

## Credit

- Original Python project:
  [SukramJ/aiomtec2mqtt](https://github.com/sukramj/aiomtec2mqtt)
- Upstream Python ancestor:
  [croedel/MTECmqtt](https://github.com/croedel/MTECmqtt)
- Pure-Go MQTT stack lifted from
  [SukramJ/openccu-loom](https://github.com/SukramJ/openccu-loom)
  (MIT-licensed; copyright preserved in the file headers).

## License

MIT — see [LICENSE](./LICENSE).
