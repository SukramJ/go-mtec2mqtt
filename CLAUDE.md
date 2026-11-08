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

## Which model does which work

Set by the owner for the go-fabric family on 2026-10-10 (go-fabric's
`CLAUDE.md`); the review split of 2026-11-08 (go-fabric #456) likewise.
Adopted here on 2026-11-08. A statement about the M-TEC Modbus registers or Home Assistant discovery that
lands in this repo is cited later as fact — a comment that names a caller
or a consumer, a docstring that states what the other side does, a
`changelog.md` line — so the work that produces such statements gets
the strongest model, and the work a guard checks may use a cheaper one. The
agents in [Sub-Agent Delegation](#sub-agent-delegation) carry a default
model each; the rows below decide when a call overrides it (`model:` on the
Agent tool). Implementation defaults to Opus.

| Work | Model |
| --- | --- |
| Analysis, planning, reviews of design, audits; anything written into `changelog.md` or a comment that names a consumer, or a comment that names a register semantic (`registers.yaml`), a Modbus behaviour or a Home Assistant discovery behaviour; the config loading with the broker and inverter settings (`internal/config/`), the web UI (`internal/web/`); *which* guard gets built and its bite line; rebases and merge conflicts where `main` may have overtaken the change | **Fable** (the default for this repo; also the `hunt` agent) |
| Implementation of a finished plan with guards behind it: a register once the pattern exists, a discovery field, a test on a recorded response, a `changelog.md` entry for a finished diff | **Opus** (the `impl` default; also what fast mode runs on) |
| **Review of a slice's pull request, split (owner decision 2026-11-08, go-fabric #456).** The code review — correctness, concurrency, error paths, tests, the fixed report shape — is **Opus**. On top of it **Fable** reads, bounded: every new comment or docstring that names a register semantic (`registers.yaml`), a Modbus behaviour or a Home Assistant discovery behaviour against `registers.yaml` with its `LICENSE.registers`, a recorded device response, go-hamqtt at its tag, every line that enters `changelog.md` or a comment that names a consumer, the paths of the first row, and the brief's decisions against the reference — the behavioural gaps a code review does not see. The load-bearing claims of the Opus review are verified before they reach the implementer. | **Opus**, then **Fable** |
| Work a test or generator checks immediately: `make fmt`, a table case; search and first reading of a tree whose result a stronger model or the owner reads again | **Sonnet** (the `guard` default; `impl` with `model: "sonnet"`) |
| Mechanical inventories whose answer is a list the caller can re-grep: which files reference X, which links are dead, which entries lack a counterpart | **Haiku** (`sweep` default) |

Sonnet never writes a claim about a register semantic (`registers.yaml`), a Modbus behaviour or a Home Assistant discovery behaviour into the repo, and
never touches the paths named in the first row. Haiku returns lists, never
verdicts. When in doubt about which row a task is in, it is in the first.

---

## Sub-Agent Delegation

As go-fabric's and openccu-loom's rule (owner, 2026-10-10). The main
conversation owns planning, contract and wire decisions, guard
specifications, sub-agent steering, and final verification. It does not own
typing.

**Delegate what a gate can prove; keep what only a reader can accept.**
Delegate freely: table cases, fakes, scaffolding, fixture updates, doc and
link cleanups, version bumps across the changelog set, inventories and grep
sweeps. Never delegate: `registers.yaml` semantics, the topic layout (mqtt-smarthome 2.0, ADR 0083), credential handling, and *which* guard gets
built. Locating is delegable; reading is not.

| Agent | Default model | Use for |
|---|---|---|
| `impl` | Opus | scoped implementation with a stated acceptance command |
| `guard` | Sonnet | tests from a caller-written guard spec, plus the bite proof |
| `sweep` | Haiku | read-only inventories and grep sweeps, high fan-out |
| `hunt` | Fable | adversarial read-only defect hunt, ranked candidates |

Each default is one row of
[Which model does which work](#which-model-does-which-work); a call that
falls into another row passes `model:` on the Agent tool — `impl` with
`model: "sonnet"` for work a generator or formatter checks immediately.

**Size CPU-bound fan-out from the host, never from a constant** — the owner
works from a 4-core box and a 14-core box:

```sh
cores=$(nproc); agents=$(( (cores - 1) / share ))   # share = cores per agent
```

Check the 1-minute load first; if it is at or above the core count, run one
CPU-bound agent and queue the rest. Eight concurrent agents is a practical
ceiling whatever the core count. Every CPU-bound agent must **pin** its share
in the command it runs (`GOMAXPROCS=<share> go test -p <share> ./internal/x/...`, `golangci-lint run --concurrency=<share>`) — handing out a number without
pinning it changes nothing. Read-only agents cost no core worth counting;
6–8 is a practical ceiling there.

Two hard rules: **no sub-agent runs the full gate (`make check`) or a
repo-wide lint** — the full gate runs once, in the main conversation, at the
end of the slice — and **no two writing agents in the same package**
(disjoint file sets, or `isolation: "worktree"`).

Every brief carries five things: the files and public surface plus one style
reference; the acceptance command the agent runs itself; the invariants it
touches; **stop conditions** (new dependency, API or wire change, credential
handling, an ADR-shaped decision — a stop is a successful outcome); and a
report format ≤ 250 words, large artefacts to the scratchpad as a path.

**A report is a claim.** Read the diff, run the gate yourself. For `guard`,
the acceptance artefact is not a green test but the **bite proof**: the named
production line removed, the test observed red with its message, the line
restored, green again.

---

## Token use

Set by the owner on 2026-10-14 ("Wichtig ist mir eine effiziente Nutzung
der Token"), as go-fabric's rule of the same date. A slice costs on
the order of half a million to a million tokens per implementing agent
before the owner's review; the rules below are what keeps that from
doubling.

- **Read slices, not files.** `sed -n a,bp`, `grep -n` with a tight
  pattern and `head`; a whole file only when the task is the file. Never
  re-read what is already in context; cite the earlier read.
- **Delegate first reads.** A source tree's inventory (go-mqtt, go-hamqtt, this repo)
  is a `sweep` or Sonnet read written to the scratchpad with `file:line`
  for every fact; the owner spot-checks the load-bearing lines and verifies
  every fact about the M-TEC Modbus registers or Home Assistant discovery before it enters a brief or
  `changelog.md` or a comment that names a consumer — the agent's result is a map, never a citation.
- **Logs by their summary.** The gate (`make check`) runs in the
  background with a summary file (one line per gate, `FAIL`/`ok` lines
  only); read the summary, and open a log only on failure, with `grep` for
  the failure markers and the error strings — never `cat`. The same for CI:
  the job log is fetched once to a file and grepped.
- **One turn, every independent call.** Reads, checks and background
  starts that do not depend on each other go out together; a wait is a
  `Monitor` or an `until` loop in the background, not polling by hand.
- **Short artefacts.** A brief names files, decisions and the acceptance
  command, not prose; a report follows the fixed shape of
  [Sub-Agent Delegation](#sub-agent-delegation) (≤ 250 words, large
  artefacts as a scratchpad path); status messages to the owner are short,
  outcome first, in German.
- **Skip what cannot have changed.** No regeneration for a comment change;
  no second gate run for a notes-only commit — say what was skipped and
  why.
- **Edit by script.** Several replacements in several files are one
  scripted call with asserted anchors, not a chain of interactive edits;
  a generated or formatted artefact is regenerated, never patched.
- **Agents read what the brief names.** An implementing agent gets the
  file list and the pinned sources in the brief, reads those, and reports
  in the fixed shape; it does not survey the tree. One agent per brief;
  two agents only on disjoint paths, at most one Opus agent at a time.
- **Push, pull request and tag are the session's own calls.** An
  implementing agent works locally — branch, commits, guards — and
  reports; the owner's session reads the diff, runs the gate, pushes,
  opens the pull request and tags the merge commit; the owner merges. A
  delegated push, pull request or tag is refused by the auto-mode
  classifier ("Create Public Surface", 2026-11-07), and the review in
  [Which model does which work](#which-model-does-which-work) sits
  between the agent's report and the push anyway.

---

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
