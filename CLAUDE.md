# CLAUDE.md — AI Assistant Guide for go-mtec2mqtt

## Project Overview

**go-mtec2mqtt** is a pure-Go daemon that bridges an **M-TEC Energybutler**
(hybrid PV / battery inverter, GEN3) to an **MQTT broker**, with optional
**Home Assistant** MQTT auto-discovery. It polls 90+ registers over Modbus
TCP, publishes raw and computed pseudo-registers (`consumption`,
`autarky_rate_*`, `own_consumption_*`, `api_date`) to MQTT, and writes back
config/select/switch changes from Home Assistant to the inverter. It began
as a Go port of the Python
[`aiomtec2mqtt`](https://github.com/sukramj/aiomtec2mqtt), which derives
from Christian Rödel's
[`croedel/MTECmqtt`](https://github.com/croedel/MTECmqtt) (the register
catalogue). Its MQTT topics follow mqtt-smarthome 2.0 (`<name>/status/…` as
`{"val","ts","lc"}`, `<name>/set/…`, `<name>/connected`, `<name>/info`,
maintenance; openccu-loom ADR 0083); Home Assistant discovery is ONE
retained device document per inverter. The Python daemon is no longer a
reference for behaviour.

## Key Characteristics

- **Language**: Go 1.27+ (see `go.mod` / CI `GO_VERSION`).
- **Module path**: `github.com/SukramJ/go-mtec2mqtt`.
- **License: two works, two licences.** The program (`cmd/`, `internal/`,
  everything but the catalogue) is **MIT** ([LICENSE](./LICENSE)); every
  Go source file starts with:
  ```go
  // SPDX-License-Identifier: MIT
  // Copyright (C) 2026 SukramJ
  ```
  The register catalogue `registers.yaml` is **LGPL-3.0-or-later**
  ([LICENSE.registers](./LICENSE.registers), [NOTICE.md](./NOTICE.md)):
  it derives from Christian Rödel's `croedel/MTECmqtt`, is loaded from the
  filesystem at run time and is never compiled into the binary. Keep it
  that way (no `go:embed` of registers.yaml).
- **Deployment**: two static binaries (`CGO_ENABLED=0`): the `mtec2mqtt`
  daemon and the `mtec-util` interactive register CLI. Also ships as a
  Docker image (distroless runtime) and a Home Assistant add-on
  (`addon/`).
- **Dependencies are deliberately minimal**: `golang.org/x/sync`
  (errgroup), `gopkg.in/yaml.v3`, and `github.com/SukramJ/go-mqtt`
  (the shared MQTT client extracted from `openccu-loom`, MIT — MQTT 5.0
  by default, 3.1.1 selectable via `TCPConfig.ProtocolVersion`).
  The Modbus-TCP transport (MBAP codec + client) is the shared
  `github.com/SukramJ/go-modbus` module (MIT, zero dependencies), also
  used by go-keba2mqtt; `internal/modbus` keeps only the catalog-aware
  `Reader` and type aliases for the transport. The web UI is the shared
  `github.com/SukramJ/go-bridgeui` module (MIT, zero dependencies), also
  used by go-keba2mqtt and go-zendure2mqtt; `internal/web` is only the
  adapter, the layout and the bridge's labels.
- **Config**: YAML (`config-template.yaml` is the annotated reference),
  overridable per-key via `MTEC_<KEY>` env vars. Loaded from
  `--config`, then `./config.yaml`, `$XDG_CONFIG_HOME/mtec2mqtt/config.yaml`
  (or `$APPDATA` on Windows), `~/.config/mtec2mqtt/config.yaml`; the
  directory of releases up to 2.x (`aiomtec2mqtt`) is searched after each.
  Keys removed in 3.0.0 (`MODBUS_FRAMER`, `MODBUS_RETRIES`,
  `MQTT_FLOAT_FORMAT`) are ignored with a warning (`config.removedKeys`).
- **Rounding**: floats go on the wire rounded to the register's
  `precision` (registers.yaml), default 3 decimals (`Register.Round`).

## Repository Structure

```
cmd/mtec2mqtt/         daemon entry point (main.go)
cmd/mtec-util/         interactive register CLI (list/read/write registers)
internal/config/       YAML loader + MTEC_* env overlay + validation
internal/registers/    register catalog loader, type-aware decoders, address clustering
internal/modbus/       catalog-aware Reader over github.com/SukramJ/go-modbus (transport aliases in client.go)
internal/hass/         Home Assistant discovery payload builder + topic functions
internal/evcc/         the shared evcc profile: Derive (grid/pv/battery, evcc signs), battery modes, docs contract test
internal/coordinator/  orchestration: poll loops, pseudo-registers, write queue, virtual/equipment logic, evcc battery mode, discovery publish
internal/state/        thread-safe live-value cache (Store) shared between coordinator writers and web readers
internal/web/          go-bridgeui adapter: the coordinator's state as the shared dashboard's contract, page layout + labels (i18n/)
internal/version/      build-info package (Version/Commit/BuildDate, set via ldflags)
addon/                 Home Assistant add-on packaging (Dockerfile, config.yaml, DOCS.md)
script/                install.sh (curl|bash installer), run.sh, extract-release-notes.sh
docs/evcc.md            evcc integration incl. the evcc.yaml snippet (checked by internal/evcc/docs_test.go)
registers.yaml          register catalog (operator-editable, not embedded — copied alongside the binary)
config-template.yaml     annotated reference config
.github/workflows/       ci.yml (lint/test/build), docker-build-push.yml, addon-image.yml, release-on-tag.yml, codeql.yml, dependabot-auto-merge.yml
```

The MQTT transport is not part of this tree: it comes from the external
`github.com/SukramJ/go-mqtt` module (MIT, `openccu-loom` provenance) as a
regular `go.mod` dependency, not an `internal/` package.

## Development Commands

All defined in the `Makefile` (`make help` lists them):

```sh
make build          # build both binaries into bin/ (build-daemon + build-util)
make run            # build-daemon then run against ./config.yaml + ./registers.yaml
make test           # go test -race -count=1 -timeout=60s ./... (CGO_ENABLED=1 for race detector)
make test-cover     # tests + coverage report (coverage.out)
make vet            # go vet ./...
make fmt            # gofumpt -w . && goimports -w -local <module> .
make fmt-check      # fail if gofumpt would rewrite anything (CI gate)
make lint           # golangci-lint run ./...
make vuln           # govulncheck ./...
make licenses       # go-licenses check, forbids GPL/AGPL/LGPL(reciprocal)/MPL deps
make check          # vet + fmt-check + lint + test — the pre-commit/pre-push gate
make docker         # build a tagged container image
make release        # cross-compile linux/amd64, linux/arm64, darwin/arm64 archives into dist/
make setup          # install gofumpt/goimports/golangci-lint/govulncheck/go-licenses + git hooks
make tidy           # go mod tidy
make clean          # remove bin/, dist/, coverage.out
```

Run a single package's tests directly with `go test ./internal/registers/...`
etc. — no special test runner beyond `go test`.

## Code Conventions Observed

- **The evcc profile is shared** with go-keba2mqtt and go-zendure2mqtt
  (`internal/evcc`, `docs/evcc.md`). Change its keys, units, signs or
  battery-mode semantics only in all three projects together.

- **License header** on every `.go` file: `SPDX-License-Identifier: MIT`
  + `Copyright (C) 2026 SukramJ`.
- **Identity strings are API**: `unique_id` (`MTEC_<key>`, serial-scoped
  under HASS_UNIQUE_ID_INCLUDE_SERIAL), `default_entity_id` (slugified
  English name), device identifier (bare serial) and node id. Pinned by
  `internal/hass/testdata/identity.json`; Home Assistant has no migration
  path for them.
- **No CGo** in the default build (`CGO_ENABLED=0`); CGo is only
  re-enabled transiently in CI/Makefile to get the race detector during
  `make test`.
- **`golangci-lint` v2** config (`.golangci.yaml`) enables: `bodyclose`,
  `contextcheck`, `copyloopvar`, `errcheck`, `errorlint`, `exhaustive`,
  `gocritic`, `gosec`, `govet`, `intrange`, `makezero`, `nilerr`,
  `noctx`, `prealloc`, `reassign`, `revive`, `sloglint`, `staticcheck`,
  `thelper`, `tparallel`, `unconvert`, `unparam`, `unused`,
  `usestdlibvars`, `wastedassign`.
- **Formatting**: `gofumpt` (stricter gofmt) + `goimports -local
  github.com/SukramJ/go-mtec2mqtt` for import grouping.
- **Structured logging**: `log/slog` (enforced by `sloglint`).
- **Package layout mirrors the data flow**: config → registers → modbus
  → coordinator → mqtt/hass/web, each as its own `internal/` package
  with colocated `_test.go` files.
- **Modbus wire correctness is cross-checked** in go-modbus: its
  `protocol` package ships byte-for-byte goldenfile vectors generated
  from `pymodbus 3.13` (`protocol/testdata/cross_check.py`). Reader tests
  here run against `go-modbus/modbustest`.
- **Dependency licensing is enforced by tooling**, not just policy —
  `make licenses` (`go-licenses check --disallowed_types=forbidden,
  restricted,reciprocal`) blocks GPL/AGPL/LGPL/MPL-style dependencies
  from entering the tree.
- **Git hooks**: `make setup` (or `make hooks`) points `core.hooksPath`
  at `.githooks/`, which blocks direct commits on `main`/`master`.
- **Commit style**: Conventional Commits with a scope, e.g.
  `feat(hass): add Home Assistant add-on...`, `fix(addon): ...`,
  `chore(deps): ...` (see `git log`).
- **Release bookkeeping — three files move together.** A version bump
  touches `internal/version/version.go` and `addon/config.yaml`, and
  every `changelog.md` entry must ALWAYS be mirrored into
  `addon/CHANGELOG.md` (the file Home Assistant renders in the add-on
  UI's Changelog tab) — keep the two changelog files identical.
- **CI** (`.github/workflows/ci.yml`) runs three jobs: `lint` (go vet +
  gofumpt check), `test` (matrix across ubuntu/macos/windows with the
  race detector), `build` (compiles both binaries and checks
  `--version` banner). Separate workflows publish the Docker image, the
  HA add-on image, and run CodeQL + Dependabot auto-merge.

## When in Doubt

- Read [`README.md`](./README.md) first — it documents the MQTT topic
  layout, config keys, quickstart paths (systemd installer, Docker, HA
  add-on, plain binary), and the inverter/firmware compatibility matrix.
- [`changelog.md`](./changelog.md) has the release history.
- [`config-template.yaml`](./config-template.yaml) documents every
  config field inline.
- [`addon/README.md`](./addon/README.md) and
  [`addon/DOCS.md`](./addon/DOCS.md) cover the Home Assistant add-on
  specifically.
- For register semantics, the catalogue's origin is `croedel/MTECmqtt`
  (and the M-TEC / Wattsonic GEN3 Modbus documentation evcc's
  `solinteg-gen3` template is built on).
