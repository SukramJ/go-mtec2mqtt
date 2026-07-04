# Version 1.5.0 (2026-07-04)

## What's Changed

### Changed

- **MQTT publishes are circuit-protected.** Upgraded to `go-mqtt` v1.1.0
  and adopted its new `Breaker` decorator on the coordinator's publish
  path: during a degraded-broker phase (TCP link up, acknowledgements
  missing) publishes fail fast with `ErrCircuitOpen` instead of each
  stalling on the full ack timeout. After 5 consecutive broker-side
  failures the circuit opens; after 30 seconds a single half-open probe
  tests recovery, and one success closes the circuit again. Local
  conditions (caller cancellation, oversized packets) never trip it.
  Every state transition is logged as a `mtec2mqtt.mqtt_breaker_state`
  warning. Subscriptions are deliberately not gated — they carry their
  own SUBACK-bounded wait and must keep working while the publish side
  is browned out. The lifecycle's reconnect loop remains in charge of
  the link itself.

# Version 1.4.0 (2026-07-04)

## What's Changed

This release hardens the Modbus write path and codec against malformed
input and anchors the transport with fuzz targets and poison-path tests.

### Fixed

- **`NaN` write payloads no longer silently write 0.** A `nan` value sent
  to a writable register's MQTT command topic slipped past the range
  checks (NaN compares false against every bound) and `uint16(NaN)` put 0
  on the wire — e.g. silently resetting a mode register. Such payloads are
  now rejected with a parse error, as are `+Inf`/`-Inf`.
- **Overflowing write payloads no longer wrap back into range.** A huge
  integer payload (e.g. `1844674407370955162` on a scale-10 register)
  overflowed int64 during scaling and could land back inside `0..65535`,
  writing an unintended value instead of failing. The bound is now checked
  before the multiplication.

### Changed

- **FC03 responses with a zero byte-count are rejected.** The Modbus spec
  requires at least one register in a read-holding response; a zero
  byte-count now fails decoding instead of yielding an empty result.
- **Upgraded to `go-mqtt` v1.0.0 — MQTT 5.0 is now the wire default.** The
  bridge negotiates MQTT 5.0 with the broker unless the code is pinned back
  to 3.1.1 via `TCPConfig.ProtocolVersion` (see the updated `MQTT`
  compatibility note in the README); brokers that only speak 3.1.1 keep
  working via that opt-out. Reconnects are now event-driven instead of
  polling `IsConnected()`, so a dropped link is retried immediately instead
  of waiting for the next poll tick. Command-topic subscriptions now block
  until the broker's SUBACK and a rejected filter is a hard startup error
  instead of a log line that was easy to miss. Publishing while the link is
  known to be down now fails fast instead of riding out the full ack
  timeout. The underlying client also gained full QoS 0/1/2 support in both
  directions (this bridge still only publishes/subscribes at QoS 0/1).

### Added

- **Fuzz targets for the Modbus codec.** Four `go test -fuzz` targets pin
  the hand-rolled MBAP codec's invariants (accepted headers/PDUs must be
  internally consistent, frames must round-trip); their seed corpora run
  as part of every `go test`.
- **Transport poison-path tests.** The mock Modbus server can now mutate
  responses (wrong transaction-id, wrong unit-id, bad protocol-id,
  truncated frame), asserting the client tears the connection down and the
  next call observes `ErrNotConnected` — the contract the coordinator's
  resilience layer relies on. Statement coverage: `internal/modbus/protocol`
  74 % → 100 %, `internal/modbus` 80 % → 97 %.

# Version 1.3.2 (2026-07-03)

## What's Changed

### Changed

- **Adopted `go-mqtt` v0.2.0.** Picks up the retained `MessageHandler` flag,
  per-filter QoS replay on reconnect, and a hardened ping watchdog — clients no
  longer see spurious `ping_timeout` reconnects.

# Version 1.3.1 (2026-07-02)

## What's Changed

### Docs

- Updated stale `internal/mqtt` documentation references to point at the
  extracted `github.com/SukramJ/go-mqtt` module (no functional change).

# Version 1.3.0 (2026-07-02)

## What's Changed

This release hardens the MQTT / Modbus / web surfaces and extracts the MQTT
client into a shared module.

### Security

- **MQTT frame-size cap.** The MQTT read path capped an incoming frame's
  `remaining length` only at the 256 MiB wire maximum and allocated the body
  buffer unconditionally, so a malicious or malfunctioning broker could force a
  multi-hundred-megabyte allocation per frame (OOM/DoS). Frames larger than
  1 MiB are now rejected before any allocation.
- **Web write-endpoint CSRF guard.** `POST /api/write` (which writes to the
  inverter) accepted any Content-Type, so a cross-site form/fetch could trigger
  a register write with the browser auto-attaching Basic-Auth credentials. It
  now requires `application/json` and rejects cross-site / cross-origin
  requests.
- **Web security headers.** `X-Content-Type-Options`, `Referrer-Policy` and a
  strict `Content-Security-Policy` are now set on every response (no
  `frame-ancestors`, so Home Assistant Ingress embedding keeps working).

### Added

- **Opt-in MQTT TLS.** New `MQTT_SSL` (and `MQTT_SSL_INSECURE` for
  operator-controlled self-signed certificates) config keys — MQTT credentials
  previously always crossed the wire in clear text. TLS always verifies the
  broker certificate (correct `ServerName`, TLS 1.2 minimum) unless explicitly
  disabled; defaults keep existing plain-TCP setups unchanged.
- **Rejected MQTT subscriptions are surfaced.** A broker SUBACK failure code
  (`0x80`) is now decoded and logged instead of silently leaving a command
  topic undelivered.

### Fixed

- **Register decode crash guard.** A `U32` / `S32` / `I32` register
  mis-declared with `length: 1` in `registers.yaml` would index out of bounds
  and panic the decoder; it now returns a bounds error instead.

### Changed

- **MQTT client extracted to `github.com/SukramJ/go-mqtt`.** The hand-rolled
  MQTT 3.1.1 client that lived under `internal/mqtt` is now the shared
  `go-mqtt` module (v0.1.0), used by all four `go-*2mqtt` bridges, so a fix
  lands once instead of drifting across four copies. No behavioural change for
  this daemon.

# Version 1.2.2 (2026-07-02)

## What's Changed

### Fixed

- MQTT half-open connections are now detected and recovered. The keep-alive loop
  sent PINGREQ but never checked that the matching PINGRESP came back, and the
  read loop runs without a read deadline — so a broker/network drop without a TCP
  FIN/RST (e.g. a Mosquitto or Home Assistant restart) left the read loop blocked
  in `ReadFrame` forever: the socket was never torn down, no reconnect happened,
  and QoS-1 publishes timed out with `context deadline exceeded` on the dead
  socket until a manual restart. A PINGRESP watchdog now declares the connection
  lost when a keep-alive ping goes unanswered, so the existing reconnect logic
  re-dials automatically (within one keep-alive interval).

# Version 1.2.1 (2026-07-01)

## What's Changed

### Fixed

- **Add-on invisible in Home Assistant.** After adding the repository the
  add-on never appeared in the store: the manifest `addon/config.yaml` had
  been excluded from git by the repo's blanket `config.yaml` `.gitignore`
  rule (intended only for the operator's runtime config), so it never
  reached `main`. Without a `config.yaml` the Supervisor does not recognise
  `addon/` as an add-on. The rule now negates the add-on manifest
  (`!/addon/config.yaml`) and the file is committed, so the add-on shows up
  and installs (the `ghcr.io/sukramj/go-mtec2mqtt-addon-{arch}` images were
  already published and public).

# Version 1.2.0 (2026-07-01)

## What's Changed

Adds a first-class **Home Assistant add-on** and the container-image
deployment pipeline it depends on.

### Added

- **Home Assistant add-on.** New `addon/` manifest (`config.yaml`,
  `build.yaml`, `Dockerfile`, `README.md`, `DOCS.md`) plus a top-level
  `repository.yaml` so the daemon installs from **Settings → Add-ons →
  Add-on Store → Repositories** (`https://github.com/SukramJ/go-mtec2mqtt`).
  Add-on options are mapped 1:1 onto the daemon's config keys by
  `script/run.sh` (a bashio entrypoint) and exported as `MTEC_*` env.
  - **MQTT zero-config:** leaving `mqtt_server` empty auto-connects to the
    Home Assistant MQTT broker (the configured MQTT integration /
    `core-mosquitto`) via the Supervisor's `mqtt` service, like
    zigbee2mqtt; `hass_enable` is on by default so entities appear
    automatically via MQTT discovery.
  - The diagnostic web UI is surfaced as a sidebar panel through **Ingress**
    (no exposed port required).
- **Container-image publishing.** `docker-build-push.yml` builds and pushes
  the multi-arch distroless daemon image `ghcr.io/sukramj/go-mtec2mqtt`
  (referenced by the README's `docker run` instructions), and
  `addon-image.yml` builds the per-arch add-on images
  `ghcr.io/sukramj/go-mtec2mqtt-addon-{arch}`. Both run on tag pushes.

### Changed

- **Environment-only configuration.** When no `config.yaml` is found (or
  supplied), the daemon now builds its configuration from `MTEC_*`
  environment variables and defaults alone instead of exiting. This lets
  the add-on (and env-only `docker run`) drive every setting without
  shipping a config file; `Validate` still enforces the required values.
- **Ingress-compatible web UI.** The embedded dashboard now uses relative
  API URLs so it works both when accessed directly and behind the Home
  Assistant Ingress path prefix.

# Version 1.1.2 (2026-06-27)

## What's Changed

### Added

- **Home Assistant discovery orphan cleanup.** After publishing the
  discovery configs the coordinator now reconciles the broker's retained
  config topics against the set it just published and clears any of *our
  own* stale ones (entities removed, renamed or re-platformed across catalog
  or daemon versions) by writing an empty retained payload, so they no
  longer linger as unavailable entities in Home Assistant. Cleanup is
  guarded by `Discovery.IsOwnConfig` (unique_id in the `MTEC_` namespace and
  state topic under our MQTT root), so the discovery configs of other
  integrations sharing the discovery prefix are never touched. The pass runs
  asynchronously and is gated so only one reconcile runs at a time.

# Version 1.1.1 (2026-06-15)

## What's Changed

### Fixed

- **Localised enum/select round-trip.** With `LANGUAGE: de` the inverter
  operation-mode select showed "unknown" and set-commands were silently
  dropped: the published enum state and the write-side label→code lookup
  still used the English value-items map while the Home Assistant select
  options were German, so the state matched no option and a German option
  sent back reverse-mapped to nothing. Published enum states now use the
  localised label so they match the select options, and the write path
  accepts the English **or** German label (`Register.CodeForLabel`), so a
  translated option maps back to the device regardless of language (numeric
  codes still work too).

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
