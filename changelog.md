# Version 1.1.0 (2026-06-15)

## What's Changed

Adds full internationalisation (English default, German optional) across
the web dashboard and Home Assistant entity names, plus two new
charge/discharge "active" switch entities.

### Added

- **Internationalisation (i18n).** New `LANGUAGE` config key (`en` default
  / `de`) localises both the embedded web dashboard and the friendly names
  of Home Assistant entities.
  - Web UI strings come from embedded translation bundles
    (`internal/web/static/i18n/en.json`, `de.json`); the SPA loads the
    bundle for the configured language and formats numbers/timestamps for
    the matching locale. Static markup is keyed via `data-i18n` attributes.
  - Register names and enum value labels carry optional `name_de` /
    `hass_value_items_de` entries in `registers.yaml`, resolved per-entry
    with a fallback to English (`LocalizedName` / `LocalizedValueItems`).
  - Enum/select round-trip is language-consistent: published state values
    use the localised label so they match the (localised) HA select
    options, and the write path accepts the English **or** German label
    (`CodeForLabel`) so a translated select option maps back to the device
    regardless of language.
- **Stable Home Assistant entity_ids under translation.** Every discovery
  payload now emits an explicit `object_id` derived from the
  language-independent MQTT key, so changing `LANGUAGE` re-labels the
  friendly name only and never re-creates an entity.
- **Charge/discharge "active" switches.** Two new HA `switch` entities
  (`charge_active` / `discharge_active`). "On" writes the configured
  amperage to the charge/discharge limit register, "off" writes 0. The last
  non-zero value is remembered on every config poll and restored on the next
  "on"; `CHARGE_ACTIVE_VALUE` / `DISCHARGE_ACTIVE_VALUE` (default 50) are the
  initial fallback. The switches are also rendered as toggles in the web UI,
  and their write path is shared between the HA command queue and the web
  API.

### Changed

- The web dashboard default language is now English (was German); German is
  available via `LANGUAGE: de`.

### Test coverage

- Config: `LANGUAGE` validation + defaulting, `CHARGE_ACTIVE_VALUE` /
  `DISCHARGE_ACTIVE_VALUE` range and defaults.
- Registers: localisation fallback (`name_de`, per-code enum fallback).
- HA discovery: stable `object_id`, German names/options, virtual-switch
  entities (topics, payloads, localised names).
- Coordinator: virtual-switch state derivation, off→restore and
  default-fallback writes, write routing, payload parsing, localised
  `Registers()`.
- Web: localisation bundles are served from the embedded asset tree.

# Version 1.0.0 (2026-05-25)

## What's Changed

First release of `go-mtec2mqtt` — a pure-Go port of the Python
[`aiomtec2mqtt`](https://github.com/SukramJ/aiomtec2mqtt) bridge for
the M-TEC Energybutler hybrid PV/battery inverter. Same YAML config,
same MQTT topic layout, same Home Assistant entities — drop-in
replacement for the Python daemon.

### Added

- **Modbus-TCP transport** with an own MBAP codec (`internal/modbus/protocol`)
  validated bit-for-bit against `pymodbus 3.13` goldenfile vectors. Sequential
  request/reply on a single TCP connection, per-call context deadline,
  connection poisoning on I/O errors so the watchdog can reconnect.
- **Pure-Go MQTT 3.1.1 client** (TCP + TLS, QoS 0/1 with PUBACK
  tracking, LWT, keepalive ping, subscription replay on reconnect)
  lifted from [`openccu-loom`](https://github.com/SukramJ/openccu-loom).
  Exponential backoff lifecycle wrapper, no external dependencies.
- **Register catalog** (`registers.yaml`) ported verbatim from
  `aiomtec2mqtt` — 94 register definitions including STR / BYTE / BIT
  / DAT types, scaling factors, value-items maps for select/enum
  registers and Home Assistant device-class hints.
- **Type-aware decoders** for every Modbus data type the inverter
  exposes (U16/S16/U32/S32/BYTE/BIT/DAT/STR), byte-for-byte
  compatible with the Python coordinator's output shape.
- **Address clustering** that folds nearby registers into single
  Modbus reads (gap ≤ 10) so the inverter's slow Modbus stack stays
  responsive.
- **Coordinator orchestration** with one polling goroutine per group
  (`base` / `config` / `secondary` round-robin / `day` / `total` /
  `static`), driven by the existing `REFRESH_*` config keys.
- **Pseudo-registers**: `consumption`, `api_date`, `consumption_day`,
  `autarky_rate_day`, `own_consumption_day`, plus the corresponding
  `*_total` set — formulas identical to the Python coordinator with
  zero-denominator guards so a brand-new day never produces `NaN`.
- **Home Assistant auto-discovery** (`internal/hass`) — emits
  `sensor` / `binary_sensor` / `number` / `select` / `switch`
  payloads with the same topic layout (`<hass_base>/<platform>/MTEC_<key>/config`)
  the upstream Python daemon uses, so HA dashboards keep working
  unchanged. 98 discovery entries from the shipped catalog.
- **Write-back path** for HA `select` / `number` / `switch`
  interactions: value-items reverse-lookup (label → code), scaling,
  uint16 narrowing — all matching the Python `WriteRegisterByMQTT`
  semantics.
- **YAML configuration** with the full schema from `aiomtec2mqtt`
  (every field documented in `config-template.yaml`) plus runtime
  override via `MTEC_<KEY>` env vars. `MQTT_FLOAT_FORMAT` translated
  from Python format specs (`{:.3f}`) to Go fmt verbs (`%.3f`).
- **Config locator** that walks `./config.yaml` → `$XDG_CONFIG_HOME/aiomtec2mqtt/`
  → `$APPDATA/aiomtec2mqtt/` → `~/.config/aiomtec2mqtt/` so an
  existing Python install can swap binaries without moving files.
- **Watchdog-driven reconnect**: Modbus transport poisons the
  connection on any I/O error; the watchdog re-dials on a 5 s tick.
  MQTT lifecycle handles its own reconnect with exponential backoff
  and replays every registered subscription.
- **Interactive register CLI** (`mtec-util`) for listing the
  catalog, reading a single register or a whole group, and writing
  a value (with confirmation prompt). Works without a Modbus
  connection for the listing options.
- **Daemon entry point** (`mtec2mqtt`) with `--config`,
  `--registers`, `--version` flags and `SIGINT` / `SIGTERM` graceful
  shutdown via `signal.NotifyContext`.
- **Multi-stage Docker build** producing a statically-linked
  `gcr.io/distroless/static-debian12:nonroot` image (CGO disabled).
  `/config` volume + `XDG_CONFIG_HOME=/config` so
  `docker run -v ./my-config:/config:ro` Just Works.
- **Makefile** with the usual targets (`build` / `test` /
  `test-cover` / `vet` / `fmt` / `fmt-check` / `check` / `docker`)
  plus build-info injection via `-ldflags`.
- **GitHub Actions CI** running `go vet` + `gofumpt -l` (lint),
  `go test -race` matrix across Linux / macOS / Windows, and a
  build smoke that checks the `--version` banner.
- **Tag-triggered release workflow**: pushing a `X.Y.Z` (or
  `vX.Y.Z`) tag extracts the matching changelog section, cross-
  compiles binaries for `linux/amd64`, `linux/arm64`, `darwin/arm64`,
  and attaches them as release assets.

### Test coverage

~165 tests across 9 packages, all `-race`-clean. Highlights:

- Modbus codec: 23 sub-tests against 13 `pymodbus`-generated
  goldenfile vectors (request/response/exception for FC03 + FC06).
- Modbus transport: 11 tests with an in-process mock Modbus-TCP
  server, including concurrent-reads-are-serialised, timeout-
  poisoning, FC06 echo mismatch.
- Register catalog: 4 loader tests (incl. smoke against the real
  `registers.yaml`), 16 decoder tests covering every data type and
  the sign-extension boundaries, 10 clustering tests.
- Config loader: 10 tests including ENV-override coercion, format-
  spec translation, multi-error aggregation.
- HA discovery: 11 tests + smoke against the real catalog (98
  entries, all round-trip through `json.Unmarshal` cleanly).
- Coordinator: 7 integration tests with stub transports covering
  STATIC init, pseudo-register publication, enum conversion, HA
  discovery, inbound `/set` command handling, watchdog reconnect,
  fail-fast-on-connect.
- MQTT transport: 35 tests inherited from `openccu-loom` covering
  the full lifecycle, codec, and reconnect scenarios.

### Verified

End-to-end against a live M-TEC Energybutler GEN3 (firmware
V27.53.5.0) and a Mosquitto broker:

- STATIC init reads `serial_no`, `firmware_version`, `equipment_info`
  correctly.
- 98 retained HA discovery entries published; HA picks up every
  entity.
- All six poll loops cycle on the configured cadence; pseudo-
  registers published alongside the raw values.
- Clean shutdown on `SIGTERM` — Modbus and MQTT both disconnect
  gracefully without leaving the inverter holding half-open sockets.

# Version 0.0.0 (2026-01-01)

Sentinel entry — anchors the changelog so the
`.github/workflows/release-on-tag.yml` workflow can compute the
"compare" range for `1.0.0`. Do not remove.
