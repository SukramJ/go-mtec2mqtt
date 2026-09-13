# ADR 0070 phase 6 — measurement for go-mtec2mqtt

- Status: **closed**. Sections 1-7 and findings F1-F11 are the measurement,
  which took no decision and fixed nothing; the closing section *Phase 6
  outcome* records every finding's disposition and every place the
  measurement has since been overtaken.
- **Convention: sections 1-7 and the findings are a frozen snapshot.** They
  say what was true on 2026-09-13, against `origin/main` at `d402f71` and
  `go-hamqtt` v0.31.0, and they are never edited to match what is true now.
  Every correction is carried in *Phase 6 outcome* at the end instead. So a
  passage above may be stale by design; it is not wrong, and the outcome
  section is the authority wherever the two disagree. The one thing this
  document must never do is read as an open question that has since been
  closed — see the outcome section's *Corrections* first.
- Date: 2026-09-13
- Subject: [ADR 0070](https://github.com/SukramJ/openccu-loom/blob/main/docs/adr/0070-shared-ha-discovery-model-module.md)
  and its rollout table, row *"6 | `go-mtec2mqtt` (591) | Fixes
  retain/availability/slugify bugs as a side effect"*
  (`notes/concepts/shared-ha-discovery-model.md:868`)
- Measured against: this repository at `origin/main` (`d402f71`),
  `github.com/SukramJ/go-hamqtt` v0.31.0 (`41215a5`),
  `github.com/SukramJ/go-mqtt` v1.3.0 (consumed) / v1.5.1 (required),
  `github.com/SukramJ/go-ha-catalog` v0.2.1
- Template: `go-zendure2mqtt`'s `docs/adr0070-pilot-measurement.md`
  (phase 5, 2026-09-12) and its PRs #39–#45

This document measures what phase 6 costs, before any code moves. It is the
mtec counterpart of the zendure pilot measurement. No Go file in any
repository was modified. Defects found while reading are recorded in
[Findings](#findings) and were not fixed.

Every count below is measured and the method is stated. Where a number could
be got two ways, both are given. Where the "after" cannot be established
without running code, the section says so and names what would settle it.

> **Why `notes/`.** This repository has no `docs/` or `notes/` tree. Its
> markdown is five files at the repository root (`README.md`, `CLAUDE.md`,
> `AI_POLICY.md`, `NOTICE.md`, `changelog.md`) plus three under `addon/`,
> and every one of them is a *shipped* artefact: `CLAUDE.md:134-152` treats
> the root markdown as the operator-and-agent-facing set and requires
> `changelog.md` to be mirrored into `addon/CHANGELOG.md` for the Home
> Assistant add-on UI. A working measurement placed in that flat root would
> read as one more shipped document. So the brief's fallback applies:
> `notes/adr0070-phase6-measurement.md`, a new directory whose only member
> is working material. `docs/` was deliberately not used, because that is
> zendure's convention and not this repository's.

---

## 1. What is actually there

### 1.1 The whole repository

Measured over `git archive origin/main` extracted to a scratch tree, with

```sh
find . -name '*.go' -not -name '*_test.go' | xargs cat | wc -l   # 6722
find . -name '*_test.go'                   | xargs cat | wc -l   # 8015
```

| | Lines |
| --- | ---: |
| Go, non-test | **6 722** |
| Go, test | **8 015** |
| `registers.yaml` (the catalog) | 1 448 |
| `changelog.md` | 33 570 bytes |

Per package, same method, `-maxdepth 1` per directory:

| Package | Non-test | Test |
| --- | ---: | ---: |
| `cmd/mtec2mqtt` | 423 | 216 |
| `cmd/mtec-util` | 546 | 329 |
| `internal/config` | 777 | 655 |
| `internal/coordinator` | 1 895 | 1 948 |
| **`internal/hass`** | **576** | **734** |
| `internal/modbus` | 582 | 1 543 |
| `internal/modbus/protocol` | 232 | 424 |
| `internal/registers` | 883 | 1 245 |
| `internal/state` | 245 | 155 |
| `internal/version` | 28 | 0 |
| `internal/web` | 535 | 766 |

Two shapes worth naming before the numbers are used:

- **`internal/hass` is a single file.** `discovery.go` (576) and
  `discovery_test.go` (734). There is no `cleanup.go` equivalent —
  `ConfigFilter` and `IsOwnConfig` live in the same file
  (`internal/hass/discovery.go:306` and `:321`). So "the package", "the
  directory" and "the file" are the same number here, which is not true of
  zendure and matters for §1.2.
- **This repository is 1.8× zendure's non-test size and 8.2× its test size.**
  The test suite is the largest single fact about it: 8 015 test lines
  against 6 722 non-test. That changes the pinning answer in §4 completely
  relative to the pilot.

### 1.2 The 591 LOC figure

**591 is `internal/hass/discovery.go`, non-test, at commit `410ca08`, and it
is now 576.** Measured directly:

```sh
for c in $(git log --format=%h -20 -- internal/hass/); do
  echo "$c $(git show $c:internal/hass/discovery.go | wc -l)"
done
```

| Commit | Lines | Subject |
| --- | ---: | --- |
| `d402f71` (`origin/main`) | **576** | `refactor(hass): drop the dead object_id discovery key (#48)` |
| `410ca08` | **591** | `feat(license): relicense the program to MIT (#44)` |
| `f73503a` | 591 | `fix: resolve all 39 findings from the full-codebase audit (#41)` |

So the figure is **right about what it covers and fifteen lines stale**, the
staleness caused by the same class of commit that made zendure's 375 three
lines stale: dropping the `object_id` key. Both bridges removed it
independently, and `go-hamqtt` removed it by construction
(`discovery/bundle.go:82-86`, CHANGELOG.md:1851-1856).

The pilot's correction was that the recorded figure covered one directory
while the real addressable surface was ~672. The same correction applies here
and is larger, because mtec's MQTT plane is spread across four more files:

| Also in scope for the migration | File:lines | Lines |
| --- | --- | ---: |
| Discovery payload assembly, identity policy, slugify, device block, ownership test | `internal/hass/discovery.go` (whole file) | 576 |
| Command subscribe + retry | `internal/coordinator/coordinator.go:295-327` | 33 |
| Inbound dispatch (birth half + command half) | `coordinator.go:372-414` | 43 |
| HA birth gracetime wait | `coordinator.go:454-461` | 8 |
| Discovery publish loop | `coordinator.go:559-589` | 31 |
| Discovery republisher (birth-driven) | `coordinator.go:597-611` | 15 |
| Topic-shape helpers used by the command path | `coordinator.go:726-759` | 34 |
| State publish loop | `internal/coordinator/poll.go:149-157` | 9 |
| Orphan reconcile (subscribe, collect, unsubscribe, retract) | `internal/coordinator/reconcile.go:43-146` | 104 |
| MQTT bootstrap, will, breaker, lifecycle | `cmd/mtec2mqtt/main.go:124-187` | 64 |
| Availability announce | `main.go:281-289` | 9 |
| `mqttSession` (the hand-rolled `SplitClient`) | `main.go:392-407` | 16 |
| **Total addressable surface** | | **942** |
| Its tests | `discovery_test.go` (734) + `reconcile_test.go` (79) + the MQTT half of `coordinator_test.go` + `main_test.go` | ≥ 1 000 |

Line ranges are brace-matched function extents, not `awk`-to-next-`func`
spans, so a function's trailing doc comment for the *next* function is not
counted. The helper block `coordinator.go:726-759` is four functions
(`startsWith`, `endsWith`, `isTopicSafe`, `splitPath`); only the first,
second and fourth serve the command path — `isTopicSafe` guards the serial
and stays.

**942, not 591.** The ratio to the recorded figure (1.6×) is close to
zendure's (672/375 = 1.8×), which suggests the rollout table's figures are
consistently "the discovery package only" across all six rows and should be
read that way.

### 1.3 Does it still carry the pre-extraction `internal/mqtt`? No.

Removed in commit `394a0a8`, *"feat: security hardening + shared go-mqtt
module (1.3.0) (#20)"*, which deletes
`internal/mqtt/{adapter_tcp,client,lifecycle,test_mock_broker}.go`,
their tests and `internal/mqtt/protocol/`. Verified with

```sh
git log --oneline --diff-filter=D --name-only -- 'internal/mqtt/*'
git ls-tree -r origin/main --name-only | grep -i internal/mqtt   # empty
```

Today's `go.mod` is four lines of `require`:

```
require (
	golang.org/x/sync v0.22.0
	gopkg.in/yaml.v3 v3.0.1
)
require github.com/SukramJ/go-mqtt v1.3.0
```

**`go-mqtt` v1.3.0. `go-hamqtt` v0.31.0 requires `go-mqtt` v1.5.1.** That is
a two-minor gap, not one, and it is wider than the pilot's:

- **v1.4.0** is where `mqtt.SplitClient(p Publisher, s Subscriber) Client`
  landed. This repository hand-rolls it as `mqttSession`
  (`cmd/mtec2mqtt/main.go:392-407`), sixteen lines including the
  compile-time contract assertion at `:404-407`. It deletes itself on the
  bump, independently of anything else here, and two of `main_test.go`'s
  seven tests (`TestMQTTSessionPublishIsCircuitGated:51`,
  `TestMQTTSessionSubscribeBypassesBreaker:77`) become tests of library code.
- **v1.5.1** is required because through v1.5.0 an identifier-less delivery
  was matched by topic against stamped subscriptions, so a consumer's own
  broad subscription could run a handler twice per published message. Worth
  checking against this bridge specifically, because its command filter *is*
  broad: `MTEC/+/+/+/set` (`coordinator.go:307`). Its three filters —
  `homeassistant/status` (`coordinator.go:306`, 2 segments),
  `MTEC/+/+/+/set` (5 segments) and the transient
  `homeassistant/+/+/config` (`internal/hass/discovery.go:307`, 4
  segments) — are pairwise non-overlapping by segment count alone, so this
  bridge is very likely *not* exposed to the v1.5.0 defect. The bump is
  required regardless.

There is **no** `go-hamqtt` and no `go-ha-catalog` in `go.mod`, and no CI
preparation for either (unlike zendure, which had already excluded
`go-ha-catalog` from Dependabot auto-merge before it had the dependency).

---

## 2. How it builds discovery payloads

**Hand-built `map[string]any`, one retained config per entity**, assembled in
five per-platform builders (`internal/hass/discovery.go:404`, `:438`, `:460`,
`:480`, `:496`) plus one for synthetic switches (`:385`), marshalled by
`appendEntry` (`:525`) with `encoding/json`. No struct, no template, no
validation — a marshal error is a `panic` (`:528`). The ADR's payload-style
column is correct.

### 2.1 The shape

| Concern | Code | Value |
| --- | --- | --- |
| Config topic | `discovery.go:539-541` | `<hass_base>/<platform>/<unique_id>/config` — **four** segments, no `node_id` level |
| Config filter for the sweep | `discovery.go:306-308` | `<hass_base>/+/+/config` |
| `unique_id` | `discovery.go:246-251` | `MTEC_<mqtt_key>`, or `MTEC_<serial>_<mqtt_key>` under `HASS_UNIQUE_ID_INCLUDE_SERIAL` |
| `default_entity_id` | `discovery.go:225-227` | `<platform>.` + `slugify(englishName)`, or `<platform>.<deviceSlug>_<slug(englishName)>` |
| `object_id` | — | **not published** since #48 (`d402f71`) |
| `node_id` | — | **no such concept.** Nothing occupies that level |
| Device `identifiers` | `discovery.go:273` | `[<serialNo>]` — the bare serial, **no namespace prefix at all** |
| Sub-devices / `via_device` | — | **none.** One HA device per daemon, flat |
| Availability | — | **absent from every payload.** See [F1](#f1) |
| `origin` | — | **absent**, as the ADR predicted for all six consumers |
| `entity_category` | — | absent |
| Idempotency | — | none; every entry is republished on every pass (`coordinator.go:568-576`) |

`<hass_base>` is `config.HASSBaseTopic`, default `homeassistant`
(`internal/config/defaults.go:30`). `<mqtt_topic>` is `config.MQTTTopic`,
default `MTEC` (`internal/config/config.go:22`), exposed as the add-on
option `mqtt_topic` (`addon/config.yaml:60`, schema `str`).

**The `MTEC_` namespace is a compile-time constant** (`discovery.go:31`),
*not* the configurable MQTT root. This is the one place where this bridge is
straightforwardly better than the pilot; see [§6.4](#64-what-this-bridge-does-better).

### 2.2 The slug

`slugify` (`discovery.go:178-194`) lower-cases, keeps `[a-z0-9]`, folds every
run of anything else to a single `_`, and trims. **No transliteration.**
`Größe` → `gr_e`; `ÜÄÖ` → `""`. This is the defect ADR 0070 records at line
50 and §8.2 line 118, and it is *pinned by a test*:
`discovery_test.go:535-553`, cases `"ÜÄÖ": ""` and `"  PV-Dach #2  ":
"pv_dach_2"`. See [§5.3](#53-slug-and-naming) for what changing it costs,
measured over the real catalog.

### 2.3 Platforms and entity count

The catalog declares six platforms (`discovery.go:39-46`). Measured from
`registers.yaml` by running the real loader:

| | Count |
| --- | ---: |
| Catalog entries with a `group:` | 94 |
| …of which carry HA hints (`HasHassHints`) | **89** |
| …of which are `writable:` | 9 |
| `hass_component_type:` values present | `number` ×9, `switch` ×3, `select` ×1, `sensor` ×1 |
| Synthetic switches minted in code | 2 (`discovery.go:92-111`) |

`buildEntries` (`discovery.go:344-379`) emits **two** entities for three of
the platforms: `number`+`sensor`, `select`+`sensor`, `switch`+`binary_sensor`
(lines 360-368). So 89 annotated registers + 9 dual emissions + 2 synthetic
switches = **100 entities**.

Ran through the real code — `registers.Load("registers.yaml")` →
`hass.New("homeassistant", "MTEC", …, "en", DefaultVirtualSwitches(…), "")` →
`Initialize("MT1234567890", "1.2.3", "EB-10kW")` → `Entries()`, in a
throwaway copy of `origin/main`. The measured result for **one M-TEC
Energy-Butler is 100 entities on 1 HA device**:

| Platform | Count |
| --- | ---: |
| `sensor` | 86 |
| `number` | 5 |
| `switch` | 5 (3 real + 2 synthetic) |
| `binary_sensor` | 3 |
| `select` | 1 |
| `button` | **0** — declared, dispatched to an empty case ([F6](#f6)) |
| **Total** | **100** |

Per-key union across all 100 payloads, with the count of payloads carrying
each key:

| Key | Count |
| --- | ---: |
| `default_entity_id`, `device`, `enabled_by_default`, `name`, `state_topic`, `unique_id` | 100 |
| `unit_of_measurement` | 87 |
| `state_class` | 77 |
| `value_template` | 77 |
| `device_class` | 76 |
| `command_topic` | 11 |
| `payload_on`, `payload_off` | 8 |
| `options` | 5 |
| `mode` | 5 |
| `availability_topic` / `availability` / `payload_available` | **0** |
| `origin` | **0** |
| `object_id` | **0** |

**91 distinct `unique_id`s across 100 config topics.** Nine `unique_id`s are
published twice, under two different platforms, because of the dual emission:

| `unique_id` | Platforms |
| --- | --- |
| `MTEC_charge_limit`, `MTEC_discharge_limit`, `MTEC_grid_inject_limit`, `MTEC_off_grid_soc_limit`, `MTEC_on_grid_soc_limit` | `number` + `sensor` |
| `MTEC_grid_inject_switch`, `MTEC_off_grid_soc_switch`, `MTEC_on_grid_soc_switch` | `binary_sensor` + `switch` |
| `MTEC_mode` | `select` + `sensor` |

This is legal in Home Assistant's per-entity form — the registry key is
(entity domain, integration, `unique_id`), and `sensor` and `number` are
different domains. **Whether it is legal inside one device bundle is the one
question this migration cannot answer by reading**, and it is
[§3.3](#33-what-blocks-the-rest-of-the-after-column)'s first item. It has no
analogue in the pilot: zendure emits exactly one entity per catalog entry.

Likewise 91 distinct `state_topic`s: the nine pairs share a state topic.

### 2.4 Two measured payloads, verbatim

`homeassistant/sensor/MTEC_grid_power/config`, captured from `origin/main`
(serial and firmware are plausible stand-ins, the rest is real output):

```json
{
  "default_entity_id": "sensor.grid_power",
  "device": {
    "identifiers": ["MT1234567890"],
    "manufacturer": "M-TEC",
    "model": "Energy-Butler",
    "model_id": "EB-10kW",
    "name": "MTEC EnergyButler",
    "serial_number": "MT1234567890",
    "sw_version": "1.2.3"
  },
  "device_class": "power",
  "enabled_by_default": true,
  "name": "Grid power",
  "state_class": "measurement",
  "state_topic": "MTEC/MT1234567890/now-base/grid_power/state",
  "unique_id": "MTEC_grid_power",
  "unit_of_measurement": "W",
  "value_template": "{{ value | round(0) }}"
}
```

`homeassistant/select/MTEC_mode/config`, the only `select`:

```json
{
  "command_topic": "MTEC/MT1234567890/now-base/mode/set",
  "default_entity_id": "select.inverter_operation_mode",
  "device": { "…": "as above" },
  "enabled_by_default": false,
  "name": "Inverter operation mode",
  "options": ["General mode", "Economic mode", "UPS mode", "Off grid"],
  "state_topic": "MTEC/MT1234567890/now-base/mode/state",
  "unique_id": "MTEC_mode"
}
```

Four things in there are load-bearing for §3:

- **`device.identifiers` is the bare serial** — no `MTEC_` prefix, no
  namespace, no separator. `"MT1234567890"`. Any library default that builds
  an identifier from a slug or a namespaced value changes the device registry
  key, and the device registry has no migration path either.
- **`default_entity_id` seeds from the English register *name*, not from the
  mqtt key** (`discovery.go:196-212`), deliberately, to stay a drop-in
  replacement for the Python `aiomtec2mqtt`. Alone among the six consumers
  (ADR 0070 §8.2:125). So `sensor.grid_power` comes from `"Grid power"`, and
  `select.inverter_operation_mode` from `"Inverter operation mode"` — while
  its `unique_id` is `MTEC_mode`, from the mqtt key. **The two identity
  strings are derived from two different source fields.** That is a
  reproducibility question for §3 and it is answerable: both are pure
  functions of catalog data.
- **`options` are localised labels**, and the published state is the matching
  localised label (`internal/coordinator/process.go:28-31`). A `LANGUAGE`
  change rewrites both the `options` list and every state payload of every
  enum entity. Not new, but it is a payload the migration must not move by
  accident.
- **`enabled_by_default` is `false` on 9 of 100** — every writable entity's
  `number`/`select`/`switch` view (`discovery.go:466`, `:486`, `:502`), but
  `true` on the two synthetic switches (`discovery.go:391`). That
  inconsistency is [F7](#f7).

### 2.5 The topic schema

There is no topic module; the four builders are methods on `Discovery` and
one `fmt.Sprintf` in the poll loop:

```
MTEC/<serial>/<group>/<mqtt_key>/state     discovery.go:545-548, poll.go:150
MTEC/<serial>/<group>/<mqtt_key>/set       discovery.go:552-555
MTEC/<serial>/<group>/<key>/set            discovery.go:387 (synthetic switch)
homeassistant/status/lwt                   main.go:130   ← see F2
homeassistant/status                       coordinator.go:201 (subscribed)
```

`<group>` is one of nine: `config`, `now-base`, `now-grid`, `now-backup`,
`now-battery`, `now-inverter`, `now-pv`, `day`, `total`, plus `static`
(polled, published, no entities). **Three of them contain a hyphen**, which
matters for `topic.Layout` (§5.2) and for `topic.Safe`.

The state topic is built in **two places** from two different expressions:
`discovery.go:545-548` (`d.mqttTopic + "/" + d.serialNo + …`) and
`poll.go:150` (`topicBase + "/" + group + "/" + key`, where `topicBase` is
`cfg.MQTTTopic + "/" + serial`, `coordinator.go:522`). They agree today.
Nothing tests that they agree. That is [F5](#f5).

### 2.6 The wire volume

Measured from the capture and the shipped poll cadences
(`internal/config/defaults.go:43-47`; the five `now-*` secondary groups
round-robin at `RefreshNow`, `poll.go:77-101`):

| Group | Distinct state topics | Publishes/hour |
| --- | ---: | ---: |
| `now-base` | 18 | 6 480 |
| `config` | 10 | 1 200 |
| `now-grid` | 13 | 936 |
| `now-backup` | 12 | 864 |
| `now-inverter` | 7 | 504 |
| `now-pv` | 7 | 504 |
| `now-battery` | 6 | 432 |
| `day` | 9 | 108 |
| `total` | 9 | 108 |
| **Total** | **91** | **≈ 11 136** |

Every one of those is **QoS 0 and `retain=false`** (`poll.go:152`). Plus the
`static` group's 3 registers, which carry no HA hints and are published to
topics no entity reads.

---

## 3. What adoption would move on the wire

ADR 0070 sanctions a clean break for the bridges. The pilot's decisive
finding was that the break could be *declined* for zendure, which is what let
the runtime be proven on an installed base without confounding. The same
question, asked of this bridge:

> **Every identity string this bridge publishes can be preserved
> byte-exactly, and — unlike the pilot — so can its QoS, because v0.27.0
> added the sentinel the pilot's measurement asked for. The things that
> cannot stay are the discovery *topic* and the absence of `origin`. Two
> further changes are unavoidable and are *fixes*: state becomes retained,
> and the availability plane has to be decided rather than omitted.**

### 3.1 Before / after, per string

"After (library default)" is what `go-hamqtt` v0.31.0 produces with
`discovery.StdContext` taken as it comes. "After (preserving)" is what it
produces with the override named in the last column.

| String | Before (measured) | After (library default) | After (preserving) | How |
| --- | --- | --- | --- | --- |
| Discovery topic | `homeassistant/sensor/MTEC_grid_power/config` ×100 | `homeassistant/device/<node_id>/config` ×1 | **cannot be preserved** | device-bundle only, `discovery/bundle.go:7-11` |
| `node_id` | **does not exist** | `StdContext.NodeID(dev)`, `discovery/context.go:55` | n/a — a new string | required non-empty, `discovery/validate.go:114-115` |
| `unique_id` | `MTEC_grid_power` | slug-derived from device + entity | identical to before | override `Context.UniqueID`, `discovery/render.go:28` |
| device `identifiers` | `["MT1234567890"]` | namespaced/slugged | identical to before | `model.Identifier{Namespace: "", Value: serialNo}` — the documented escape hatch, `model/device.go:40-58` |
| `via_device` | absent | absent (no `Device.Via` set) | absent | — |
| `default_entity_id` | `sensor.grid_power` | `StdContext.ObjectID`-derived | identical to before | override `Context.ObjectID` returning `slugify(englishName)`; the library prefixes `platform + "."` itself, `discovery/render.go:340-347` |
| `object_id` | absent | absent | absent | no such field, `discovery/bundle.go:82-86` |
| `state_topic` | `MTEC/<sn>/<group>/<key>/state` | `topic.Default`'s `<root>/<scope…>/<addr>/…` | identical to before | own `topic.Layout` (4 methods, `topic/topic.go:29-50`) |
| `command_topic` | `…/set` | as above | identical | same `Layout` |
| `name` | localised label | `Component.Name` | identical | `discovery/bundle.go:80` |
| `device.name`/`model`/`model_id`/`manufacturer`/`serial_number`/`sw_version` | flat strings | typed | identical | `discovery/bundle.go:44-55`, `model/device.go:176-184` |
| `device_class`, `state_class`, `unit_of_measurement` | flat keys | typed | identical | `discovery/bundle.go:88-90` |
| `value_template` (77 entities) | flat key | typed | identical | `discovery/bundle.go:97` |
| `enabled_by_default` (100 entities) | flat bool | `*bool` | identical | `discovery/bundle.go:93` |
| `options` (5 entities) | flat list | typed | identical | `discovery/bundle.go:100` |
| `mode: "box"` (5 numbers) | flat key | **not on `Component`** — `discovery.NumberFields.Mode` | identical | `discovery/gen_fields.go:375-376` |
| `payload_on`/`payload_off` (8) | flat keys | **not on `Component`** — `SwitchFields` / `BinarySensorFields` | identical | `discovery/gen_fields.go:33-34`, `:410-412` |
| `origin` | absent | `{"name": …}` | **cannot be omitted** | `Origin.Name` required, `discovery/validate.go:119-123` |
| availability | **absent on all 100** | an `availability` **list** naming bridge + device levels | omit with `model.NoAvailability()` | `model/description.go:65,139`; the default is `{LevelBridge, LevelDevice}`, `model/description.go:115` |
| Every publish's QoS | **0** (`poll.go:152`, `coordinator.go:570`, `reconcile.go:84`, `main.go:283`) | 1 | **QoS 0 *is* preservable** | `publisher.QoSAtMostOnce`, `publisher/qos.go:53` |
| Command subscribe QoS | **1** (`coordinator.go:318`) | 1 | identical | `CommandConfig.QoS`, `publisher/command.go:563` |
| State retain | **false** (`poll.go:152`) | **true — not expressible as false** | **cannot be preserved** | `StatePublisher.Publish` is unconditionally retained, `publisher/state.go:313-318`; `Pulse` (`state.go:501`) is the only non-retained path and is for event payloads |
| State payload | bare scalar (`process.go:240-254`) | `StatePublisher.Publish` takes raw bytes | identical | no envelope needed |

### 3.2 The four unavoidable changes

1. **100 per-entity configs become 1 device bundle.** Registry-safe: ADR
   0070's second amendment measured it on HA 2026.9 with 958 MQTT entities —
   the entity keeps its `unique_id`, its custom name, its icon, its renamed
   `entity_id` and its `device_id`. The order is mandatory and fails silently
   in one direction: retract the per-entity config first, publish the bundle
   second. `Runtime.PublishBundle` implements exactly that
   (`publisher/publisher.go:433-446`: the dedup check runs *before* the
   retraction, and a failed retraction aborts the publish at `:443-445`,
   unlike `Retract`, which is best-effort, `publisher.go:531-558`).

   The resulting document is roughly **33 KB** — the 100 payloads sum to
   50 585 compact bytes, of which 99 copies of the 179-byte device block
   (17 721 bytes) collapse into one. That is comfortably under `go-mqtt`'s
   1 MiB default `MaximumPacketSize`, but it is the largest bundle in the
   programme by a wide margin and it is worth stating so nobody discovers the
   limit later.

2. **The `origin` block appears.** New key, no identity impact, and one of
   the backlog items the ADR counted.

3. **Every state publish becomes retained.** This is a *fix*, and it is the
   first half of the rollout table's "fixes retain… as a side effect" —
   confirmed mechanically, not by intent: `StatePublisher` has no
   non-retained mode for a state value. After a Home Assistant restart every
   entity currently sits at `unknown` until the next poll (up to 3 600 s for
   the `static` group, 300 s for `day`/`total`); after the migration the
   broker serves the value. It is still a wire-behaviour change of an
   installed base, and it is coupled to [F8](#f8): today `formatValue(nil)`
   returns `""` (`process.go:249-250`), and an empty *retained* payload is
   MQTT's retraction. The library refuses it by construction
   (`ErrEmptyStatePayload`, `publisher/state.go:79-88`). **Flipping the
   retain flag by hand without that guard would be the one way to turn this
   fix into a defect.**

4. **The availability plane stops being absent and has to be decided.** This
   is the change with the largest blast radius and it is entirely a *choice*,
   which is why it must not ride inside another step. See §3.4.

**And one thing the pilot recorded as impossible that is now possible.** The
pilot measured v0.26.0 and wrote "*Every publish and subscribe moves from QoS
0 to QoS 1. There is no opt-out.*" v0.27.0 added `publisher.QoS` with
`QoSAtMostOnce = 0x80` (`publisher/qos.go:53`) explicitly because of that
measurement — the type's doc comment cites the pilot document by date
(`publisher/qos.go:9-18`). So mtec can state QoS 0 and keep it. The
resolution funnel is `resolveQoS` (`publisher/qos.go:108`), used at
`publisher/publisher.go:276`, `command.go:563`, `state.go:288-289`,
`availability.go:155`. **Do not carry the pilot's QoS row forward
unchanged.**

### 3.3 What blocks the rest of the "after" column, and what settles it

Four things above are derived from reading the library, not from running it.

- **Whether Home Assistant accepts a bundle carrying two components with the
  same `unique_id` on two different platforms.** Nine of mtec's 100 do
  (§2.3). The library does not block it: `discovery/render.go:186-188`
  refuses duplicate *component keys* (`model.Entity.Key()`), not duplicate
  `unique_id`s, and `discovery/validate.go` has no `unique_id` uniqueness
  check at all (`grep -n UniqueID discovery/validate.go` → no hits). In the
  per-entity form the two are distinct registry rows because the topic
  carries the domain; inside a bundle the domain is `Component.Platform` and
  the behaviour is unmeasured. **This is the single highest-risk unknown in
  this migration, and it does not exist in the pilot.**
  *Settled by:* one throwaway device against a live HA 2026.9, publishing a
  two-component bundle whose components share a `unique_id` across `sensor`
  and `number`, watching the log. If HA refuses, the fallback is to re-key
  the sensor view (e.g. `MTEC_charge_limit_sensor`) — which is an orphaning
  change for 9 entities and would have to be declared as such, or to drop the
  redundant sensor view entirely (see [F7](#f7)).

  > **Corrected — this question is settled and no live Home Assistant is
  > outstanding.** `go-hamqtt` v0.32.0 narrowed `discovery.Validate`'s
  > duplicate check to `(platform, unique_id)`; the nine are legal in a
  > bundle and neither fallback was needed. The paragraph above is the
  > frozen snapshot of what was unknown on 2026-09-13. See
  > [Phase 6 outcome](#phase-6-outcome--what-the-measurement-got-and-what-has-overtaken-it).

- **The rendered bundle JSON.** Every key and its source is named above, but
  not the exact byte sequence, because no consumer exists yet.
  *Settled by:* writing the `Layout` + one `model.Device` + 100
  `*model.Basic` entities and dumping `discovery.Render(...)`, then
  `cmd/hacheck` over the output. This is the first migration step in §7.

- **Whether `Validate` accepts `mode`, `payload_on`/`payload_off` through
  `Component.Fields` for these four platforms.** The typed `Fields` structs
  exist (`discovery/gen_fields.go:33,375,410`) and the unknown-key check runs
  against the per-platform catalog schema, so this is near-certain — but
  "near-certain" is not a measurement.
  *Settled by:* `cmd/hacheck` on the rendered payload.

- **Whether Home Assistant's conflict refusal fires for the four-segment
  legacy form.** The amendment's measurement used the five-segment form. This
  is settled *in the library's favour*: the v0.27.0 CHANGELOG entry
  (CHANGELOG.md:405-422) records the conflict being measured live on HA
  2026.9 on 2026-09-10/11 against exactly the four-segment
  `<prefix>/<platform>/<unique_id>/config` shape, which is mtec's shape.
  Nothing further is needed.

### 3.4 The availability decision, and why it is a trap

Today **no entity references any availability topic** (0 of 100 payloads).
The daemon writes a retained `online`/`offline` to `homeassistant/status/lwt`
(`main.go:130`, `:146-150`, `:281-289`) and nothing reads it. That is
[F1](#f1) and [F2](#f2) together.

`go-hamqtt`'s default is *not* "no availability". `model.Availability.Resolved()`
defaults to `{LevelBridge, LevelDevice}` with mode `all`
(`model/description.go:107-119`), and `StdContext.Availability` renders one
entry per level (`discovery/context.go:209-240`). If mtec adopts the library
and says nothing, every one of its 100 components acquires an `availability`
list naming two topics — one of which (`LevelDevice`,
`Layout.Availability(deviceSlot)`) **nothing in this bridge publishes**.
Under `availability_mode: all`, the default, an availability source that
never publishes is not neutral: **the entity stays unavailable, forever,
with nothing in the log to say why.** The library's own type doc measures
exactly this outcome in a sibling project (`publisher/availability.go:87-99`).

So there are three defensible choices and one accident:

| Choice | Effect | Cost |
| --- | --- | --- |
| `model.NoAvailability()` (`model/description.go:139`) | byte-identical to today | keeps [F1](#f1) |
| `model.BridgeOnly()` (`model/description.go:145`) + move the status topic out of HA's birth tree | entities grey out when the daemon dies | fixes [F1](#f1)+[F2](#f2); one new key on 100 configs; the status-topic move is a wire change |
| `{LevelBridge, LevelDevice}` + publish `AvailabilityPublisher.Device` on Modbus reachability (`publisher/availability.go:311`) | entities grey out when the *inverter* is unreachable | the correct end state; needs a new signal from the Modbus watchdog (`coordinator.go:616-633`) |
| say nothing and take the default | **100 entities permanently unavailable** | the accident |

This is where mtec differs most from the pilot. Zendure already had a working
bridge-level availability plane and the migration was neutral about it. Here
the plane does not exist, the library's default assumes it does, and the
failure mode is silent and total.

---

## 4. Is there anything to pin it against?

**No golden files, no captured payload fixture, no `testdata/` for the
discovery plane.** Verified: the only `testdata/` in the repository is
`internal/modbus/testdata` (23 `.bin` frame goldens plus a
`cross_check.py`), and `grep -rn golden --include='*.go' .` returns one hit,
a comment in `internal/registers/decode_test.go:98` referring to those
Modbus frames.

**But the guard is far stronger than the pilot's**, and this is the single
biggest structural difference between the two bridges:

| Test | File | What it actually pins |
| --- | --- | --- |
| `TestSensorEntryShape` | `discovery_test.go:176-197` | for `MTEC_grid_power`: 8 keys verbatim (`name`, `state_topic`, `unique_id`, `unit_of_measurement`, `device_class`, `value_template`, `state_class`, `enabled_by_default`), plus "a `device` key exists" |
| `TestBinarySensorOmitsUnit` | `:199` | the binary-sensor shape |
| `TestNumberEntryEmitsCommandTopic` | `:218-238` | `MTEC/SN12345/config/grid_inject_limit/set` verbatim |
| `TestSelectEntryOptionsAreSortedByCode` | `:239` | option ordering |
| `TestSwitchAlsoPublishesBinarySensor` | `:255` | the dual emission |
| `TestDiscoveryEntryOrderFollowsYAML` | `:283-305` | two config topics verbatim, and catalog order |
| **`TestDiscoveryAgainstRealCatalog`** | `:306-330` | **the real `registers.yaml`**: every entry round-trips as JSON and carries a `unique_id`; ≥ 50 entries |
| `TestEntriesCarryStableEntityID` / `…StayEnglishUnderTranslation` | `:362`, `:382-398` | `sensor.grid_power` under `en` *and* `de` |
| `TestGermanNamesAndOptions` | `:399` | localisation |
| `TestVirtualSwitchEntities` | `:425-456` | the two synthetic switches' command and state topics verbatim |
| `TestConfigFilter` | `:457-463` | `homeassistant/+/+/config` |
| `TestIsOwnConfig` / `…SerialScoped` | `:464`, `:691` | ownership over hand-written payload literals |
| **`TestSlugify`** | `:535-553` | **seven slug cases including `"ÜÄÖ" → ""` and `"  PV-Dach #2  " → "pv_dach_2"`** |
| `TestDeviceNameUnsetKeepsGenericIdentity` | `:555-571` | `default_entity_id`, `unique_id`, `device.name` for the no-name case |
| `TestDeviceNameSetRewritesEntityIdentity` | `:578-610` | the same four plus `device.serial_number` and `state_topic` verbatim, and `IsOwnConfig` round-trip |
| `TestEnumSensorCarriesOptions` / `TestNonEnumSensorHasNoOptions` | `:631`, `:657` | enum `options` presence |
| `TestSerialScopedUniqueIDsOptIn` / `…DefaultsOff` | `:667`, `:682` | `MTEC_SN12345_grid_power` and its config topic verbatim |
| `TestRunWithHASSPublishesDiscoveryRetained` | `coordinator_test.go:504-518` | discovery publishes are retained |
| `TestHASSBirthTriggersDiscoveryRepublish` | `coordinator_test.go:642-688` | birth → republish |
| `TestPublishDiscoveryReturnsPublishedSet` / `TestOrphanTopics` | `reconcile_test.go:15`, `:54` | the published set and orphan selection |
| `TestAnnounceAvailabilityPublishesRetained` | `main_test.go:194-211` | the LWT topic and retain flag |

Twenty-five tests in `internal/hass` alone, 734 lines, **and they run against
the real catalog with the real default topic roots** (`"homeassistant"`,
`"MTEC"` — `discovery_test.go:311`). The pilot's fourth finding — that
zendure's tests used a root (`"zendure"`) no deployment uses, so the one
pinned `unique_id` literal did not have the shape of any string in the field
— **does not apply here.** `MTEC_grid_power` is exactly what ships.

### 4.1 What is nevertheless pinned by nothing

Counted against the 100-entity payload of §2.3:

- **`device.identifiers`** — the device-registry key, with no migration path.
  Referenced nowhere in any test. `device.manufacturer`, `device.model`,
  `device.model_id`, `device.sw_version`: likewise. Only `device.name`
  (`discovery_test.go:568`, `:594`) and `device.serial_number`
  (`:598`) are asserted, and `serial_number` only in one test.
- **Every key on 99 of the 100 entities.** `TestSensorEntryShape` pins eight
  keys on `MTEC_grid_power`; nothing else pins a full payload. 77
  `value_template`s, 76 `device_class`es, 87 units, five `mode: "box"`
  keys, eight `payload_on`/`payload_off` pairs — one of each class is
  covered, at most.
- **QoS.** Every test stub discards it: `coordinator_test.go:157` and `:175`,
  `main_test.go:25`, `:37`, `:184` all take `_ mqtt.QoS`. **The delivery
  guarantee of this bridge is pinned by nothing at all.**
- **`retain=false` on state publishes.** The stubs record `retain`
  (`coordinator_test.go:150,171`) but only assert it for `homeassistant/`
  topics (`:510`, `:656`). No test asserts that a state publish is *not*
  retained — which is fortunate, because that behaviour is [F3](#f3) and
  should change.
- **The nine duplicate `unique_id`s.** Nothing names them; nothing would
  notice if the dual emission changed.
- **The state-topic agreement between `discovery.go:545` and `poll.go:150`**
  ([F5](#f5)).

### 4.2 What pinning it would take

loom's twelve planes were byte-pinned before they moved, and of the eight
measured slug divergences **zero** appeared in any fixture. Here the
equivalent count is better but not zero: `TestSlugify` covers the hyphen and
the umlaut *as a unit test of `slugify`*, but no test connects either to a
catalog entry, and §5.3 shows eight real entities turn on the hyphen.

Concretely, and it is small because the builder is deterministic and pure:

1. **One golden file for the whole device.**
   `internal/hass/testdata/discovery_en.json` and `discovery_de.json` — the
   full 100-entity map, topic → payload, sorted, `json.MarshalIndent`. It is
   produced exactly the way §2.4 was produced (that capture is ~45 lines of
   throwaway test code) and it pins **every** key at once, including
   `device.identifiers` and the 99 entities no test names. Both languages,
   because `LANGUAGE` rewrites `name`, `options` and the state payloads.
2. **A state-topic golden.** The 91 state topics plus the 11 command topics,
   as sorted lists, asserted against *both* builders — `discovery.stateTopic`
   and the `poll.go:150` expression — so [F5](#f5) becomes impossible.
   This is the half of the wire Home Assistant's registry does *not* protect:
   a changed `state_topic` leaves the entity in place and makes it
   permanently `unknown`.
3. **The identity list on its own.** `unique_id` × 100 and
   `default_entity_id` × 100, sorted, in a separate file. Separating it from
   the payload golden means a payload change and an identity change produce
   two different diffs, and only one of them is ever allowed to be non-empty
   without a major release.
4. **Rows for the inputs that diverge, chosen from the diff.** For the
   `slugify` → `topic.Slug` question (§5.3): the four hyphenated register
   names are already in the catalog and are covered free by (1); what is
   missing is a `DEVICE_NAME` fixture containing `ü` (today `_`, library
   `ue`), one containing `é` (today dropped, library `e`), and one whose
   slug is empty (today `""` → falls back to the generic identity,
   library `"x"` → **would produce `sensor.x_grid_power`**). That last one is
   a real behaviour difference, not a cosmetic one, and nothing covers it.
5. **A `cmd/mtec-util` sub-command that dumps the same thing.** The util is
   546 lines and already loads the catalog; a `discovery-dump` would let an
   operator produce a before/after diff on their own hardware with their own
   `DEVICE_NAME` and `LANGUAGE`, which is what the migration note needs users
   to be able to do.

Items 1-4 are pure test additions with no production change, and they must
land **before** the first line of the migration.

---

## 5. What the library still does not cover for this consumer

`go-hamqtt` v0.31.0 is 12 142 non-test lines across 11 packages, with
15 347 test lines. `publisher` alone is 5 341 of them. It is not short of
vocabulary. What follows is what this bridge specifically has to bring or
work around.

### 5.1 What maps cleanly

Most of it. The device block maps completely — `ModelID`, `SerialNumber`,
`SWVersion`, `Manufacturer`, `Model` (`discovery/bundle.go:44-55`,
`model/device.go:176-184`). Every flat key in §2.3's union has a typed home
(§3.1). `select` options are typed with a reverse lookup that matches the
code *and* every language's label (`model.Enum.Code`), which is
`registers.Register.CodeForLabel` (`internal/registers/register.go:122`)
generalised — a convergence, not a gap. `number` bounds are typed and gated
to the platforms that accept them. `enabled_by_default` is a `*bool`, so
"false" and "unset" stay distinct (this bridge needs that: 9 entities are
explicitly `false` and 91 explicitly `true`).

### 5.2 `topic.Layout` — the interface is right, and `Bucket` does not fit

Four methods (`topic/topic.go:29-50`): `State(model.Slot)`,
`Command(model.Slot)`, `Availability(model.Slot)`, `Bridge()`. This bridge
has no topic module to convert — it has two `fmt.Sprintf`s
(`discovery.go:545-555`) and one inline expression (`poll.go:150`), which
become one `Layout` of ~40 lines and thereby fix [F5](#f5) as a side effect.

The friction is one level down, exactly as in the pilot. `model.Slot`
carries `Scope []string`, `Address`, `Channel`, `Bucket`, `Path []string`
(`model/slot.go:75-95`), and `Bucket` is a closed enum of five paramset
names — `BucketUnset/Values/Master/Calculated/Custom` (`model/slot.go:23-37`).
This bridge's topic level is `config | now-base | now-grid | now-backup |
now-battery | now-inverter | now-pv | day | total | static`: ten values that
mean something else entirely. They do not map and they do not need to —
`BucketUnset` renders to nothing and the group rides in `Slot.Path` — but
`Bucket` is dead weight in every slot this bridge constructs, and this is
now the *second* consumer to record that. Worth telling the library.

One hyphen caveat: three of the ten groups contain `-`. `topic.Safe`
(`topic/topic.go:123-140`) replaces only `/ + # NUL` and whitespace, so a
`Layout` that routes the group through `Safe` preserves `now-base`. A
`Layout` that routes it through `Slug` also preserves it (hyphen is in the
accept class, `topic/topic.go:174`). Either is safe here; it is worth
asserting in the step-1 golden anyway.

**The measured trap — `Layout.State` is not the config's `state_topic` on 10
of 32 platforms — does not bite this bridge today, and it is one register
away from biting.** The ten are enumerated at `publisher/state.go:94-96` and
`:343-346`: climate, water_heater, lawn_mower, camera, tag, **button**,
device_automation, image, notify, scene. This bridge publishes `sensor`,
`number`, `select`, `switch`, `binary_sensor` — five of the 22 that do have
a `state_topic`. **But `button` is a declared platform of this bridge**
(`discovery.go:45`) with an empty dispatch case (`discovery.go:369-371`),
i.e. [F6](#f6). The day someone fills that case, `layout.State(slot)` and
`comp.StateTopic` diverge and the failure is a state topic no entity reads.
The rule therefore applies from day one: publish through
`StatePublisher.PublishComponentValue` / `ComponentStateTopic`
(`publisher/state.go:409`), never through `Layout.State`.

### 5.3 Slug and naming

| | this bridge (`discovery.go:178-194`) | `go-hamqtt` (`topic/topic.go:146-197`) |
| --- | --- | --- |
| `ü`, `ö`, `ä`, `ß` | `_` (dropped, folded) | `ue`, `oe`, `ae`, `ss` |
| `é`, `ñ`, `ø`, `ç` | `_` | `e`, `n`, `oe`, `c` |
| `-` | folded to `_` | **preserved** |
| empty result | `""` | `"x"` |
| adjacent duplicate tokens | not collapsed | not collapsed |

Four divergence classes. Two of them are already exercised by
`TestSlugify` (`discovery_test.go:535-553`) — `"ÜÄÖ" → ""` and
`"PV-Dach #2" → "pv_dach_2"` — so **swapping `slugify` for `topic.Slug`
breaks a test loudly rather than shipping green.** That is a materially
better position than the pilot's, where the current suite exercised none of
the six.

And unlike the pilot, **the hyphen divergence reaches the real catalog.**
Measured by re-slugging all 100 captured entity names under both functions:

| Entity | today | `topic.Slug` |
| --- | --- | --- |
| `number` / `sensor` — *Off-grid SOC limit* | `off_grid_soc_limit` | `off-grid_soc_limit` |
| `number` / `sensor` — *On-grid SOC limit* | `on_grid_soc_limit` | `on-grid_soc_limit` |
| `switch` / `binary_sensor` — *Off-grid SOC limit switch* | `off_grid_soc_limit_switch` | `off-grid_soc_limit_switch` |
| `switch` / `binary_sensor` — *On-grid SOC limit switch* | `on_grid_soc_limit_switch` | `on-grid_soc_limit_switch` |

**8 of 100 `default_entity_id`s change.** (Home Assistant would slugify the
hyphen back to `_` when it derives the actual `entity_id`, so the *rendered*
entity id is probably unchanged — but the *published string* differs, a
golden would catch it, and "probably" is exactly the word this programme
keeps having to delete.)

The `""` → `"x"` divergence is the one with teeth. Today a `DEVICE_NAME` of
`"ÜÄÖ"` slugs to `""`, and `entityIDBase` (`discovery.go:206-212`) treats
that as "no device name" and falls back to the bare seed — `sensor.grid_power`.
Under `topic.Slug` it becomes `"x"` and every entity gets
`sensor.x_grid_power`. Inert for installed entities, decisive for new ones,
and covered by no test.

The library's `Slug` is the better function — its doc comment names
*"Größe → gr_e"* as the defect a consuming bridge shipped
(`topic/topic.go:142-145`), and that bridge is this one. But "better" is not
"same", and swapping it is a step with byte risk, taken alone, last, with
fixtures ahead of it.

### 5.4 `CommandRouter` — the overlap rule is free here

`CommandRouter.Handle` (`publisher/command.go:635`) refuses any filter pair
that *structurally* overlaps: `filtersOverlap` (`publisher/command.go:1563-1578`)
walks the two filters' segments and returns true on any `#`, on a `+` against
anything, or on equal literals to the end.

**How many filters does mtec register, and do any overlap? Three subscribe
call sites; none overlaps.** Counted exhaustively —
`grep -n "Subscribe(" internal/coordinator/*.go` returns exactly three
non-test hits:

| Filter | Site | Segments | Fate under the library |
| --- | --- | ---: | --- |
| `homeassistant/status` | `coordinator.go:306` (via `hassStatusTopic`, `:201`) | 2 | `publisher.WatchBirth` (`publisher/birth.go:131`) |
| `MTEC/+/+/+/set` | `coordinator.go:307` | 5 | one `CommandRouter.Handle` route |
| `homeassistant/+/+/config` | `reconcile.go:54` (transient) | 4 | `Runtime.Sweep` (`publisher/sweep.go:186`) |

Pairwise: 2 vs 5, 2 vs 4, 4 vs 5 — different lengths, no `#`, so
`filtersOverlap` returns false in every pair. **The rule costs this bridge
nothing**, exactly as in the pilot, and the three land on three different
library facilities anyway.

`CheckDisjoint` (`publisher/command.go:1004`) is the guard this bridge has no
equivalent of: it checks every topic the consumer *publishes* against the
router's own filters and fails the boot on a self-echo. Checked by hand here:
`MTEC/<sn>/<group>/<key>/state` is five segments ending `state` against a
five-segment filter ending `set` — disjoint; `homeassistant/status/lwt` is
three segments — disjoint; the 100 config topics are four — disjoint. Clean
today, unguarded today.

One real cost, and this bridge already pays it the right way:
`CommandHandler` runs on a router worker, not the read loop
(`publisher/command.go:1151-1155`, pool at `publisher/command_pool.go:26`
with per-topic FIFO). This bridge does the same by hand — `onMessage`
(`coordinator.go:372`) hands off to a bounded write queue via `enqueueWrite`
(`coordinator.go:428-449`) rather than writing Modbus inline — so the
behaviour is identical and the hand-rolled queue's *drop-oldest* policy
(`coordinator.go:416-427`, with its own two tests) is something the library's
unbounded FIFO does **not** have. See §6.5.

### 5.5 `Envelope` has no timestamp, and this bridge does not want one

`publisher.Envelope` is `{Value any; Available bool}`
(`publisher/state.go:44-55`), and the omission is argued
(`publisher/state.go:32-43`): a timestamp would make every payload unique
and turn the dedup gate — `bytes.Equal` over the full payload,
`publisher/state.go:363` — into a no-op.

This bridge publishes a bare scalar (`formatValue`,
`internal/coordinator/process.go:240-254`). No timestamp, no envelope, so it
keeps the dedup gate — and here the gate is a much larger win than it was for
the pilot. Today `publishGroupOnce` re-publishes every value of a group on
every cycle (`poll.go:149-157`): **≈ 11 136 publishes per hour** (§2.6),
nearly all byte-identical to the previous one. Under `StatePublisher` that
collapses to the changes only. `config`, `day`, `total` and most of `static`
are near-constant: entities like `charge_limit` or `total consumption` would
publish once per process, or once per change.

Note the interaction with §3.2 item 3: the dedup gate makes retained state
*cheaper* than non-retained state is today, not more expensive.

### 5.6 What the consumer must still write itself

| Must supply | Where | Size |
| --- | --- | ---: |
| `topic.Layout` (4 methods) | `topic/topic.go:29-50` | ~40 lines, from three `Sprintf`s |
| `publisher.Transport` | satisfied by `gomqtt.Transport(client)` | 1 line |
| `model.Entity` per register | embed `*model.Basic` | an adapter over `registers.Register` |
| The `Owns` sweep predicate | `publisher/sweep.go:46`, `ErrSweepUnscoped` if nil | this is `IsOwnConfig` (`discovery.go:321-338`), ~18 lines |
| `NumberFields{Mode: "box"}`, `SwitchFields`, `BinarySensorFields` | `discovery/gen_fields.go` | ~12 lines |
| `Context.UniqueID` / `ObjectID` overrides | if identity is preserved (§3.1) | ~15 lines |
| An explicit availability decision | §3.4 | 1 line, and a release note |
| `Config.LegacyEntityTopics = {LegacyTopicByUniqueID}` | `publisher/publisher.go:150` | 1 line, and §7's highest-risk step |
| `Bundle.RemoveComponents` (never `Bundle.Remove`) | `discovery/bundle.go:339` | see §7.3 |

Nothing on that list is hard, and nothing on it is missing from the library.

---

## 6. What this bridge does better, or differently

### 6.1 Verdict on the two suspected defects: **both confirmed**

The brief asked whether this bridge has (a) a daemon LWT referenced by no
entity, and (b) an availability topic inside Home Assistant's own birth tree.
**It has both, and they are the same string.**

```go
// cmd/mtec2mqtt/main.go:126-130
// Retained availability topic: the broker-side will covers
// ungraceful death, the OnConnect hook below publishes the matching
// "online" birth, and the shutdown path re-publishes "offline"
// because a graceful DISCONNECT suppresses the will.
lwtTopic := cfg.HASSBaseTopic + "/status/lwt"
```

with `cfg.HASSBaseTopic` defaulting to `"homeassistant"`
(`internal/config/defaults.go:30`), wired as the will at `main.go:146-150`,
announced `online` on every connect at `main.go:160` and `offline` on
graceful stop at `main.go:185`, both retained, both QoS 0
(`main.go:283`).

- **(b) Inside HA's birth tree: confirmed.** `homeassistant/status/lwt` sits
  one level under `homeassistant/status`, the topic Home Assistant itself
  publishes its birth message to — and which *this same daemon subscribes
  to* (`coordinator.go:201`, `:306`). The library's own measurement of this
  exact shape is at `cmd/hacheck/runtime.go:44-58`, describing *"a bridge
  publishes its own status at `<discovery_prefix>/status/lwt` — inside Home
  Assistant's own birth tree"*, and it names it as one of ADR 0070's five.
  This is that bridge.
- **(a) Referenced by no entity: confirmed by exhaustive count.** Across the
  100 captured payloads, the keys `availability`, `availability_topic`,
  `payload_available` and `payload_not_available` each appear **0 times**.
  `grep -n "availability" internal/hass/discovery.go` returns no hits. So the
  will fires on a hard crash, writes `offline` to a topic in Home Assistant's
  own tree, and every one of the 100 entities stays *available* forever,
  showing the last value it ever saw. The library states the consequence at
  `publisher/publisher.go:11-13` and CHANGELOG.md:947-952.

Both are inert today *and mutually reinforcing*: because no entity reads the
topic, the fact that the topic is in the wrong place has never surfaced.
Fixing (b) alone changes nothing observable; fixing (a) alone would point 100
entities at a topic inside HA's birth tree. **They have to be fixed
together**, which is why §7 gives them one step.

### 6.2 The third documented defect: **also confirmed**

ADR 0070 §8.2:109-111 records *"mtec publishes state non-retained. After a
Home Assistant restart every entity sits at `unknown` until the next poll."*

```go
// internal/coordinator/poll.go:152
if err := c.deps.MQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, false); err != nil {
```

Confirmed: `retain=false`, on the only state-publish call site in the
repository (`grep -n "MQTT.Publish" internal/coordinator/` returns three
hits: `poll.go:152` state, `coordinator.go:570` discovery retained,
`reconcile.go:84` retraction retained). Worst case is the `static` group at
`RefreshStatic = 3600` seconds (`internal/config/defaults.go:46`): an hour of
`unknown` after a Home Assistant restart.

> **Corrected — fixed in step 2a.** State is retained now, at QoS 0 stated
> rather than defaulted (`coordinator.StateQoS`), with F8's empty-payload
> guard in the same change. Pinned by `TestPublishQoSAndRetain`. See
> [Phase 6 outcome](#phase-6-outcome--what-the-measurement-got-and-what-has-overtaken-it).

So **the rollout table's note — "fixes retain/availability/slugify bugs as a
side effect" — is confirmed on all three counts.** One correction to the
brief's attribution, though: that note is **not** in `go-hamqtt`'s CHANGELOG.
An exhaustive search of that repository for `mtec` finds only test fixtures
(`cmd/hadoctor/main_test.go:15,17`, `cmd/hacheck/runtime_test.go:17`). The
claim lives in openccu-loom, at
`notes/concepts/shared-ha-discovery-model.md:868` and, in expanded form, at
`:95-120`. The library records the *defect shapes* without naming the bridge;
loom names the bridge. Both are right about the same thing.

### 6.3 But "side effect" understates it — two of the three are not free

- **Retain (§3.2 item 3)** is genuinely a side effect: `StatePublisher` has
  no non-retained mode, so adopting it *is* the fix. But it is coupled to
  [F8](#f8), and the coupling runs the wrong way if the flag is flipped by
  hand outside the library.
- **Availability (§3.4)** is the opposite of a side effect. The library's
  *default* would make the situation catastrophically worse, not better. It
  is a decision that has to be taken explicitly, in its own step.
- **Slugify** is a side effect only if `topic.Slug` is adopted, which §7 says
  not to do — and 8 of 100 entities' `default_entity_id` turn on it (§5.3).

### 6.4 What this bridge does better

**The `unique_id` namespace is a compile-time constant, not the configurable
MQTT root.** `uniqueIDPrefix = "MTEC_"` (`discovery.go:31`), used at
`discovery.go:246-251` and `:329`. `go-hamqtt` states the rule directly —
*"the namespace must be a constant of the bridge, never configurable"* — and
cites the defect it comes from. **That defect is zendure's F2**, not this
one. Here an operator who changes `MQTT_TOPIC` keeps every `unique_id`, keeps
every config topic, keeps every entity's history, and only moves the state
topics — which the very next discovery publish re-points, because the config
topic is keyed on the unchanged `unique_id`. The sweep's second half is still
namespaced on the root (`discovery.go:332-337`), so a stale config could
survive; but it is the *same topic* that gets overwritten, so nothing is
orphaned. **This is the correct design and the pilot does not have it.**

**A Home Assistant birth subscription exists.** `coordinator.go:306` +
`onMessage`'s birth branch (`:375-386`) + `discoveryRepublisher`
(`:597-611`) + a configurable `HASS_BIRTH_GRACETIME`
(`waitForHASSBirth`, `:454-461`, default 15 s,
`internal/config/defaults.go:31`). That is zendure's F6 — which the pilot
records as *absent* there — implemented here, with a generation counter
(`discoveryGen`, `coordinator.go:382`, `:564`, `:577`) that closes the race
where a birth arriving mid-publish is swallowed by the final `Store(true)`.
It is pinned by two tests (`coordinator_test.go:642`, `:791`). The library's
`WatchBirth` (`publisher/birth.go:131`) is the same idea with a
single-slot dispatcher; **this bridge's generation guard is a refinement the
library's `Republish` path should be checked against.**

**A bounded, drop-oldest command queue.** `enqueueWrite`
(`coordinator.go:428-449`) drops the *oldest* pending command when the queue
is full, with the rationale that dropping the newest strands an inverter on
an intermediate value when a Home Assistant slider is dragged, and it replies
to the dropped caller (`replyTo`, `:174-182`) so the web UI gets an error
rather than a hang. Two tests pin it (`coordinator_test.go:818`, `:850`).
`commandPool`'s queue is an unbounded slice plus condvar
(`publisher/command_pool.go:32-48`), deliberately, to avoid deadlocking the
read loop — a different and also correct trade-off, but it has no
back-pressure policy and no caller feedback. **Worth telling the library.**

**A retained-command guard.** `onMessage` drops any `/set` delivered with the
retain flag (`coordinator.go:395-404`), because a broker replaying a past
command on resubscribe would re-override a setting the user has since
changed. Pinned by `TestRunIgnoresRetainedSetCommand`
(`coordinator_test.go:552`). `publisher.CommandRouter` has a retain-flag
guard of its own; this bridge arrived at it independently and logs the drop.

**A genuinely strong test suite.** 8 015 test lines against 6 722 non-test,
running against the real catalog with the real default topic roots (§4). The
pilot had 976 against 3 763 and used a topic root no deployment uses.

### 6.5 Three things the library should learn from this bridge

1. **A back-pressure policy for the command pool.** Drop-oldest plus a reply
   to the dropped caller (`coordinator.go:416-449`) is a better answer than
   an unbounded queue for any consumer whose device writes are slow and whose
   commands are *positions* rather than *events* — which is every dimmer,
   every setpoint and every slider. The library's unbounded FIFO cannot be
   configured into that shape.
2. **A birth generation counter.** `discoveryGen` (`coordinator.go:382`,
   `:564`, `:577`) closes a race the library's single-slot birth dispatcher
   may or may not close: a birth arriving *during* a republish must not be
   swallowed by that republish's completion. Worth checking
   `publisher/birth.go:145-159` against this case explicitly.
3. **Two topic roots, not one.** This bridge separates the *discovery* root
   (`HASS_BASE_TOPIC`) from the *data* root (`MQTT_TOPIC`), and namespaces
   `unique_id` with neither. `publisher.Config` has `Prefix` for the first
   and leaves the second to the `Layout`, which is the same separation — but
   nothing in the library documents that the identity namespace must be a
   third, constant thing. `discovery/naming.go` says it; `Config` does not
   carry it. A `Config.Namespace` that is documented as "never derived from
   `Prefix` or from the layout root" would make the rule structural instead
   of advisory.

### 6.6 What it does *differently*, without being better or worse

- **Two entities per writable register.** A `number` and a read-only `sensor`
  view of the same value; a `switch` and a `binary_sensor`
  (`discovery.go:360-368`). No sibling does this. It is the source of the
  nine duplicate `unique_id`s (§2.3) and therefore of the migration's one
  genuine unknown (§3.3). It is also, on its own terms, useful: the sensor
  view is `enabled_by_default: true` while the control is `false`, so a user
  sees the value without being handed the control.
- **`default_entity_id` seeded from the English *name*.** Deliberate, to stay
  a drop-in replacement for `aiomtec2mqtt`, and documented at length at
  `discovery.go:196-205`. Alone among the six.
- **No sub-devices.** One flat HA device. Which means this bridge exercises
  *none* of the `Via` hierarchy the pilot was chosen to exercise, and
  conversely exercises the "one very large bundle" case nothing else does.

---

## 7. Verdict and sequencing

### 7.1 Is mtec the right phase 6?

**Yes, and for the reason the pilot gave for going second, now discharged.**

The pilot document argued mtec should *not* go first because *"it cannot
distinguish 'the library changed the payload' from 'the library fixed the
payload', and it has the same absent pins."* Half of that is right and half
is not:

- The confounding is real and is now *worse* than the pilot assumed, because
  the availability change is not a side effect at all (§6.3) and can make
  things silently worse. The pilot's judgment holds: this bridge must not be
  the one that proves the machinery.
- **The "same absent pins" half is wrong.** This bridge has 25 discovery
  tests, a real-catalog test, and a `TestSlugify` that already fails on both
  divergence classes that matter (§4, §5.3). Its gap is different: it pins
  one payload of 100 and no QoS at all. That is a *golden-file* gap, not a
  test-culture gap, and it is one PR.

What makes mtec the right phase 6 rather than phase 7 is that the pilot has
already bought two of its three hardest problems:

- `publisher.QoSAtMostOnce` (v0.27.0) exists **because** of the pilot's QoS
  finding, and mtec is QoS 0 throughout. Without it the migration would
  silently change the delivery guarantee of an installed base.
- `LegacyTopicByUniqueID` (v0.27.0) exists **because** of the pilot's
  four-segment finding, and **mtec's fleet is the same four-segment form**
  (`<prefix>/<platform>/<unique_id>/config`, `discovery.go:539-541`). Without
  it `PublishBundle` would retract nothing and Home Assistant would refuse
  100 entities with one `WARNING` line.

Both traps were measured on zendure and are pre-paid here. That is the pilot
working exactly as intended.

### 7.2 Recommended sequencing

Ordering principle, borrowed from loom's inventory and the pilot: every step
that cannot change a published byte goes first, so the steps that can arrive
alone, on a clean tree, each with its own release note.

**Step 0 — bump `go-mqtt` v1.3.0 → v1.5.1 and delete `mqttSession`.** No byte
risk. `mqtt.SplitClient(breaker, mqttClient)` replaces `main.go:392-407`;
`main_test.go:51` and `:77` become tests of library code and can go.
Independent of everything else, and `go-hamqtt` v0.31.0 requires v1.5.1
anyway. *Pins: compilation, plus the existing suite.*

**Step 1 — pin the current payload.** No production change. The four
artefacts of §4.2: the 100-entity payload golden in `en` and `de`, the
91+11 topic golden asserted against *both* builders, the identity list, and
the three `DEVICE_NAME` divergence rows. Loaded from the real
`registers.yaml`, not a test catalog. This is the step that makes every later
one measurable. *Nothing may be skipped here for being obvious — the pilot's
evidence is that the pins caught four defects code review did not.*

**Step 2 — fix the three payload defects, each alone, each against the step-1
goldens.** These are *not* migration steps; they are the debt that must not
be inside one, and this is where mtec's order departs most from the pilot's
(which had no such debt):

- **2a — retain.** `poll.go:152` `false` → `true`, **together with**
  [F8](#f8): refuse to publish an empty payload rather than retracting.
  One-line diff in the golden (there is no state payload in the golden; add a
  state-retain assertion to `coordinator_test.go`). Wire-visible, payload-
  invisible, registry-neutral.
- **2b — availability.** Move the status topic out of `HASS_BASE_TOPIC` — to
  `MTEC/bridge/status`, matching three of the five siblings — *and* add
  `availability_topic` + the two payload keys to all 100 configs, in the same
  commit, because either alone is wrong (§6.1). This is a payload change
  visible in the step-1 golden as exactly three new keys on 100 entities. It
  needs its own release note: a user's automations that referenced
  `homeassistant/status/lwt` (unlikely, but it is a documented topic) break.
- **2c — the redundant `button` platform.** Either implement it or delete the
  case and the constant ([F6](#f6)). Zero payload change; it exists only to
  stop §5.2's trap from becoming live during the migration.

  *Deliberately NOT in step 2: the slugify fix.* See §7.4.

**Step 3 — write the model adapter and render one bundle, publishing
nothing.** A `topic.Layout` over the group/key shape; one `model.Device` with
`model.Identifier{Namespace: "", Value: serialNo}`; a `*model.Basic` per
`registers.Register` plus two for the synthetic switches;
`Context.UniqueID`/`ObjectID` overrides returning today's strings;
`NumberFields`/`SwitchFields`/`BinarySensorFields`; an explicit
`model.Availability` matching whatever 2b decided. Then `discovery.Render` →
`discovery.Validate` → `cmd/hacheck`, and diff the component bodies against
the step-1 goldens key by key. **This is where §3.3's duplicate-`unique_id`
question is answered in code** — and where, if the library refuses it, the
whole shape of step 6 changes. *Byte risk: none — nothing publishes.*

**Step 3b — settle the duplicate `unique_id` against a live Home Assistant.**
*(Cancelled — a version bump settled it instead; see
[Phase 6 outcome](#phase-6-outcome--what-the-measurement-got-and-what-has-overtaken-it).)*
Before anything else moves. One throwaway device, one two-component bundle
sharing a `unique_id` across `sensor` and `number`, HA 2026.9, watch the log.
This is a half-day and it gates step 6. If HA refuses, decide then between
re-keying the nine sensor views (an orphaning change, own major release) and
dropping them; do not decide it inside the migration.

**Step 4 — move the state plane onto `StatePublisher`, per-entity configs
unchanged.** The dedup gate is the win and it is registry-neutral: same
topics, same payloads, ~11 136/hour becomes the changes only. Use
`PublishComponentValue` / `ComponentStateTopic`, never `Layout.State`
(§5.2). State `QoS: publisher.QoSAtMostOnce` explicitly, so the installed
base's delivery guarantee is unchanged and *stated*. *Byte risk: the topics
are pinned by step 1's topic golden; the message-rate drop is the one
behaviour change a user could be surprised by.*

**Step 5 — move birth/LWT, the command router and the sweep onto `Runtime`,
still per-entity.** `Will()` at CONNECT, `AnnounceOnline` on reconnect,
`WatchBirth` replacing `discoveryRepublisher` (check the generation-counter
race of §6.4 survives), one `CommandRouter.Handle("MTEC/+/+/+/set")` with
`CheckDisjoint` over the 91 state topics and the bridge topic, and `Sweep`
with `Owns` = today's `IsOwnConfig`. Keep the drop-oldest write queue
(§6.5) — the router worker feeds it, it does not replace it. *Byte risk: the
sweep can retract a live config if `Owns` is wrong, and step 1's goldens do
not cover the sweep; this step wants a mock-broker test of its own.
`Retract(res.Owned...)` is the composition that cleared 29 live configs in a
sibling repo (`publisher/sweep.go:113-123`) — retract `res.Retracted`, never
`res.Owned`.*

**Step 6 — the bundle migration. Alone, last, with the migration note.** Set
`Config.LegacyEntityTopics = []publisher.LegacyTopicFunc{publisher.LegacyTopicByUniqueID}`
(`publisher/publisher.go:150`), use `Bundle.RemoveComponents` and never
`Bundle.Remove` (`discovery/bundle.go:339` vs `:298` — a tombstone written by
`Remove` carries a platform and nothing else, so the four-segment form has no
`unique_id` to key on), then `PublishBundle`. Verify against a live HA 2026.9
that no conflict warning appears and that a renamed, re-iconed entity
survives. *Byte risk: maximal, and the failure mode is silent. This step is
the reason the five above are separate.*

### 7.3 Where this order departs from the pilot's, and why

| | Pilot (zendure) | Here (mtec) | Why |
| --- | --- | --- | --- |
| Step 0 | one minor (`v1.3.0 → v1.4.0`) | **two minors (`v1.3.0 → v1.5.1`)** | `go-hamqtt` v0.31.0 requires v1.5.1 |
| Defect fixes | *"Each is its own commit, before or after, never inside a step"* — and none was on the critical path | **a whole numbered step, before the adapter** | three of mtec's defects are *on* the planes being moved; 2b in particular must precede any `model.Availability` decision |
| Availability | neutral — a working bridge plane already existed | **its own step, with a release note, and the library's default is the wrong answer** | §3.4; the failure mode is 100 permanently-unavailable entities |
| QoS | *"cannot be preserved"* | **preserved explicitly with `QoSAtMostOnce`** | v0.27.0, bought by the pilot |
| `SupersededTopics` | *"the single highest-risk step… `SupersededTopics` will not do it"* | **one config line does it** | v0.27.0's `LegacyTopicByUniqueID`, bought by the pilot |
| A live-HA gate | one (the conflict refusal) | **two (the conflict refusal, already settled; the duplicate `unique_id`, not settled)** — *corrected: one, and it too was settled without a live session; phase 6 completed with zero* | mtec's dual emission has no analogue |
| Sub-devices | the point of the pilot | none | mtec exercises "one 33 KB bundle" instead |
| Pins | *"no golden, tests using a root no deployment uses"* | **25 tests on the real root and the real catalog; one payload of 100 pinned** | the gap is a golden file, not a test culture |

The one structural inversion: **the pilot fixed nothing and moved
everything; mtec must fix three things before it moves anything.** Trying to
take them as side effects would leave the migration unable to say which of
"the payload changed" and "the payload was fixed" is which — precisely the
confounding the pilot document predicted, and the reason it put mtec second.

### 7.4 What I would NOT do

- **Re-key `unique_id` or `default_entity_id`.** The break is sanctioned and
  it is still not worth taking. Freeze both with the §3.1 overrides. If they
  are ever taken, they are taken as their own major release, alone, after
  fixtures cover all four slug divergence classes.
- **Swap `slugify` for `topic.Slug`.** Same reason, one level smaller, and
  *more* pressing here than in the pilot because the divergence reaches the
  real catalog: 8 of 100 `default_entity_id`s change (§5.3), and the
  `""` → `"x"` empty-slug case changes all 100 for any operator whose
  `DEVICE_NAME` has no ASCII alphanumerics. Inert for installed entities is
  exactly why it would ship green and split new installs from old ones.
  If it is ever taken, it takes `TestSlugify` with it — which is a feature.
- **Adopt `topic.Default`.** The `Bucket` enum does not fit (§5.2) and the
  topic tree is in `README.md`, in `addon/DOCS.md`, and in every user's
  automations.
- **Drop the dual `sensor`/`number` emission to make the bundle simpler.**
  It is 9 entities of installed base and a deliberate UX choice (§6.6). If
  step 3b says a bundle cannot carry duplicate `unique_id`s, re-key the
  sensor views as a declared orphaning change — do not silently delete them.
  *(Corrected: step 3b was cancelled and this branch was never taken — the
  nine are legal in a bundle.)*
- **Enable `HASS_UNIQUE_ID_INCLUDE_SERIAL` as part of the migration.** It
  changes every `unique_id` and is correctly documented as a deliberate
  opt-in that orphans an existing installation (`discovery.go:138-146`).
  Touching it inside a step that also moves the topic would make the two
  indistinguishable.
- **Add `entity_category`, `icon`, `suggested_display_precision` or any other
  newly-available key during the migration.** They are all additive and all
  tempting because the typed `Component` makes them one field each. Every one
  of them is a payload diff against the step-1 golden that is not the
  migration.
- **Touch the web UI's state plane** (`internal/web`, `internal/state`). It
  is a separate cache fed from `publishGroupOnce` (`poll.go:146-148`) and is
  not a Home Assistant surface.

### 7.5 What the migration note must say

1. **Discovery moves from 100 retained per-entity configs to one device
   bundle.** Entities keep their `unique_id`, their `entity_id`, their
   customisations, their history and their device assignment — measured on HA
   2026.9, not assumed. Home Assistant ≥ 2024.11 is required from this
   release on.
2. **Nothing is re-keyed.** `unique_id`, `default_entity_id`, `state_topic`,
   `command_topic` and device `identifiers` are byte-identical to the
   previous release. State it explicitly.
3. **State is now retained.** After a Home Assistant restart entities show
   their last value immediately instead of sitting at `unknown` for up to an
   hour. This is a fix; say so.
4. **State is now de-duplicated.** A value that does not change is no longer
   re-published every 10-30 seconds. Anything downstream that counted
   messages rather than reading the retained value — a second MQTT consumer,
   a Node-RED flow triggered on message rather than on change — will see far
   fewer messages. This and (3) together are the two behaviour changes a user
   could reasonably be surprised by.
5. **QoS is unchanged at 0**, deliberately and now explicitly. (Say it,
   because the library's default is 1 and a reader who knows that will
   assume it moved.)
6. **The bridge availability topic moved** from `homeassistant/status/lwt` to
   `MTEC/bridge/status`, and every entity now references it. An automation
   that watched the old topic must be updated. This ships in its own release
   (step 2b), ahead of the bundle.
7. **The first start retracts the old configs before publishing the bundle.**
   If it is interrupted between the two, entities are briefly *absent* rather
   than unavailable; restarting the bridge restores them.
8. **Rolling back needs the same care in reverse.** Turning bundle mode off
   is not "stop publishing bundles" — the retained device document has to be
   retracted first, or every per-entity config of the next boot is refused
   with nothing but a log line. Ship the rollback instruction, not just the
   upgrade one.
9. **`HASS_UNIQUE_ID_INCLUDE_SERIAL` must not be toggled in the same
   upgrade.** Not new, but this release is the moment to say it.

---

## Findings

Defects and latent defects found while reading. **None were fixed; no Go file
in any repository was modified.** Ordered by consequence.

<a id="f1"></a>
**F1 — no entity has any availability source, so a dead daemon leaves 100
entities showing stale values forever.** Measured: the keys `availability`,
`availability_topic`, `payload_available`, `payload_not_available` appear in
**0 of the 100** captured payloads; `grep -n availability
internal/hass/discovery.go` returns no hits. The daemon does maintain a
retained availability marker (`cmd/mtec2mqtt/main.go:130`, `:146-150`,
`:160`, `:185`) — nothing reads it. On a hard crash (SIGKILL, OOM, power
cut) the will fires, and every entity stays *available*, showing the last
value it ever saw, until the daemon returns. `go-hamqtt` records this class
at `publisher/publisher.go:11-13` and CHANGELOG.md:947-952, and reports it
as `no-availability` in `cmd/hadoctor`. **This is one of the two defects ADR
0070 attributes to this bridge, and it is confirmed.**

<a id="f2"></a>
**F2 — the bridge availability topic is inside Home Assistant's own birth
tree.** `lwtTopic := cfg.HASSBaseTopic + "/status/lwt"`
(`cmd/mtec2mqtt/main.go:130`), i.e. `homeassistant/status/lwt` by default
(`internal/config/defaults.go:30`) — one level under the topic Home Assistant
publishes its own birth message to, and which this same daemon subscribes to
(`internal/coordinator/coordinator.go:201`, `:306`). It is in the wrong
place *and* inert (F1), and the two hide each other: because nothing reads
the topic, nothing has ever surfaced that it is in the wrong tree.
`go-hamqtt` describes this exact shape at `cmd/hacheck/runtime.go:44-58`.
**The second of ADR 0070's two, confirmed.** Fixing either alone is wrong;
see §7.2 step 2b.

<a id="f3"></a>
**F3 — state is published non-retained, so a Home Assistant restart blanks
every entity for up to an hour.** `poll.go:152`, `mqtt.QoS0, false`. The
`static` group polls every 3 600 s (`internal/config/defaults.go:46`),
`day`/`total` every 300 s. Discovery *is* retained (`coordinator.go:570`),
so the entities exist; they just have no value. Already recorded in ADR 0070
§8.2:109-111; confirmed here. Fixed by construction under `StatePublisher`
(`publisher/state.go:313-318`) — but see F8.

> **Corrected — fixed in step 2a**, together with F8. See the disposition
> table in
> [Phase 6 outcome](#phase-6-outcome--what-the-measurement-got-and-what-has-overtaken-it).

<a id="f4"></a>
**F4 — `slugify` performs no transliteration, so a German `DEVICE_NAME`
degrades to underscores.** `internal/hass/discovery.go:178-194` keeps only
`[a-z0-9]` after `ToLower`. `Größe` → `gr_e`; `ÜÄÖ` → `""`, which
`entityIDBase` (`:206-212`) then treats as "no device name at all", silently
discarding the operator's configuration from every `default_entity_id`.
`go-hamqtt`'s `topic.Slug` names this exact defect in its doc comment
(`topic/topic.go:142-145`) and in `topic/topic_test.go:13-15`. Recorded in
ADR 0070:50 and §8.2:118; confirmed. Note this bridge's test suite *pins*
the current behaviour (`discovery_test.go:535-553`), so any fix is loud —
which is why §7.4 says not to take it during the migration.

<a id="f5"></a>
**F5 — the state topic is built in two places and nothing checks they
agree.** `internal/hass/discovery.go:545-548` composes
`d.mqttTopic + "/" + d.serialNo + "/" + r.Group + "/" + r.MQTT + "/state"`;
`internal/coordinator/poll.go:150` composes
`topicBase + "/" + group + "/" + key + "/state"` where `topicBase` is
`cfg.MQTTTopic + "/" + serial` (`coordinator.go:522`). They agree today and
are pinned separately — `discovery_test.go:185` pins the config's copy, and
nothing pins the publisher's copy against it. A divergence would leave 100
entities pointing at topics nobody publishes, with nothing in the log. The
library's README records two reference bridges that ended up inconsistent
exactly this way; `ComponentStateTopic` (`publisher/state.go:409`) exists to
make it structurally impossible.

<a id="f6"></a>
**F6 — `button` is a declared platform that silently publishes nothing.**
`PlatformButton` is declared at `internal/hass/discovery.go:45` and
dispatched to an empty `case` at `:369-371` with the comment *"registers
declaring it are intentionally not published as entities."* Zero of the 94
grouped catalog entries declare it (`grep -c 'hass_component_type: button'
registers.yaml` → 0), so this is a trap for the next person editing
`registers.yaml`, not a live defect: the register would be polled, its state
published, and no entity would ever appear, with no diagnostic. It is also
the case that would make §5.2's `Layout.State` warning bite, since `button`
is one of the ten platforms with no `state_topic` (`publisher/state.go:94-96`).

<a id="f7"></a>
**F7 — the two synthetic switches are `enabled_by_default: true` while every
real control is `false`.** `discovery.go:391` against `:466`, `:486`,
`:502`. The policy for catalog-derived controls is "visible but disabled, so
a user opts in before writing to the inverter"; the synthetic charge/discharge
switches, which write to the *same* registers as the disabled `charge_limit`
and `discharge_limit` numbers, are enabled. Almost certainly unintended, and
it is a one-key payload change — so it must not ride inside a migration step.

<a id="f8"></a>
**F8 — `formatValue(nil)` returns `""`, which becomes a retraction the moment
state is retained.** `internal/coordinator/process.go:249-250`. Today
`retain=false` makes an empty payload inert. Under F3's fix an empty retained
payload is MQTT's retraction: the entity's value would be *deleted* from the
broker rather than set to unknown. Latent rather than live —
`registers.Decode` never returns `(nil, nil)` (`internal/registers/decode.go`
returns an error on every failure path), and `processOne`
(`process.go:46`) preserves its input — so no current path produces a nil.
But the coupling is structural, and it is the reason F3 must be fixed
*with* a non-empty guard rather than by flipping a boolean. `go-hamqtt`
refuses it by construction (`ErrEmptyStatePayload`, `publisher/state.go:79-88`,
recorded as a measured defect of the reference implementation at
CHANGELOG.md:519-533).

<a id="f9"></a>
**F9 — `publishDiscovery`'s doc comment describes behaviour the function does
not have.** `coordinator.go:543-546`: *"…and subscribes to every writable
entity's command topic so HA can drive the inverter back. Existing
command-topic subscriptions are idempotent on the adapter."* The function
(`:559-589`) contains no `Subscribe` call; the command path is a single
wildcard subscription installed once at `:307`. `Entry.CommandTopic`
(`internal/hass/discovery.go:55`) is populated for 11 entries and read by
nothing outside tests (`grep -rn CommandTopic internal/ cmd/ --include='*.go'`
→ 4 hits in `discovery.go`, 3 in `discovery_test.go`, 0 in
`internal/coordinator/` and 0 in `cmd/`). Harmless, and it is the kind of
stale comment that makes a reader trust a subscription that is not there.

<a id="f10"></a>
**F10 — the `static` group publishes three state topics that no entity ever
reads.** Measured: `static` has 3 grouped registers and **0** with HA hints,
yet `spawnPolls` polls it (`poll.go:43-45`) and `publishGroupOnce` publishes
every key in the processed map (`poll.go:149-157`), not only the annotated
ones. `now-base` likewise has 20 registers of which 18 are annotated. Five
topics of pure noise per device, at the group's cadence. Not a defect so much
as a missing filter — but it means the topic tree is wider than the discovery
tree, which the step-1 topic golden should record rather than discover.

<a id="f11"></a>
**F11 — the orphan sweep would not recognise a device bundle as its own, in
either direction.** `IsOwnConfig` (`internal/hass/discovery.go:321-338`)
unmarshals top-level `unique_id` and `state_topic`; a device bundle has
neither at the top level, so `json.Unmarshal` succeeds with both fields
empty, the `MTEC_` prefix test fails, and the bundle is classified as
another integration's. That is *safe* today — an old daemon running alongside
a new one will not retract the new one's bundle — and it is a *gap*
tomorrow: nothing in the current code could ever clean up a stale bundle.
Recorded here because it is the one place where the existing sweep and the
migration target silently do not see each other, and §7.2 step 5's `Owns`
predicate has to be written knowing it.

---

## Phase 6 outcome — what the measurement got, and what has overtaken it

Added after step 6 landed (PRs #48–#54). The sections above are left exactly
as they were written; this section is where every correction lives.

### Corrections — read these before trusting a passage above

- **§3.3's duplicate-`unique_id` question is SETTLED, and it did not take a
  live Home Assistant.** The measurement calls it *"the single highest-risk
  unknown in this migration"* (`:533`) and scopes a half-day live-HA session
  to settle it. What actually settled it was a version bump:
  `go-hamqtt` v0.32.0 narrowed `discovery.Validate`'s duplicate check to key
  on `(platform, unique_id)`, mirroring Home Assistant's own
  `(domain, platform, unique_id)` entity-registry index. The nine duplicated
  ids are legal inside a device bundle exactly as they have always been legal
  in the per-entity form. Recorded in
  [`adr0070-phase6-step3-results.md`](./adr0070-phase6-step3-results.md) §3
  and [`adr0070-phase6-steps45-results.md`](./adr0070-phase6-steps45-results.md)
  §1; asserted by `TestRenderedBundleAcceptsTheDuplicatedUniqueIDs`.
- **Step 3b (`:1168`) is cancelled.** It existed only to answer the above.
  Neither of the two fallbacks it names was needed: no re-keying of the nine
  sensor views, no dropping them, no catalogue change at all. §7.4's *"if
  step 3b says a bundle cannot carry duplicate `unique_id`s"* (`:1244`) is
  therefore a branch that was never taken.
- **§7.3's live-HA-gate table row (`:1216`) now reads "one, and it was
  already settled".** It says *"two (the conflict refusal, already settled;
  the duplicate `unique_id`, not settled)"*. One remained at the time of
  writing; zero remain now. Phase 6 completed without any live Home Assistant
  session.
- **§6.2 and F3 describe a defect that is fixed.** The measurement records
  state published non-retained (`poll.go:152`, `mqtt.QoS0, false`). Step 2a
  fixed it: state is retained, at QoS 0 stated rather than defaulted
  (`coordinator.StateQoS = publisher.QoSAtMostOnce`), with F8's empty-payload
  guard in the same change because an empty payload on a retained topic is a
  retraction. Pinned by `TestPublishQoSAndRetain` and
  `TestNilValueIsNotPublished`.
- **F5 undercounted.** The measurement counts two state-topic composers; the
  real count was five. See
  [`adr0070-phase6-steps45-results.md`](./adr0070-phase6-steps45-results.md)
  §2.4 and F14 there.
- **The "measured against" versions are superseded.** `go-hamqtt` v0.31.0 →
  v0.32.0, `go-mqtt` v1.3.0 → v1.5.1, both taken during the migration.

### Disposition

| Finding | Disposition |
| --- | --- |
| **F1** — no availability source | **Fixed** in step 2b. Every payload declares the bridge status topic; pinned by `TestEveryPayloadDeclaresBridgeAvailability`. |
| **F2** — availability topic inside HA's birth tree | **Fixed** in step 2b, with F1 and in the same change, because fixing either alone is wrong. Pinned by `TestAvailabilityTopicIsOutsideTheDiscoveryTree`. |
| **F3** — state published non-retained | **Fixed** in step 2a, with F8. Pinned by `TestPublishQoSAndRetain`. |
| **F4** — `slugify` does not transliterate | **Left, deliberately.** §7.4's reasoning stands: Home Assistant never renames a registered entity, so swapping the function strands ids rather than migrating them. `TestSlugify` still pins the current behaviour, so any future fix is loud. |
| **F5** — the state topic built in more than one place | **Fixed and closed** in step 4; the composers converged on `hass.StateTopic`. Worse than measured (five, not two) — see F14 of the steps-4-and-5 results. |
| **F6** — `button` silently publishes nothing | **Fixed.** An `hass_component_type` this builder cannot emit is now loud rather than silent; `TestUnsupportedComponentTypeIsLoud` is the assertion. |
| **F7** — the synthetic switches' `enabled_by_default` | **Left**, and still pinned as an inconsistency by `TestGoldenPinsTheEnabledByDefaultInconsistency`. It is a one-key payload change that toggles entities on in installed fleets; it does not ride inside a migration step. |
| **F8** — `formatValue(nil)` becomes a retraction once state is retained | **Fixed** in step 2a, in the same change as F3 and for that reason. Pinned by `TestNilValueIsNotPublished`. |
| **F9** — `publishDiscovery`'s doc comment describes a subscription it does not make | **Fixed.** The comment now describes the retained device document the function actually writes. |
| **F10** — the state-topic tree is wider than the discovery tree | **Left**, still pinned as a defect in the coordinator package's topic golden. The filter is a later step. |
| **F11** — the sweep would not recognise a device bundle as its own | **Fixed** in steps 5 and 6. `hass.OwnsConfigTopic` judges a parsed `publisher.ConfigTopic`, including its `Bundle` flag, instead of unmarshalling a payload. |

### What the frozen measurements are still good for

Every count in sections 1-6 — 100 payloads, 91 distinct `unique_id`s, the
~11 136 messages an hour, the slug divergence, the capture method in the
appendix — is a record of what `origin/main` at `d402f71` actually put on a
broker. That is the baseline every later step's byte-equality proof is
measured against, and rewriting it to match today's output would destroy
exactly the thing it was taken for. It stays.

---

## Appendix — how the payload capture was taken

`git archive origin/main` was extracted to a scratch directory outside every
repository. A throwaway `_test.go` in a new package under that copy called
the production code path directly:

```go
m, _, _ := registers.Load("registers.yaml")
vs := hass.DefaultVirtualSwitches(2000, 2000)
d := hass.New("homeassistant", "MTEC", m, "en", vs, "")
d.Initialize("MT1234567890", "1.2.3", "EB-10kW")
for _, e := range d.Entries() { /* topic -> json.Unmarshal(e.Payload) */ }
```

`MT1234567890`, `1.2.3` and `EB-10kW` are plausible stand-ins for the serial,
firmware and equipment string the STATIC register read supplies; every other
byte is real output of `origin/main`. The scratch copy was discarded. No file
in `/home/markus/Dokumente/GitHub/go-mtec2mqtt` was modified by the capture.

Slug divergence (§5.3) was measured by re-slugging the 100 captured `name`
values under a transcription of `internal/hass/discovery.go:178-194` and a
transcription of `topic/topic.go:146-197`, and diffing. Publish volume
(§2.6) was computed from the distinct state topics per group and the shipped
cadences in `internal/config/defaults.go:43-47`, with the five `now-*`
secondary groups at `RefreshNow / 5` each (`poll.go:77-101`).
