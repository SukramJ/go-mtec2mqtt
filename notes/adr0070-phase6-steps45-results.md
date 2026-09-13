# ADR 0070 phase 6, steps 4 and 5 — the runtime moves onto go-hamqtt

- Status: results of a completed migration step, not a decision
- Date: 2026-09-13
- Subject: [`notes/adr0070-phase6-measurement.md`](./adr0070-phase6-measurement.md)
  §7.2 steps 4 and 5, taken together in one pull request
- Measured against: this repository at `0263946` (post-#51),
  `github.com/SukramJ/go-hamqtt` **v0.32.0** (bumped here from v0.31.0),
  `github.com/SukramJ/go-mqtt` v1.5.1,
  `github.com/SukramJ/go-ha-catalog` v0.2.1
- Predecessor: [`notes/adr0070-phase6-step3-results.md`](./adr0070-phase6-step3-results.md)

Step 3 proved the shared library reproduces this bridge's discovery
payloads byte for byte while publishing nothing. This is the step where
that path starts actually publishing — the state plane, the bridge's own
birth/LWT, the command router and the orphan sweep — and where the one
question step 3 left open is answered by a version bump rather than by a
live Home Assistant.

Nothing in the catalogue changed. No golden was regenerated.

---

## 1. The duplicate `unique_id`s are legal, and step 3b is cancelled

`go-hamqtt` v0.32.0 narrowed `discovery.Validate`'s duplicate check to key
on `(platform, unique_id)`, mirroring Home Assistant's own
`(domain, platform, unique_id)` registry index.

**Confirmed rather than assumed.** `TestRenderedBundleAcceptsTheDuplicated\
UniqueIDs` renders both device bundles — `en` and `de` — and asserts
`discovery.Validate` accepts each. It is the step-3 pin, updated: that pin
asserted the refusal precisely so a change here would be a failing test
rather than a surprise, and on the bump it went red with the message it was
written with ("this is new information and step 6's fallback is no longer
needed; update this test"). It now asserts three things:

- 100 components, 91 distinct `unique_id`s, and exactly the nine
  duplications step 3 enumerated — unchanged.
- Every `(platform, unique_id)` pair in the bundle is distinct. This is the
  key v0.32.0 actually uses, and it is worth asserting on its own: a
  catalogue that ever emitted the *same* platform twice under one
  `unique_id` would still be refused, and that refusal would be correct.
- `discovery.Validate` returns no error, in both languages.

Consequences, all of which shrink step 6:

- The former **step 3b live-HA gate is cancelled.** It existed to find out
  whether a bundle may carry the duplication. The library now says yes, and
  its own publish path no longer withholds the document.
- **The catalogue needs no change.** Neither of the two fallbacks the
  measurement named — re-keying the nine sensor views as a declared
  orphaning change, or dropping them — is required.
- §3 of the step-3 results is superseded on this one point. The rest of that
  document (byte equality 200/200, the legacy topic form, the mutation
  table) stands.

---

## 2. The state plane — what moved, and what did not

`publisher.StatePublisher` now writes every register value.

| | before | after |
| --- | --- | --- |
| topic | `poll.go`'s own `fmt.Sprintf` | `hass.StateTopic`, one function |
| payload | `formatValue(v, GoFloatVerb())` | **unchanged** |
| retain | `true` | `true` |
| QoS | `mqtt.QoS0` | `publisher.QoSAtMostOnce` → wire 0 |
| messages/hour | ≈ 11 136 | the changes only |

### 2.1 The QoS, stated

This is the trap the brief named and it is worth writing down twice.
`publisher.QoS`'s zero value is `QoSUnset`, which every runtime type in the
library resolves to **QoS 1**. A `StateConfig` that simply omitted the field
would have tripled this bridge's broker traffic and changed the delivery
guarantee of an installed base, inside a step whose stated purpose is
de-duplication, with a broker capture as the only evidence.
`publisher.QoSAtMostOnce` is `0x80` — deliberately outside the wire's 0–2
range — precisely so "unset" and "deliberately at most once" cannot be
written the same way.

Three constants now say it, one per question, in `internal/coordinator`:

| constant | value | wire | pinned by |
| --- | --- | ---: | --- |
| `StateQoS` | `QoSAtMostOnce` | 0 | `TestPublishQoSAndRetain` (#49's pin), `TestStateQoSIsStatedNotDefaulted` |
| `DiscoveryQoS` | `QoSAtMostOnce` | 0 | `TestPublishQoSAndRetain` |
| `CommandQoS` | `QoSAtLeastOnce` | 1 | `TestSubscribeQoS` (#49's pin) |

**How it was verified, and it is not the constant.** `TestPublishQoSAndRetain`
came from #49 and reads the QoS and the retain flag *off the transport call*
— the stub records what the client was actually handed. It was not touched
in this PR and it still passes, which means the byte that reaches
`mqtt.Publish` is still `QoS0` after the plane moved. The constant is
additionally asserted to resolve to wire 0 via `publisher.QoS.Wire()`, and
mutating it to `QoSUnset` or `QoSAtLeastOnce` turns both tests red.

### 2.2 The payload is still `formatValue`'s

`publisher.StatePublisher.PublishValue` + `discovery.RawEncoding` was the
obvious route and it is the wrong one here: this bridge's float verb is
operator-configurable (`GoFloatVerb`) and `publisher.RenderRawValue` renders
a Go float with `%v`. Routing the rendering through the library would move
bytes the goldens pin. `StatePublisher.Publish` takes the bytes as they are.

### 2.3 Two things that are new rather than preserved

- **A (re)connect reopens the dedup gate.** `PublishOnline` calls
  `StatePlane.Reset()` before announcing. A broker that came back without
  its retained store would otherwise have every entity suppressed by a cache
  that believes the value is already there — which for the `static` and
  `total` groups is forever. The next poll is the snapshot pass the
  library's `Reset` documentation asks a consumer to pair it with.
- **The state plane is checked against this process's own command filter.**
  `StateConfig.CommandFilters` is `hass.CommandFilter(root)` — the same
  string the subscription and the router route read.

### 2.4 F5 is closed, and it was worse than measured

The measurement found the state topic built in **two** places. It is built
in **five**, and the two extra pairs are the least visible kind: the
synthetic charge/discharge switches composed their own state and command
topics inline in `appendVirtualSwitch`, and no register backs them, so
nothing else ever writes there to reveal a mismatch.

All five now go through `hass.StateTopic` / `hass.CommandTopic`, which
render through the go-hamqtt `topic.Layout` step 3 introduced. **The 200
discovery-payload pins and the 96-topic golden held byte-for-byte through
that convergence**, which is the evidence that the `Layout` reproduces the
five concatenations exactly.

The §5.2 trap the measurement warned about — `Layout.State` returns a topic
for all 32 platforms while `state_topic` is projected on only 22 — is now a
test rather than a rule to remember:
`TestLayoutStateTopicEqualsTheConfigsOwnStateTopic` renders all 100
components in both languages and asserts
`publisher.ComponentStateTopic(comp) == hass.StateTopic(...)` for every one.
The day `button` (F6) or any other state-topic-less platform joins this
catalogue, that test goes red instead of the fleet going quiet.

---

## 3. Birth, LWT and the command router

### 3.1 The will is the runtime's statement, not a literal

`publisher.Runtime` is built **before** the MQTT client, over a
`deferredTransport` whose client is wired in before anything connects. The
ordering is forced: the Last Will is part of CONNECT, and the will is
`Runtime.Will()`'s answer. `run()` copies all four fields — topic, payload,
QoS, retain — into `mqtt.TCPConfig.Will` without writing any of them down.

`publisher.Config` takes the same `hass.Layout` the discovery side renders
from and derives the status topic from its `Bridge()`, refusing a
`StatusTopic` that disagrees. So `hass.BridgeStatusTopic` is read by the
will, by `AnnounceOnline`/`AnnounceOffline`, and by the 100 entities'
`availability` list, and the three cannot drift.

**The status topic is `MTEC/bridge/status`, not under the serial, and that is
deliberate**: the marker is published at CONNECT, before the STATIC read
that learns the serial number. `TestAnnouncementsUseTheBridgeTopic` asserts
that it is not serial-scoped, so nobody "fixes" it later.

**One gap, stated because the test cannot cover it**: that `run()` uses
`Will()`'s values rather than a literal of its own is a property of eleven
lines of wiring in a function that dials a broker. The same gap the phase-5
pilot recorded.

### 3.2 `WatchBirth` was deliberately not adopted

The measurement's step 5 offered `publisher.Runtime.WatchBirth` as a
replacement for `discoveryRepublisher`. It is declined, for three reasons
that all point the same way:

- **QoS.** `WatchBirth` subscribes at the runtime's own QoS, which here is
  0. This daemon subscribes `homeassistant/status` at QoS 1 and has since
  its first release, pinned by `TestSubscribeQoS`. Downgrading a birth
  subscription inside a step whose claim is that nothing moved is exactly
  the trade this programme keeps refusing.
- **The generation counter (§6.4 of the measurement).** `publishDiscovery`
  samples `discoveryGen` before its first publish and only marks the batch
  sent when it is unchanged afterwards, so a birth arriving mid-publish is
  not swallowed. `WatchBirth` has no equivalent and no seam to add one.
- **Retry.** `discoveryRepublisher` retries a *failed* batch every 5 s, not
  only on a birth. An open circuit breaker at startup publishes nothing at
  all; `WatchBirth` would then leave Home Assistant without entities until
  the next birth, which — if Home Assistant was already up — never comes.

What did move is the writer underneath: `publishDiscovery` publishes through
`publisher.Runtime`, so the replay feeds the claim set the sweep reads, and
an unchanged fleet costs zero broker writes.

### 3.3 The command router

`publisher.CommandRouter` replaces the hand-rolled `/set` dispatch. Same
filter (`MTEC/+/+/+/set`), same QoS (1), same topic shape. What changes:

- The route's `+` levels arrive already split (`Command.Wildcards`) instead
  of being recovered by `splitPath` and a negative index. Two of the
  library's measured consumer's confirmed defects were exactly that
  arithmetic.
- Handlers run on a router worker rather than the transport's read loop.
  The bounded **drop-oldest** write queue is kept — the router feeds it, it
  does not replace it. The library's own FIFO does not drop the oldest, and
  dropping the oldest is what makes a dragged Home Assistant slider end on
  the value the user released it at.
- Retained commands are dropped by policy (`DeliverRetained: false`) rather
  than by a hand-written check.
- The subscription carries MQTT 5.0's **No Local**, so this process cannot
  echo its own publishes into its own handler.
- `CheckDisjoint` runs after the STATIC read, over all 96 state topics plus
  the bridge status topic, and **fails the boot** on a collision. This
  daemon had no equivalent; the measurement checked it by hand and found it
  clean, but the inspection had to be redone every time a topic moved.

---

## 4. The sweep, and the report it was asked for

### 4.1 Report-only, then retracted here

The pass is `SweepRequest{ReportOnly: true}` and this daemon retracts what
it chose, through `Runtime.Retract`. That is not caution for its own sake:
the library's retracting pass judges a topic on `Owns` alone, which sees the
parsed topic and nothing else, while this daemon's rule has always been the
stronger one — the retained **payload** must carry a `unique_id` in the
`MTEC_` namespace *and* a state topic under this bridge's MQTT root.
Retracting on the topic namespace alone would widen what this daemon is
willing to delete from a tree it shares, inside a step whose whole claim is
that nothing moved.

`Owns` is `hass.OwnsConfigTopic`, as narrow as the topics this bridge
actually publishes — three conditions, each excluding a real population:

1. the four-segment per-entity form only (a device document, a five-segment
   node-id config and the node-id-less three-segment form are all declined);
2. one of the **five** platforms this daemon emits, out of Home Assistant's
   32;
3. an object id — which in this form *is* the `unique_id` — inside the
   compile-time `MTEC_` namespace.

### 4.2 What the report-only pass would have retracted

`TestReportOnlySweepOverTheRealFleet` runs the pass over the real
`registers.yaml` against a broker holding this fleet plus the company it
keeps. Measured:

```
108 retained configs offered, 100 claimed by this daemon,
  1 would be retracted: [homeassistant/sensor/MTEC_retired_sensor/config]
```

The eight non-fleet rows and why each survives:

| retained topic | verdict | declined by |
| --- | --- | --- |
| `sensor/MTEC_retired_sensor/config` | **retract** | — ours, unclaimed |
| `sensor/zigbee2mqtt_0x00124b/config` | keep | namespace |
| `binary_sensor/tasmota_ABC123_status/config` | keep | namespace |
| `device/MTEC_OTHERSERIAL/config` | keep | not the four-segment form |
| `sensor/mtecnode/MTEC_via_node/config` | keep | five-segment node-id form |
| `climate/MTEC_thermostat/config` | keep | platform this daemon never emits |
| `sensor/MTEC_solar_power/config` (state topic under `SOLAR/…`) | keep | payload check — a sibling under another root |
| `sensor/MTEC_grid_power/config.other` | keep | does not parse as a config topic |

One retraction out of 108 offered, and it is the one leftover. The pair
`inspected` / `retracted` is logged together for the same reason it is
reported together here: "0 retracted" and "0 inspected" look identical in a
log line that reports only the second number.

### 4.3 Two instances against two inverters — the predicates DO overlap

Asked, checked, and the answer is yes, completely — and it is not a
regression.

`HASS_UNIQUE_ID_INCLUDE_SERIAL` is off by default, so a `unique_id` carries
no serial and **two instances render the same 100 config topics with the
same `unique_id`s**. Their published sets are therefore identical, which is
why neither judges the other's configs orphaned — and also why the two
overwrite each other's `state_topic` on every boot. That second half is a
pre-existing condition of the default configuration; this step neither
introduces it nor fixes it, and `HASS_UNIQUE_ID_INCLUDE_SERIAL` is the
documented answer to it.

In the configuration that actually separates two instances (serial-scoped
unique ids), the config topics differ **and** `IsOwnConfig` additionally
requires the state topic to sit under this inverter's own serial — so
neither instance claims the other's config. Both halves are asserted by
`TestTwoInstancesOwnTheSameConfigTopics`.

### 4.4 Step 6's ordering is expressible

The brief's constraint: a per-entity config retained for a `unique_id` and a
device bundle carrying the same id cannot coexist, the refusal is symmetric
and silent, and the correct order is always **retract first, publish
second**. Nothing bundle-shaped is published here. What this PR puts in
place so step 6 is a small change rather than a risky one:

- `hass.LegacyConfigTopic` renders the per-entity config topic through
  `publisher.LegacyTopicByUniqueID` — the form step 3 measured at 100/100
  against the pins, recorded as `hass.LegacyConfigTopicForm`. It is now the
  **single** spelling in the repository: the builder publishes to it, the
  sweep retracts through it.
- `hass.LegacyConfigTopicForms()` is wired into
  `publisher.Config.LegacyEntityTopics` **already**. It is inert on the wire
  — that field is read by `PublishBundle` alone — but it puts the form in
  the boot log (`publisher.legacy_forms`) where an operator can see it
  before the migration rather than after it failed silently. Naming a form
  *replaces* the library's five-segment default rather than adding to it,
  which is the intent: this fleet is 100/100 on the four-segment form and
  the default matches 0.
- `Runtime.PublishBundle` performs the retraction itself, in the right
  order, from that stated form. Step 6 is one call plus a changelog.

---

## 5. The one pinned value that moved, and why

**`subscribe_filters` in `internal/coordinator/testdata/topics.json`, one
line, edited by hand.** `-update-topics-golden` was never passed; the other
four lists in that file and both 100-entity discovery goldens are
byte-identical to what #49 and #50 pinned.

```diff
   "subscribe_filters": [
     "MTEC/+/+/+/set",
-    "homeassistant/+/+/config",
+    "homeassistant/#",
     "homeassistant/status"
   ],
```

The hand-rolled orphan reconcile subscribed `homeassistant/+/+/config`;
`publisher.Runtime.Sweep` opens its snapshot window over `homeassistant/#`
so that all three discovery topic forms reach one parser instead of a
wildcard shape that matches only one of them. For the two seconds the window
is open this daemon therefore *receives* every retained message under the
discovery prefix rather than only the four-segment configs.

It acts on none that `hass.OwnsConfigTopic` and `Discovery.IsOwnConfig` do
not both claim, and the pass is report-only, so the widening is in what it
reads and never in what it writes. `Discovery.ConfigFilter` was deleted
rather than left behind naming a filter nothing subscribes to.

---

## 6. Mutation verification

Every new assertion was verified to fail under a mutation of the production
code it covers: the code was perturbed, the suite run, the change reverted.
**Thirty-two mutations, thirty-one caught and one surviving deliberately** —
the 31 rows of the table below plus the survivor named after it.

> **Corrected 2026-09-13 (review of PRs #49–#53).** This line said
> "twenty-seven mutations, twenty-six caught" over a table that already had
> thirty-one caught rows, and the PR #52 body said "28 mutations, 27
> caught" over a 27-row table. The count was UNDERSTATED here and
> mis-stated there; the table was always the record, and it is the table
> the number now comes from. Corrected in both directions, because a
> summary that undersells is as wrong as one that oversells — it is the
> agreement between the prose and the artefact that makes either
> checkable.

| Mutation | Caught by |
| --- | --- |
| `StateQoS` left unset (library would resolve it to QoS 1) | `TestStateQoSIsStatedNotDefaulted` |
| `StateQoS` = QoS 1 | + `TestPublishQoSAndRetain` |
| `DiscoveryQoS` = QoS 1 | `TestStateQoSIsStatedNotDefaulted`, `TestPublishQoSAndRetain` |
| `CommandQoS` = QoS 0 | `TestStateQoSIsStatedNotDefaulted`, `TestSubscribeQoS` |
| retained commands delivered (`DeliverRetained: true`) | `TestRetainedCommandsAreNotDelivered` |
| `onCommand` reads `Wildcards[1]` as the mqtt key | `TestRoutedCommandReachesTheWriteQueue`, `TestRetainedCommandsAreNotDelivered` |
| `onCommand` drops the sibling-serial check | `TestCommandForAnotherInvertersSerialIsDropped` |
| `checkCommandDisjoint` never fails | `TestCheckDisjointRefusesASelfEcho` |
| `PublishOnline` drops `StatePlane.Reset()` | `TestPublishOnlineReopensTheDedupGate` |
| `PublishOnline` announces offline | `TestAnnouncementsUseTheBridgeTopic` |
| discovery published straight to the client (no claim, no dedup) | `TestDiscoveryRepublishIsDeduplicated`, `TestHASSBirthTriggersDiscoveryRepublish` |
| state published straight to the client (no dedup) | `TestStatePublishIsDeduplicated` |
| poll builds the state topic itself again (F5 reintroduced) | `TestTopicGolden`, `TestStateTopicBuildersAgree` |
| poll publishes to the command topic instead of the state topic | 10 tests, incl. `TestPublishQoSAndRetain`, `TestStatePayloadsAreCanonical` |
| poll evicts instead of publishing | 6 tests, incl. `TestNilValueIsNotPublished`, `TestStatePayloadsAreCanonical` |
| sweep retracts on the topic alone (no `IsOwnConfig` body check) | `TestSweepRetractsOnlyOurOwnOrphans` |
| sweep armed rather than report-only | `TestSweepRetractsOnlyOurOwnOrphans`, `TestReportOnlySweepOverTheRealFleet` |
| sweep ignores the runtime's claim set | `TestSweepSparesADeclaredConfigOutsideThisBatch` |
| sweep ignores this boot's published set | `TestSweepSparesAConfigWhoseOwnPublishFailed` |
| `OwnsConfigTopic` drops the platform check | `TestOwnsConfigTopicIsNarrow` + 2 sweep tests |
| `OwnsConfigTopic` drops the node-id / bundle form check | `TestOwnsConfigTopicIsNarrow` + 2 sweep tests |
| `OwnsConfigTopic` drops the `MTEC_` namespace check | `TestOwnsConfigTopicIsNarrow` |
| `LegacyConfigTopicForms` returns the five-segment default | `TestLegacyConfigTopicFormIsTheOneWired`, `TestLegacyFormsNamesTheMeasuredForm` |
| `LegacyConfigTopic` keys on the node-id form | 36 tests, incl. both discovery goldens |
| `Layout.State` renders `/value` instead of `/state` | 15 tests, incl. both discovery goldens |
| `Layout.Bridge` back to the discovery tree | 17 tests |
| `Layout.base` swaps the root and the serial | 19 tests, incl. both discovery goldens |
| `CommandFilter` loses a wildcard level | `TestSubscribeQoS`, `TestTopicGolden`, `TestRunHandlesIncomingSetCommand` + 2 |
| the config topic keeps a hand-built spelling with a changed prefix | 26 tests |
| virtual switches build their own state topic again (F14) | 16 tests, incl. both discovery goldens |
| a sensor and its number view share `(platform, unique_id)` in the bundle | 22 tests, incl. `TestRenderedBundleAcceptsTheDuplicatedUniqueIDs` |

**The one deliberate survivor, recorded rather than papered over.**
Removing the poll loop's empty-payload guard (F8) does *not* turn the suite
red — because `publisher.StatePublisher` refuses an empty retained state
publish itself, with `ErrEmptyStatePayload`. The wire outcome is identical
either way; what differs is the diagnosis, since the local guard names the
offending topic in a warning and an error return does not. Both layers are
kept, and `TestEmptyStatePayloadIsRefusedByBothLayers` pins the new one.

Three assertions were added specifically because a first mutation pass
found them missing, which is the point of running one:

- `TestSweepSparesADeclaredConfigOutsideThisBatch` and
  `TestSweepSparesAConfigWhoseOwnPublishFailed`. The sweep subtracts TWO
  claim sets and the original fixture made them identical, so removing
  either was invisible. They are not the same set: `published` names a
  config whose publish the broker *refused* (an open breaker) and the
  declared set does not; the declared set names a config from outside this
  batch and `published` does not. Each test now makes exactly one of the
  two the only thing standing between the sweep and a live entity.
- `TestEmptyStatePayloadIsRefusedByBothLayers`, above.

---

## Findings

<a id="f14"></a>
### F14 — the state topic was built in FIVE places, not two

F5 of the measurement counted two (`internal/hass/discovery.go` and
`internal/coordinator/poll.go`). `appendVirtualSwitch`
(`internal/hass/discovery.go`) composes a third and a fourth inline for the
two synthetic charge/discharge switches, and `commandTopic` is a fifth for
the writable half. The synthetic pair is the most dangerous of the five:
no Modbus register backs those keys, so nothing else ever writes to their
topics and a drift would be invisible from every direction at once.

Fixed here, not merely recorded: all five call `hass.StateTopic` /
`hass.CommandTopic`, and the 200 payload pins plus the 96-topic golden held
byte-for-byte through the change.

<a id="f15"></a>
### F15 — the library's duplicate-`unique_id` refusal was the library's bug, and this repository's pin is what found it

Recorded as a process observation rather than a defect of this repository.
Step 3's `TestRenderedBundleIsRefusedForDuplicateUniqueIDs` pinned a
behaviour the measurement had got wrong (F13) and that the step-3 author
believed was the library's considered position. It was not: v0.32.0 changed
the check to key on `(platform, unique_id)`.

The pin did exactly what a pin is for — the bump turned it red, with a
message telling the reader what the red meant and what to do about it,
instead of the change landing unnoticed and step 6 being planned around a
constraint that no longer existed. Worth stating because the cost of that
pin was about fifteen lines.

<a id="f16"></a>
### F16 — two default-configured instances are mutually indistinguishable

See §4.3. Two daemons against two inverters, with the shipped defaults,
publish the same 100 config topics with the same `unique_id`s and differing
`state_topic`s, and each accepts the other's payload as its own. The sweep
is safe (identical published sets, so neither orphans the other) but the
two overwrite each other's configs on every boot.

Not introduced here and not fixed here.
`HASS_UNIQUE_ID_INCLUDE_SERIAL` is the documented answer and is correctly
scoped as a deliberate opt-in that orphans an existing installation. Worth
recording because the sweep's ownership predicate had to be written knowing
it, and because the documentation frames the flag as being about *discovery
config collisions* without saying that the collision is total.
