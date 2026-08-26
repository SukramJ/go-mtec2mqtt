# CLAUDE.md — AI Assistant Guide for go-mtec2mqtt

## Project Overview

**go-mtec2mqtt** is a pure-Go daemon that bridges an **M-TEC Energybutler**
(hybrid PV / battery inverter, GEN3) to an **MQTT broker**, with optional
**Home Assistant** MQTT auto-discovery. It polls 90+ registers over Modbus
TCP, publishes raw and computed pseudo-registers (`consumption`,
`autarky_rate_*`, `own_consumption_*`, `api_date`) to MQTT, and writes back
config/select/switch changes from Home Assistant to the inverter. It is a
Go port of the Python project
[`aiomtec2mqtt`](https://github.com/sukramj/aiomtec2mqtt), which itself
derives from Christian Rödel's
[`croedel/MTECmqtt`](https://github.com/croedel/MTECmqtt) — same YAML
config, same MQTT topic layout, same HA entities, drop-in replacement for
the Python daemon.

## Key Characteristics

- **Language**: Go 1.26+ (see `go.mod` / CI `GO_VERSION`).
- **Module path**: `github.com/SukramJ/go-mtec2mqtt`.
- **License: LGPL-3.0-or-later** (not MIT — this is a derivative of
  Christian Rödel's LGPL-3.0 `MTECmqtt`). Every Go source file starts
  with:
  ```go
  // SPDX-License-Identifier: MIT
  // Copyright (C) 2026 SukramJ
  ```
  Do not introduce MIT headers or copy conventions from unrelated
  SukramJ projects (e.g. `openccu-loom`, which is MIT) — the pure-Go
  MQTT client no longer lives in this repo; it is the external
  `github.com/SukramJ/go-mqtt` module (MIT, `openccu-loom` provenance,
  noted in the README), pulled in as a regular `go.mod` dependency, but
  the rest of this repo is LGPL.
- **Deployment**: two static binaries (`CGO_ENABLED=0`): the `mtec2mqtt`
  daemon and the `mtec-util` interactive register CLI. Also ships as a
  Docker image (distroless runtime) and a Home Assistant add-on
  (`addon/`).
- **Dependencies are deliberately minimal**: `golang.org/x/sync`
  (errgroup), `gopkg.in/yaml.v3`, and `github.com/SukramJ/go-mqtt`
  (the shared MQTT client extracted from `openccu-loom`, MIT — MQTT 5.0
  by default, 3.1.1 selectable via `TCPConfig.ProtocolVersion`).
  The Modbus MBAP codec remains hand-rolled in this repo (no
  third-party protocol deps there).
- **Config**: YAML (`config-template.yaml` is the annotated reference),
  overridable per-key via `MTEC_<KEY>` env vars. Loaded from
  `--config`, then `$XDG_CONFIG_HOME/aiomtec2mqtt/config.yaml` (or
  `$APPDATA` on Windows), then `~/.config/aiomtec2mqtt/config.yaml`.

## Repository Structure

```
cmd/mtec2mqtt/         daemon entry point (main.go)
cmd/mtec-util/         interactive register CLI (list/read/write registers)
internal/config/       YAML loader + MTEC_* env overlay + validation
internal/registers/    register catalog loader, type-aware decoders, address clustering
internal/modbus/       Modbus-TCP transport (own MBAP codec, no third-party deps)
internal/hass/         Home Assistant discovery payload builder
internal/coordinator/  orchestration: poll loops, pseudo-registers, write queue, virtual/equipment logic
internal/state/        thread-safe live-value cache (Store) shared between coordinator writers and web readers
internal/web/          optional diagnostic web UI / HA add-on Ingress panel (embedded static assets, no build step)
internal/version/      build-info package (Version/Commit/BuildDate, set via ldflags)
addon/                 Home Assistant add-on packaging (Dockerfile, config.yaml, DOCS.md)
script/                install.sh (curl|bash installer), run.sh, extract-release-notes.sh
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

- **License header** on every `.go` file: `SPDX-License-Identifier:
  LGPL-3.0-or-later` + `Copyright (C) 2026 SukramJ`.
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
- **Modbus wire correctness is cross-checked**: `internal/modbus`
  ships byte-for-byte goldenfile vectors generated from `pymodbus 3.13`
  (`internal/modbus/testdata/cross_check.py`) so the hand-rolled MBAP
  codec is anchored to a reference implementation.
- **Dependency licensing is enforced by tooling**, not just policy —
  `make licenses` (`go-licenses check --disallowed_types=forbidden,
  restricted,reciprocal`) blocks GPL/AGPL/LGPL/MPL-style dependencies
  from entering the tree even though the project itself is LGPL.
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
- For register semantics, cross-reference the Python ancestor
  `aiomtec2mqtt` / `croedel/MTECmqtt` — this project mirrors their
  register map and MQTT topic conventions by design.
