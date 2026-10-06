# go-mtec2mqtt

[![Open your Home Assistant instance and add this add-on repository.](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FSukramJ%2Fgo-mtec2mqtt)

A pure-Go bridge between an **M-TEC Energybutler** (hybrid PV / battery
inverter) and an **MQTT broker**, with optional **Home Assistant**
auto-discovery. It reads register data via Modbus TCP and publishes the
values for consumption by Home Assistant, evcc, or any other MQTT
consumer.

Port of [`aiomtec2mqtt`](https://github.com/sukramj/aiomtec2mqtt)
(Python / asyncio) — same YAML config, same Home Assistant entities.
Since 2.0.0 its MQTT topics follow the
[mqtt-smarthome 2.0](https://github.com/mqtt-smarthome/mqtt-smarthome/blob/master/SPEC.md)
convention instead of the Python daemon's layout (see
[MQTT topic layout](#mqtt-topic-layout)).

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
- [mqtt-smarthome 2.0](https://github.com/mqtt-smarthome/mqtt-smarthome/blob/master/SPEC.md)
  topics: `<name>/status/…` as `{"val","ts","lc"}` objects, `<name>/set/…`,
  `<name>/connected`, `<name>/info` and the maintenance topics.
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
curl -sSfL https://raw.githubusercontent.com/SukramJ/go-mtec2mqtt/main/script/install.sh | sudo bash -s -- 1.8.0
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
MQTT_TOPIC: mtec             # optional, the instance name; default "mtec"

HASS_ENABLE: true            # optional Home Assistant discovery
```

`MQTT_TOPIC` is the **instance name**, the first level of every topic
(`<name>/status/…`, `<name>/connected`, …). It defaults to `mtec`; a value
you configured keeps working verbatim (every release before 2.0.0
required it, the examples used `MTEC`). **The name is the only thing that
keeps two instances on one broker apart, and nothing checks it:** two
instances of this bridge — one per inverter — need two different names, or
they overwrite each other's `connected` and `info`. Keep it a single topic
level (no `/`): a multi-level name still works, but runs outside
mqtt-smarthome §3 and is invisible to a tool scanning `+/info`, which the
daemon logs once at start (`mtec2mqtt.mqtt_topic_multi_level`).

To connect over TLS instead of plain TCP, set `MQTT_SSL: true` (the
daemon then dials `tls://` and defaults to port 8883 unless
`MQTT_PORT` is set explicitly). `MQTT_SSL_INSECURE: true` disables
broker certificate verification — only for a self-signed certificate
on a broker you control; never enable it against an untrusted broker.

Set `DEVICE_NAME` to give the inverter a friendly name: it becomes the
Home Assistant device name (instead of the generic "MTEC EnergyButler")
and is slugged into every entity's entity-id seed, so HA seeds fresh
entity ids like `sensor.<device_name>_grid_power` — handy to tell multiple
inverters apart. The seed is the slugified English register name (matching
the Python `aiomtec2mqtt` entity ids), never the localised display name,
so entity ids stay language-independent; only the display name follows
`LANGUAGE`. The entity `unique_id` is left unchanged, so
enabling this on an existing install does not orphan established entities
or lose their history; only newly created entities pick up the nicer id.
Leave it empty to keep the previous behaviour. The MQTT topic tree also
stays keyed on the inverter serial regardless.

Running **several daemon instances against one Home Assistant
installation** (one per inverter) additionally needs
`HASS_UNIQUE_ID_INCLUDE_SERIAL: true` on every instance: by default the
HA `unique_id`s are serial-less (`MTEC_grid_power`, matching
`aiomtec2mqtt`), and the collision is **total** rather than partial — two
default-configured instances render the same 100 entity identities. Since
1.10.0 they at least publish to two different discovery topics (the
document is keyed on the serial), so neither overwrites or retracts the
other; what still collides is the identities inside them, and Home
Assistant binds each `unique_id` to whichever device declared it first.
The opt-in scopes every `unique_id` by the inverter serial
(`MTEC_<serial>_grid_power`). Beware on an existing single-inverter
install: enabling it changes every `unique_id`, which creates fresh HA
entities and orphans the established ones together with their history —
MQTT discovery has no `unique_id` migration.

`MQTT_FLOAT_FORMAT` (default `.3f`) no longer formats anything on the
wire: since 2.0.0 values are JSON numbers, and the option survives as
their **rounding precision** — `.3f` rounds to three decimals, `.4g` to
four significant digits, exactly the number the old string payload spelled.

### Maintenance topics

On by default (mqtt-smarthome §7), so tools such as the Smart Home Engine
can manage the daemon over MQTT:

| Topic | Effect |
| --- | --- |
| `<name>/maintenance/set/loglevel` | `error` / `warn` / `info` / `debug` — changes the daemon's log level until the next start |
| `<name>/maintenance/set/restart` | graceful shutdown (`connected` → `0`, exit 0) — **only** when a supervisor restarts the process, otherwise refused and logged at `warn` |
| `<name>/maintenance/stats` | retained process statistics (`rss`, `heapUsed`, `heapTotal`, `cpu`, `uptime`, `ts`) every `MQTT_STATS_INTERVAL` seconds |

```yaml
MQTT_MAINTENANCE: true       # false switches all three off
MQTT_STATS_INTERVAL: 60      # seconds; 0 switches the stats off
```

Whether a supervisor restarts the daemon is detected: systemd as the
parent process, Kubernetes, or a container counts. Set
`MTEC_SUPERVISED=1` to state it, or `MTEC_SUPERVISED=0` to refuse the
restart — a container started **without** a restart policy is detected as
supervised, and a restart there is a stop. The Home Assistant add-on sets
`MTEC_SUPERVISED=0`.

> **Security:** anyone who may publish on the broker can restart the
> daemon or raise its log level. Use broker authentication and per-client
> ACLs (the daemon needs `<name>/#` and the discovery prefix, nothing
> else); on a broker that cannot be secured, set `MQTT_MAINTENANCE: false`.

Every config key can be overridden at runtime via an `MTEC_<KEY>` env
var — useful in Docker / systemd setups:

```bash
MTEC_MQTT_PASSWORD='change-me' ./bin/mtec2mqtt
```

Bool / int / float values are coerced; everything else stays a string.

## MQTT topic layout

Since 2.0.0 the daemon follows
[mqtt-smarthome 2.0](https://github.com/mqtt-smarthome/mqtt-smarthome/blob/master/SPEC.md),
`<name>/<function>/<item…>`, with `<name>` = `MQTT_TOPIC`:

```
<name>/status/<serial>/now_base/<key>      current power, SOC, status …
<name>/status/<serial>/now_grid/<key>      per-phase grid voltage / current
<name>/status/<serial>/now_inverter/<key>  per-phase inverter power
<name>/status/<serial>/now_backup/<key>    backup-power readings
<name>/status/<serial>/now_battery/<key>   battery cell readings
<name>/status/<serial>/now_pv/<key>        PV string voltages / currents
<name>/status/<serial>/day/<key>           daily energy totals
<name>/status/<serial>/total/<key>         lifetime energy totals
<name>/status/<serial>/config/<key>        writable settings (mirror)
<name>/status/<serial>/static/<key>        serial / firmware / equipment
<name>/status/<serial>/online              the inverter reachable: true | false
<name>/set/<serial>/<group>/<key>          command topic for writables
<name>/connected = 0 | 1 | 2               daemon / inverter availability (retained)
<name>/info                                instance description (retained JSON)
<name>/maintenance/…                       see "Maintenance topics"
<hass_base>/device/<node-id>/config        ONE retained HA discovery document
```

Old and new, side by side (`MTEC` as the configured name):

| Up to 1.11 | Since 2.0.0 |
| --- | --- |
| `MTEC/<serial>/now-base/grid_power/state` = `-500` | `MTEC/status/<serial>/now_base/grid_power` = `{"val":-500,"ts":…,"lc":…}` |
| `MTEC/<serial>/now-base/consumption/state` = `3500.000` | `MTEC/status/<serial>/now_base/consumption` = `{"val":3500,…}` |
| `MTEC/<serial>/now-base/inverter_status/state` = `Netzbetrieb` (`LANGUAGE: de`) | `MTEC/status/<serial>/now_base/inverter_status` = `{"val":"on-grid",…}` |
| `MTEC/<serial>/config/grid_inject_switch/state` = `1` | `MTEC/status/<serial>/config/grid_inject_switch` = `{"val":true,…}` |
| `MTEC/<serial>/config/charge_limit/set` | `MTEC/set/<serial>/config/charge_limit` |
| `MTEC/bridge/status` = `online` / `offline` | `MTEC/connected` = `2` / `1` / `0` |
| — | `MTEC/status/<serial>/online`, `MTEC/info`, `MTEC/maintenance/…` |

The groups are snake_case (`now-base` → `now_base`, likewise `now_grid`,
`now_inverter`, `now_backup`, `now_battery`, `now_pv`); a
`registers.yaml` of your own that still says `now-base` keeps working and
the daemon logs a catalog note at start.

`<serial>` above is the inverter serial **as the inverter reports it**.
`<node-id>` is not: it is that serial put through go-hamqtt's `topic.Slug`
— lower-cased, with every character outside `a-z`, `0-9` and `-` folded to
`_`. For an ordinary alphanumeric serial such as `MT1234567890` that is
just the lower-cased form, `mt1234567890`; a serial containing a space,
`.` or `_` differs further. **Do not compute it: the daemon logs the exact
topic at start-up**, as
`coordinator.discovery_bundle_built topic=<hass_base>/device/<node-id>/config`.

`<hass_base>` is `HASS_BASE_TOPIC`, `homeassistant` unless you changed it;
a trailing slash is trimmed.

### Status payloads

Every status item is a JSON object: `val` is the value, `ts` the time of
the reading that produced it and `lc` the time the value last changed,
both in milliseconds since the epoch.

- Numbers are JSON numbers, rounded to `MQTT_FLOAT_FORMAT`'s precision.
- Switches are JSON booleans (`true` / `false`), not `1` / `0`.
- Enumerations carry their stable **English token** (`"on-grid"`,
  `"General mode"`; fault registers `"OK"` or the comma-joined English
  fault names), whatever `LANGUAGE` says. Home Assistant still shows the
  labels of your language: the discovery document maps token to label and
  back.

Status items are **retained** and published **on change and on every
broker reconnect** only — a value identical to the one the broker already
holds is not published again, whatever its timestamp. Read the retained
value rather than counting messages. Status is published at **QoS 0**,
the command subscription is **QoS 1**.

### `set`

Publish to `<name>/set/<serial>/<group>/<key>` — not retained. A plain
value and `{"val": …}` are both accepted; an empty payload and a retained
message are ignored. Switches take `true`/`false`, `1`/`0`, `on`/`off`,
`yes`/`no` in any case; enumerations take the token in any case, a label
in English or German, or the numeric register code; numbers are written
as before (scaled, range-checked). A request the daemon refuses, or a
write the inverter rejects, is logged at `warn` with its topic and
payload. Nothing echoes the request: the new value appears on the status
topic with the next poll.

### Availability

`<name>/connected` is `0` while the daemon is not running (broker-side
last will, and on a clean shutdown), `1` while it is connected to the
broker but the inverter is not reachable over Modbus, and `2` while both
are. The inverter's own reachability is also the status item
`<name>/status/<serial>/online`. Every Home Assistant entity requires both
— `connected` ≥ 2 **and** `online` true (`availability_mode: all`) — so
the entities grey out when the daemon goes away *or* the inverter stops
answering, instead of showing stale readings.

`<name>/info` is published on every broker connect: `name`
(`go-mtec2mqtt`), `version`, `spec` (`2.0`), `go`, `host`, `pid`,
`started`, `maintenance`, plus `modbus` (the inverter's address) and
`ha_discovery`.

### Upgrading to 2.0.0

2.0.0 is a clean break, with no compatibility switch:

- **Home Assistant users** need to do nothing. Every `unique_id`, entity
  id, device identifier and the discovery topic are unchanged, and the
  entities follow the new topics on their own; history is kept.
- **Everything else that reads raw topics** — Node-RED flows, Telegraf,
  dashboards, `mosquitto_sub` scripts, evcc — must move to the new topics
  and parse the JSON `val` (see the table above).
- On every start the daemon **clears what the old layout left retained**
  for *its own* inverter: the old `…/<group>/<key>/state` (and any
  retained `…/set`) of every register it publishes, under its own name and
  its own serial, and `<name>/bridge/status`. It clears exact topics only:
  another inverter's topics on the same name, another name, and anything
  under `<name>/status/…`, `<name>/connected` and the other new functions
  are never touched. It is harmless to run repeatedly and also cleans up
  after a rollback and re-upgrade.
- Kept your name? Then nothing else changes. Running **two instances on
  one broker** under the same name worked before only because the serial
  separated the state topics; since `connected` and `info` are per name,
  give each instance its own `MQTT_TOPIC` now.

> **Upgrading from ≤ 1.9.0:** the availability marker once lived at
> `<hass_base>/status/lwt` (`homeassistant/status/lwt` by default), inside
> Home Assistant's own birth tree. The daemon still clears that old
> retained copy itself on every connect.

### Home Assistant discovery: one document per device

Discovery is published as a **single retained device document** at
`<hass_base>/device/<node-id>/config`, carrying the `device` block once,
an `origin` block, and all 100 entities as components. Home Assistant
**2024.11 or newer** is required.

Earlier releases published one retained message per entity at
`homeassistant/<platform>/MTEC_<key>/config` — 100 messages, each
repeating the whole `device` block. On the first start after upgrading the
daemon **retracts all 100 of them and then publishes the document, in that
order**. It has to be that order: Home Assistant refuses a device document
while a per-entity config for the same `unique_id` is still retained, and
it refuses the per-entity config while the document is retained. The
refusal is silent — one `WARNING [mqtt.entity] Received a conflicting MQTT
discovery message` in Home Assistant's log, and the entities simply do not
appear.

**Nothing is re-keyed.** Every `unique_id` is byte-identical, and
`unique_id` is what Home Assistant's entity registry is keyed on, so
history, renames, icons, areas, hidden flags and automation references all
survive the move untouched. `MQTT_TOPIC` namespaces nothing in a
`unique_id`, but it keys every state topic: changing it later moves the
entities to the new topics and leaves the old ones retained.

> **Downgrading to ≤ 1.9.x needs one manual step.** The document stays
> retained on the broker, and the older release republishing per-entity
> configs is refused for exactly the same reason, in the same silence.
> Clear it first:
>
> ```bash
> mosquitto_pub -h <broker> -u <user> -P <password> \
>   -t <hass_base>/device/<node-id>/config -r -n
> ```
>
> Take `<hass_base>/device/<node-id>/config` **verbatim from the daemon's
> own start-up log line** `coordinator.discovery_bundle_built` — the node
> id is the serial slugged (lower-cased, anything outside `a-z0-9-` folded
> to `_`), not the raw serial, and `<hass_base>` is `HASS_BASE_TOPIC`.
> `-u`/`-P` are required on an authenticated broker, which the Home
> Assistant Mosquitto add-on is. The old release then re-adopts the same
> entities with their history intact.

> **If the daemon dies between the two steps** — retractions out, document
> not — the device has no discovery config at all and its entities are
> *absent* rather than unavailable. Restarting repairs it: a fresh process
> re-sends the retractions (a no-op against topics the broker has already
> cleared) and then publishes the document. No manual step is needed.

> **Upgrade both instances together, or accept a gap.** Until 1.10.0 an
> upgraded instance's orphan sweep judged a not-yet-upgraded sibling's
> retained per-entity configs by the MQTT root alone, which two instances
> share by default — so a staggered upgrade could retract the sibling's
> entire fleet, permanently. Fixed in 1.10.0: ownership now requires the
> retained config's `state_topic` to sit under *this* inverter's serial,
> so an instance never retracts another's entities. The reverse direction
> was always safe — an old instance's sweep declines a device document,
> because a bundle payload carries no top-level `unique_id`.

> **Two daemons against two inverters** now own two different documents,
> because the topic is keyed on the serial where the per-entity topics were
> not. The entity `unique_id`s are still shared by default, though, and
> Home Assistant will bind each one to whichever device declared it first.
> Set `HASS_UNIQUE_ID_INCLUDE_SERIAL: true` on **both** instances before
> upgrading if you run more than one. (It is a deliberate one-way change:
> it rewrites every `unique_id` and orphans the existing entities.)

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
review gate. The same rules apply to contributions: AI-assisted work is
welcome under the [AI contribution policy](./AI_POLICY.md).

## Compatibility

| Component  | Status                                                          |
|------------|-----------------------------------------------------------------|
| Inverter   | M-TEC Energybutler GEN3. Potentially Wattsonic / Sunways / Daxtromn. |
| Firmware   | V27.52.4.0 and newer use port **502**; older firmware needs **5743** + a `MODBUS_FRAMER: rtu` switch. |
| MQTT       | MQTT 5.0 by default (3.1.1 selectable via `TCPConfig.ProtocolVersion` for brokers that don't speak 5.0 yet), plain TCP (port 1883) or TLS (port 8883, `MQTT_SSL: true`). |
| Go         | 1.27+                                                           |

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

This repository distributes **two separately authored works**, under
different licences:

| Part | Licence | |
| --- | --- | --- |
| **The program** — `cmd/`, `internal/`, the rest of the source tree | **MIT** | [LICENSE](./LICENSE) |
| **The register catalogue** — `registers.yaml` | **LGPL-3.0-or-later** | [LICENSE.registers](./LICENSE.registers) |

The program is an independent Go implementation, not a translation of the
Python original. The catalogue — the Modbus register selection, ordering and
display names — derives from Christian Rödel's
[`croedel/MTECmqtt`](https://github.com/croedel/MTECmqtt) and carries his
copyright.

The program reads the catalogue from the filesystem at run time; it is not
compiled into the binary, so the two remain separable.

[`NOTICE.md`](./NOTICE.md) records the provenance of each part, including the
measurements that establish where the boundary runs.
