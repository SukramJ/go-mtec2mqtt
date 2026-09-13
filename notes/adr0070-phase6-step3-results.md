# ADR 0070 phase 6, step 3 — what the parallel rendering path measured

- Status: results of a completed experiment, not a decision
- Date: 2026-09-13
- Subject: [`notes/adr0070-phase6-measurement.md`](./adr0070-phase6-measurement.md)
  §7.2 step 3, *"write the model adapter and render one bundle, publishing
  nothing"*
- Measured against: this repository at `c709f45` (post-#50),
  `github.com/SukramJ/go-hamqtt` v0.31.0,
  `github.com/SukramJ/go-ha-catalog` v0.2.1
- Code: `internal/hass/hamqtt.go` (the parallel path),
  `internal/hass/hamqtt_test.go` (the assertions)

The measurement left four things open that could not be established without
running code. All four are now settled. Nothing publishes; no change was
made to the publish path, the coordinator or the MQTT bootstrap.

## 1. Byte-equality: 200 of 200

`go-hamqtt` reproduces every discovery payload this bridge publishes,
byte for byte, in both shipped languages — 100 entities × `en`/`de`.

Two separate assertions, and they ask different questions:

- `TestLibraryReproducesThePinnedPayloads` compares the library's output
  against `internal/hass/testdata/discovery_{en,de}.json`, the pins #49
  established and #50 updated. This is the one that matters: it compares
  against a *file*, not against the builder the parallel path is meant to
  replace.
- `TestLibraryReproducesTheShippedBytesExactly` compares the raw bytes of
  the two paths. Both encode a `map[string]any` with `encoding/json`, so
  key order is sorted on both sides and "byte-for-byte" is literal rather
  than a figure of speech.

Neither pin was regenerated. The new tests have no `-update` flag.

Two further inputs are covered that a golden cannot reach, because a
golden is one fixture: a configured `DEVICE_NAME` (which folds a slug into
all 100 entity-id seeds) and `HASS_UNIQUE_ID_INCLUDE_SERIAL` (which
rewrites all 100 `unique_id`s).

> **Corrected 2026-09-13 (review of PRs #49–#53).** This paragraph, and the
> PR #51 body, said "six configurations in total, each compared byte for
> byte". Two corrections, in both directions:
>
> - **Overstated.** Only **two** of them — `en` and `de` — are compared
>   against a frozen artefact, and by canonical re-encoding rather than raw
>   bytes. The other variants compare the old builder against the new
>   builder in the same process, which answers "do the two paths agree" and
>   not "does either still produce what the fleet has". Both questions are
>   worth asking; only the first is a pin.
> - **Understated, and miscounted.** The variants table holds six cases,
>   but every one of them is `lang: "en"` — `de` is never combined with any
>   variant — so what is actually covered is **seven distinct
>   configurations** (`en` × six variants, plus `de` at the defaults), not
>   six.

### What it took

Everything the measurement predicted, and one thing it did not.

| Concern | How it is preserved |
| --- | --- |
| `unique_id` | `RenderContext.UniqueID`, `"MTEC_" + <mqtt key>` |
| `default_entity_id` | `RenderContext.ObjectID`, this package's `slugify` over the **English** register name; the library prefixes `"<platform>."` itself |
| `device.identifiers` | `model.Identifier{Namespace: "", Value: serial}` — the bare serial |
| topics | an own `topic.Layout`, **not** `topic.Default` |
| availability | `model.BridgeOnly()` over a `Layout` whose `Bridge()` is `MTEC/bridge/status` |
| `mode: "box"` | `discovery.NumberFields` via `discovery.Builder` |
| `payload_on`/`payload_off` | `discovery.SwitchFields` / `discovery.BinarySensorFields` |
| `value_template` | `Description.ValueTemplate` with `discovery.RawEncoding` |
| `enabled_by_default` | `*bool`, so `false` and unset stay distinct |
| `options` | `model.Enum`, codes ordered by numeric code as the shipped builder orders them |
| no `origin` | an empty `discovery.Origin` passed to `RenderComponent` |

**The one thing the measurement did not predict**
([F12](#f12-unit_of_measurement--is-published-on-five-sensors)): five
sensors publish `"unit_of_measurement": ""` — an empty string, not an
absent key. `discovery.Component.UnitOfMeasure` is `omitempty`, so the
typed field cannot express it at all; `Description.Extra` is the only
route. Reproduced, not fixed.

## 2. `discovery.Validate` accepts every payload — and refuses the bundle

This bridge had never validated its own output against Home Assistant's
schemas. It does now, and the answer splits cleanly in two.

**All 200 per-entity bodies pass `discovery.ValidateBody`.** No issues, no
warnings, on either language. That includes the empty `unit_of_measurement`
of F12, which the schema does not object to.

**Neither rendered device bundle passes `discovery.Validate`**, and the
reason is the duplication in §3.

## 3. The duplicate `unique_id`s: the library has an opinion, and it is "no"

Nine `unique_id`s are published twice, under two platforms each (5
`number`+`sensor`, 3 `binary_sensor`+`switch`, 1 `select`+`sensor`). Legal
in the per-entity form, and the 200 assertions above confirm those payloads
are unaffected.

Inside a device bundle:

- **`discovery.Render` is silent.** It refuses duplicate component *keys*
  (`model.Entity.Key()`), and these nine differ — the parallel path keys
  components `"<platform>.<mqtt key>"`. All 100 components land in the
  bundle.
- **`discovery.Validate` refuses it.** Nine issues, one per duplicated
  `unique_id`, `ValidationError.Blocking() == true`, and the result matches
  `discovery.ErrInvalidBundle`. A runtime that validates before publishing
  would therefore withhold the entire device — all 100 entities, not nine.

**The measurement was wrong about this, and the error is worth recording:**
it stated that *"`discovery/validate.go` has no such check
(`grep -n UniqueID discovery/validate.go` → no hits)"*. The check is there,
in `validateBody`'s `seenUnique` map. The grep missed it because the
identifiers are spelled `uniqueID` and `seenUnique`, never `UniqueID`.

**This does not need a live Home Assistant any more.** The measurement's
step 3b was scoped as a half-day against HA 2026.9 to find out whether a
bundle may carry the duplication. The library already answers no, and a
live HA can now only widen that answer, not narrow it: even if Home
Assistant accepted the payload, `go-hamqtt`'s own publish path would not
emit it. Step 6 therefore has to choose between the two fallbacks the
measurement named — re-keying the nine sensor views (an orphaning change,
its own release) or dropping them — and the choice is now on the critical
path rather than contingent.

**It is not resolved here.** The catalogue is unchanged, the duplication is
reproduced deliberately, and `TestRenderedBundleIsRefusedForDuplicateUniqueIDs`
pins the refusal so the day the library or the catalogue changes is a
failing test rather than a surprise.

One incidental number: the rendered bundle is **50 122 bytes**, not the
~33 KB the measurement projected. The difference is #50's availability
plane — 100 copies of a three-key `availability` list and an
`availability_mode`. Still the largest bundle in the programme.

> **Corrected 2026-09-13 (review of PRs #49–#53).** "Far under `go-mqtt`'s
> 1 MiB default `MaximumPacketSize`" compares against the wrong limit:
> that field is what this client will ACCEPT inbound. An outbound PUBLISH
> is bounded by the BROKER's advertised Maximum Packet Size. See the
> correction in `adr0070-phase6-step6-results.md` §1.

## 4. The legacy topic form: `publisher.LegacyTopicByUniqueID`

Verified against the pins rather than assumed.
`TestLegacyTopicFormMatchesThePinnedTopics` walks all 100 pinned config
topics and renders both candidate forms over each:

| Form | Segments | Reproduces |
| --- | ---: | ---: |
| `publisher.LegacyTopicByUniqueID` | 4 | **100 of 100** |
| `publisher.LegacyTopicWithNodeID` (the default) | 5 | **0 of 100** |

Every pinned topic is `homeassistant/<platform>/<unique_id>/config`, four
levels, third level equal to the payload's own `unique_id`, with no
node-id level anywhere.

Nothing uses it yet; the verdict is recorded as
`hass.LegacyConfigTopicForm` with a doc comment, and step 6 is what sets
`publisher.Config.LegacyEntityTopics`. Getting it wrong is silent and
total: the five-segment default would retract none of the 100 retained
configs, the bundle would be published while they were still retained, and
Home Assistant refuses that with one `WARNING [mqtt.entity] Received a
conflicting MQTT discovery message` — the entities simply do not appear.

## 5. Mutation verification

Every new assertion was verified to fail under mutation: the rendering was
perturbed, the suite run, and the change reverted. Nineteen mutations, all
caught.

| Mutation | Caught by |
| --- | --- |
| `Layout.Root` gains a level | 14 tests |
| `Bridge()` back to `homeassistant/status/lwt` | 14 |
| `unique_id` namespace `MTEC_` → `MT_` | 13 |
| entity-id seed from the mqtt key instead of the English name | 14 |
| `device.identifiers` namespaced | 14 |
| `model.BridgeOnly()` → the library default | 13 |
| the empty `unit_of_measurement` dropped | 13 |
| `NumberFields.Mode` `box` → `slider` | 13 |
| `payload_on`/`payload_off` swapped | 13 |
| the synthetic switches' `enabled_by_default` flipped (F7) | 13 |
| enum sensors lose the trailing `"Unknown"` option | 13 |
| select options ordered by label instead of code | 13 |
| an `origin` block stamped on the per-entity form | 14 |
| the dual emission dropped | 18 |
| `EnvelopeEncoding` instead of `RawEncoding` | 13 |
| German labels dropped from names | 4 |
| `RenderContext.DeviceSlug` ignored | 4 |
| `RenderContext.SerialInUniqueID` ignored | 3 |
| the poll group dropped from the topic | 14 |

**Three mutations are caught by exactly one test each, and that is the
point.** `Slot.Bucket`, `Slot.Channel` and `Layout.Availability` are all
*inert* for this bridge — no rendered payload reads any of them — so a
mutation of each changes nothing the 200 byte comparisons can see. The
pilot (PR #41) hit the same wall with `Slot.Bucket` and turned it into an
explicit assertion rather than leaving a blind spot; the same is done here,
in `TestLayoutRendersThisBridgesTopics`, for all three. This is the second
consumer to record that `model.Bucket`'s vocabulary — paramset names —
does not fit a bridge whose topic level is a poll group.

## Findings

<a id="f12"></a>
### F12 — `"unit_of_measurement": ""` is published on five sensors

`appendSensor` (`internal/hass/discovery.go`) sets the key unconditionally
from the catalog's `unit` column, and only the enum branch deletes it when
the unit is empty. Five non-enum sensors therefore publish an empty string
rather than no key. Home Assistant's schema accepts it (measured:
`discovery.ValidateBody` reports nothing), so this is untidiness rather
than a defect — but it is untidiness the shared library cannot express
through its typed field, and reproducing it costs a
`Description.Extra` entry on every unit-less sensor.

Not fixed: it is pinned as current behaviour by
`internal/hass/testdata/discovery_{en,de}.json`, and removing the key is a
payload change that does not belong inside a migration step. It is a
one-line fix (`if r.Unit != ""`) plus a golden regeneration whenever it is
taken.

<a id="f13"></a>
### F13 — the measurement's grep for the bundle's `unique_id` check was wrong

Recorded so the correction is not lost: `go-hamqtt` v0.31.0 *does* refuse a
bundle carrying two components with the same `unique_id`, and the
measurement said it did not. See §3. Nothing in this repository has to
change; the consequence is that step 3b's live-HA session is no longer what
decides step 6's shape.
