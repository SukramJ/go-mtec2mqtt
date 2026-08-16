# Version 1.9.0 (2026-08-16)

## What's Changed

### Fixed

- **Fault/alarm flags decoded with the wrong bit semantics** — the
  `hass_value_items` keys of the BIT registers (fault_flag_1/2, BMS
  error/protection/alarm) are bitmasks, but the decoder treated them as
  bit *positions*. A real "Mains Lost" fault published as `OK`, other
  faults carried a neighbour's label, and every mask ≥ 64 could never
  fire. Unparseable bit fields now report `Unknown` instead of a
  fail-open `OK`. (Inherited from the Python ancestor, which has the
  same bug.)
- **`curl | bash` installer was broken for every release since v1.2.0**:
  the download URL used the bare version as the release tag
  (`…/download/1.8.0/…` → 404); it now uses the real `v`-prefixed tag
  with a bare-tag fallback for the old pre-`v` releases, and backs up an
  existing systemd unit instead of silently overwriting hand edits.
- **Battery temperature registers (33003/33009/33011) are now signed
  (I16)** — −0.5 °C used to publish as 6553.1 °C straight into HA
  long-term statistics. Positive readings are unchanged. (Deliberate
  divergence from `aiomtec2mqtt`, which still decodes them unsigned.)
- **Enum sensors now advertise the mandatory `options` list** in their
  HA discovery payload (inverter/BMS status); the BIT fault-flag sensors
  drop `device_class: enum` (their comma-joined fault lists cannot be
  enumerated) and publish as plain text sensors — label conversion is
  now keyed on `hass_value_items`, not on the device class.
- **Pseudo-registers are no longer computed from incomplete reads**: a
  failed cluster read used to feed 0 into `consumption`/`autarky`/
  `own_consumption`, publishing confidently wrong values into HA
  statistics; affected pseudo-registers are now skipped for that cycle.
- **Discovery republish is reliable**: `discoverySent` is only latched
  after every config published successfully and no HA birth arrived
  mid-publish — a broker brownout during startup no longer leaves HA
  without entities until the next restart.
- **Write path hardening**: a full write queue now drops the oldest
  command instead of the newest (an HA slider ends on its target value,
  not a stale intermediate), web UI writes are serialized through the
  same queue as HA commands, transient Modbus errors are retried, a
  failed startup subscribe retries with backoff instead of killing the
  daemon, and the discovery-reconcile unsubscribe retries instead of
  leaking a permanent `homeassistant/+/+/config` subscription.
- **Modbus transport**: a cancelled caller's deadline hook can no longer
  fire late and poison the *next* healthy transaction; `IsConnected()`
  answers instantly instead of blocking on an in-flight transaction (up
  to `MODBUS_TIMEOUT`); `Close()` interrupts in-flight I/O for a prompt
  shutdown; MQTT write payloads are whitespace-trimmed.
- **Register catalog loader** now rejects type/length contradictions
  (e.g. `U32` with `length: 1` used to fail on every poll with no
  startup diagnostic), warns about unknown YAML fields (a `writeable:`
  typo was silently ignored), rejects explicit `scale: 0`/`length: 0`
  and duplicate addresses via leading zeros; `U32` decodes via `int64`
  (no more wrap to −1 on 32-bit/armv7 builds); string registers strip
  `0xFF` padding (no more `ÿÿÿÿ` serial numbers in topics and HA device
  identifiers).
- **Config loading**: an explicit `REFRESH_*: 0` now fails validation
  instead of being silently replaced by the default, presence checks are
  case-insensitive (lowercase keys no longer lose explicit zeros),
  `MTEC_*` env values are whitespace-trimmed, and a merge failure names
  the applied env keys. `config-template.yaml` no longer documents the
  invalid `binary` framer or a one-space `MQTT_LOGIN`.
- **Web UI**: all routes now carry a write deadline (slow-read clients
  can no longer pin goroutines/file descriptors forever — previously
  only SSE and the write endpoint were protected), `web.listening` is
  logged only after a successful bind, the CSRF origin check trusts
  `Sec-Fetch-Site: same-origin` (robust behind the HA Ingress proxy),
  raw Modbus transport errors stay in the server log instead of the API
  response, JSON encode failures are logged, and the frontend i18n no
  longer passes user input as a `String.replace` replacement pattern.
- **mtec-util** now supports the env-only (`MTEC_*`) configuration mode
  like the daemon, connects lazily (catalog listing works offline; no
  more silent multi-second connect before the menu) with a visible
  "connecting…" notice, and gained `--version`.
- **CI/workflows**: `workflow_dispatch` tag inputs are passed via `env:`
  (expression-injection fix), image/release workflows only trigger on
  `v*.*.*` tags (an arbitrary tag can no longer overwrite `:latest`),
  and a new `audit` CI job runs `make vuln` + `make licenses` on every
  push — the license/vulnerability gates previously existed only as
  local Makefile targets.
- **Toolchain updated to Go 1.26.6**, fixing four reachable stdlib
  vulnerabilities (GO-2026-6090, GO-2026-6089, GO-2026-5972,
  GO-2026-5856) present when building with 1.26.4.

### Added

- **`HASS_UNIQUE_ID_INCLUDE_SERIAL`** (config + add-on option, default
  off): opt-in serial-scoped HA unique_ids (`MTEC_<serial>_<key>`) so
  several daemon instances — one per inverter — can share one Home
  Assistant installation without overwriting each other's retained
  discovery configs. Enabling it on an existing install creates fresh
  entities and orphans the old ones (no unique_id migration in MQTT
  discovery), hence the explicit opt-in.
- **`mtec2mqtt --healthcheck`** probes the local web UI health endpoint
  (exit 0/1); the Docker image now ships a matching `HEALTHCHECK` so
  restart policies can detect a hung daemon.
- **linux/arm (armv7) release binaries** — `make release` and the
  installer now cover 32-bit ARM, matching the Docker image and HA
  add-on architectures.

# Version 1.8.0 (2026-08-16)

## What's Changed

### Changed

- **MQTT client updated to `github.com/SukramJ/go-mqtt` v1.3.0**, an
  audit release fixing 42 findings in the transport. No API changes
  were needed on this side — `TCPConfig`, `Lifecycle`, and `Breaker`
  usage here compiles and behaves unchanged.
- **Reconnect flap damping is on by default now** (new
  `LifecycleConfig.FlapWindow`, defaulting to 10s, applies
  automatically since the daemon builds its lifecycle from
  `mqtt.DefaultLifecycle()`): if the broker link dies again within 10s
  of a (re)connect, the daemon now backs off exponentially instead of
  reconnecting immediately. A broker that stays up longer than that
  between drops sees no change.
- **The publish-side circuit breaker no longer opens on client-side
  validation errors** (malformed topic/payload) — only genuine
  broker-side ack failures trip it now, so a single bad outbound
  publish can no longer stall unrelated publishes behind
  `mqtt.ErrCircuitOpen`.
- Graceful shutdown (`mqttLifecycle.Stop`) no longer risks a spurious
  reconnect racing the intentional disconnect.

# Version 1.7.0 (2026-07-07)

## What's Changed

### Fixed

- **Daemon hardening — 25 audit findings fixed across the whole stack**
  (#31). Highlights: the coordinator no longer replays retained `/set`
  commands to the inverter on restart, validates the inverter serial
  before splicing it into MQTT topics, fixes a `topicBase` data race,
  and actually republishes discovery on the HA birth message; Modbus
  honors context cancellation during in-flight I/O, validates FC03
  response lengths, and retries the initial connect with backoff;
  `registers.yaml` is validated at load (address space, types,
  duplicate keys, MQTT wildcards) and clusters are capped at the
  125-register FC03 limit; the web UI gets idle/read/SSE deadlines and
  clean shutdown; startup retries MQTT with backoff, exits 0 on clean
  SIGTERM, and publishes the retained LWT online/offline correctly;
  `mtec-util` enforces the write guard and scale coercion on the
  direct write path. MQTT topic layout, config keys, and HA discovery
  payloads are unchanged.

### Changed

- **MQTT client updated to `github.com/SukramJ/go-mqtt` v1.2.0**, a
  hardening release fixing 28 audit findings in the transport:
  serialised connect/disconnect, generation-counted send quota and
  packet-id allocator (no cross-reconnect leaks), typed ack waiters
  that reject forged/mismatched acknowledgements, and spec-correct
  resumed-session replay ordering. No API or behavior change for
  well-behaved brokers.
- Dependency and GitHub Actions updates (#30).

# Version 1.6.2 (2026-07-06)

## What's Changed

### Fixed

- **Home Assistant entity ids now match the Python `aiomtec2mqtt`
  scheme.** 1.6.1 seeded the `entity_id` from the short mqtt topic key
  (`grid_a`, `mode`, `pv_1`, `backup_day`, …), which diverged sharply
  from the established ids. The Python reference sets no `object_id` and
  lets HA build the id from the register **name**, so the seed is now the
  slugified English register **name** to match — e.g. register `grid_a` /
  "Grid power phase A" → `sensor.<device>_grid_power_phase_a` instead of
  `sensor.<device>_grid_a`. The English name (never the localised display
  name) keeps ids language-independent; `unique_id` / MQTT topics still
  key on the mqtt suffix, so no existing entity is renamed.

# Version 1.6.1 (2026-07-06)

## What's Changed

### Fixed

- **Home Assistant entity ids no longer follow the translated friendly
  name.** 1.6.0 published only `default_entity_id` to seed the
  `entity_id`, but current HA Core does not yet apply that option
  reliably in MQTT discovery (home-assistant/core#157241) — it falls back
  to slugging the localised `name`, so with `LANGUAGE: de` the entity ids
  came out German (e.g. `sensor.netzleistung`). The discovery payloads now
  publish **both** seeds: `object_id` (`"<key>"`, still honoured by
  today's HA) and `default_entity_id` (`"<domain>.<key>"`, its successor
  for HA Core 2026.4+ once `object_id` is removed). Both are derived from
  the language-independent register key (optionally the device-name slug),
  never the translated name, so entity ids stay English while only the
  display name follows `LANGUAGE`. `unique_id` is unchanged, so no
  existing entity is re-created.

# Version 1.6.0 (2026-07-06)

## What's Changed

### Added

- **Optional device name.** A new `DEVICE_NAME` config key (add-on option
  `device_name`, env `MTEC_DEVICE_NAME`) lets you give the inverter a
  friendly name. When set it becomes the Home Assistant device name
  (instead of the generic "MTEC EnergyButler") and is slugged into every
  entity's entity-id seed, so Home Assistant seeds fresh entity ids like
  `sensor.<device_name>_grid_power` — handy to tell multiple inverters
  apart. The seed is always the English register key (never the localised
  friendly name), so entity ids stay language-independent; only the
  display name follows `LANGUAGE`. The entity `unique_id` is deliberately
  left unchanged, so enabling this on an existing install does not orphan
  established entities or lose their history/customisations — only newly
  created entities pick up the nicer id (the seed only sets the initial
  id; HA tracks entities by `unique_id`). Leaving it empty preserves the
  previous identity exactly. The MQTT topic tree also stays keyed on the
  inverter serial regardless, and the device-registry
  `identifiers`/`serial_number` stay the serial, keeping the device entry
  stable across a rename.

### Changed

- **Home Assistant discovery migrated off the deprecated `object_id`
  option.** HA Core deprecated the `object_id` discovery option in 2025.10
  in favour of `default_entity_id` and removes it in 2026.4, so the
  payloads switched to publishing `default_entity_id` (`"<domain>.<key>"`)
  to seed the `entity_id`. (Superseded by 1.6.1, which also re-adds
  `object_id` — see above.)

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
