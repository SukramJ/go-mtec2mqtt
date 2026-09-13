# ADR 0070 phase 6, step 6 — one retained device document

- Status: results of a completed migration step, not a decision
- Date: 2026-09-13
- Subject: [`notes/adr0070-phase6-measurement.md`](./adr0070-phase6-measurement.md)
  §7.2 step 6 — the last step, and the only one that moves a published byte
  an installed base cannot ignore
- Measured against: this repository at `be59298` (post-#52),
  `github.com/SukramJ/go-hamqtt` **v0.32.0**,
  `github.com/SukramJ/go-mqtt` v1.5.1,
  `github.com/SukramJ/go-ha-catalog` v0.2.1
- Predecessors:
  [`adr0070-phase6-step3-results.md`](./adr0070-phase6-step3-results.md),
  [`adr0070-phase6-steps45-results.md`](./adr0070-phase6-steps45-results.md)

Everything before this step was reversible. This one is not, and the whole
design is arranged around that.

---

## 1. What moved

| | before | after |
| --- | --- | --- |
| discovery topic | `homeassistant/<platform>/MTEC_<key>/config` × 100 | `homeassistant/device/mt1234567890/config` × 1 |
| `device` block | repeated in all 100 payloads | once, at the top of the document |
| `platform` | in the topic, dropped from the body | a key on every component |
| `origin` | absent | one block, on the document |
| everything else | | **byte-identical** |
| retain / QoS | retained, QoS 0 | retained, QoS 0 |
| HA minimum | any | **2024.11** |

`unique_id`, `default_entity_id`, `state_topic`, `command_topic`,
`availability`, `availability_mode`, `device_class`, `state_class`,
`unit_of_measurement`, `options`, `min`/`max`/`step`, `mode`, `payload_on`,
`payload_off` and `enabled_by_default` are all unchanged. Nothing is
re-keyed, so history, renames, icons, areas, hidden flags and automation
references survive.

Checked field by field, per entity, in both languages, against the
**frozen** pre-migration pin (`internal/hass/testdata/discovery_{en,de}.json`)
rather than against anything this build renders a second time —
`TestTheMoveChangesOnlyTheTopicTheDeviceBlockAndTheOrigin`. The two sides
cannot agree on a wrong answer because one of them is a file this code does
not produce: `-update-discovery-golden` was never passed, and the new
artefact has its own flag (`-update-bundle-golden`) so a bundle test cannot
reach the per-entity one.

The new pin is `internal/hass/testdata/bundle_{en,de}.json`: topic and
payload **together**, payload stored decoded, compared on a canonical
re-encoding of both sides, driven by the real builder over the real
`registers.yaml`. 71 KB per language; `go-mqtt`'s default maximum packet
size is 1 MiB.

---

## 2. The ordering, and what "complete" means at QoS 0

Home Assistant refuses a device document while a per-entity config for one
of its components' `unique_id`s is still retained, and refuses the
per-entity config while the document is retained. The refusal is
**symmetric**, produces nothing on the wire, and shows up as exactly one
line — `WARNING [mqtt.entity] Received a conflicting MQTT discovery
message`. The entities simply do not appear. (Measured live on HA 2026.9,
2026-09-10/11; reproduced against no broker in CI.)

So: **retract first, publish second**, and the retraction must be complete
before the document goes out.

`publisher.Runtime.PublishBundle` does both, in that order, and aborts
before the document if a retraction fails. This daemon states the form its
fleet is on (`hass.LegacyConfigTopicForms()` →
`publisher.LegacyTopicByUniqueID`) in `coordinator.HARuntimeConfig`;
stating a form *replaces* the library's five-segment default rather than
adding to it, which is what makes that one line sufficient and not merely
helpful. It is **one** function rather than a literal at the composition
root because the first mutation pass found that a literal there was
guarded by nothing — see §9.

### 2.1 "All 100", derived and not listed

The retraction list is rendered by `publisher.SupersededTopics` **from the
document's own components**. That is what makes it complete by
construction rather than by maintenance:

> the set of `unique_id`s that can conflict with the document is exactly
> the set of `unique_id`s *in* the document, and that is the set the list
> is built from.

A retained per-entity config for a `unique_id` the document does **not**
carry cannot conflict with it — it is a phantom, not a conflict, and it is
the sweep's job (§5). So the coverage question reduces to "is the topic
*form* right", and that is measured: `TestSupersededTopicsAreTheWholeFrozenFleet`
renders the list and compares it to the 100 topics of the frozen pin, in
both languages — **100 of 100, none missing, none invented**.
`TestTheRuledOutLegacyFormsRetractNothing` renders the two ruled-out forms
over the same document and asserts each matches **0** of them, which is the
failure a release that forgot the composition-root line would ship: nothing
retracted, the document published into a tree still holding all 100, and
one `WARNING`.

`internal/coordinator/testdata/topics.json`'s `discovery_config_topics`
list is now derived from `hass.SupersededConfigTopics` instead of from the
pre-migration builder. **All 100 lines are byte-identical** through that
change of derivation — which is itself the evidence that what is retracted
is exactly what used to be published. The file gains **one line by hand**,
`discovery_bundle_topic`; `-update-topics-golden` was never passed.

### 2.2 What "acknowledged" means here — it is ordering, not an ack

`DiscoveryQoS` is `publisher.QoSAtMostOnce` → wire **QoS 0**, unchanged
from every previous release (pinned by `TestPublishQoSAndRetain`, which
reads the byte off the transport call). At QoS 0 there is **no
acknowledgement**, and this code does not pretend there is one.

What it relies on instead:

1. `Runtime.supersede` is sequential and synchronous. Each
   `Transport.Publish` returns only after `go-mqtt`'s `TCPClient.Publish`
   has returned, and at QoS 0 that is `writeFrame` — the whole packet
   encoded into the link buffer under `sendMu`, then one `Write` + `Flush`
   bounded by an `AckTimeout` write deadline. So "complete" means **the
   bytes are on the socket, flushed, in order, ahead of the document's**.
2. One client, one TCP connection: the packets arrive at the broker in the
   order they were written, and a broker processes a client's inbound
   stream in order (MQTT §4.6's ordered-topic rule is the same property
   stated per topic). The broker therefore applies all 100 retractions to
   its retained store before it applies the document, and so before it
   forwards the document to Home Assistant.

The property that actually matters is not "the retractions were
acknowledged" but "**the document cannot reach the broker down a path its
retractions did not already travel**", and ordering gives exactly that. A
connection that dies in between loses both (`Publish` errors, `supersede`
returns, the document is withheld), and the next connect rebuilds from
scratch.

Raising `DiscoveryQoS` to 1 for a PUBACK was considered and rejected: the
library applies one QoS to the whole runtime, so it would change the
delivery guarantee of an installed base's discovery *and* availability
planes inside the one step that already moves a byte — and it would buy
nothing, because the safety property is already held by ordering.

Pinned by `TestRetractionsPrecedeTheBundle` (every superseded topic appears
before the document in the recorded publish sequence, and nothing appears
after it) and `TestAFailedRetractionWithholdsTheBundle`.

### 2.3 Retraction is once per process

`Runtime.supersede` records what it has cleared, so a document rewritten
forty times in a boot does not send forty rounds of retractions. Pinned by
`TestChangedDiscoveryPayloadIsRepublished` (a changed device block rewrites
the document and re-sends **zero** retractions) and
`TestDiscoveryRepublishIsDeduplicated`.

---

## 3. The crash window: self-healing, and how

**The dangerous state**: the retractions are out, the document is not. The
broker then holds no discovery config for this device at all and its
entities are **absent** — not unavailable, gone.

**It heals on the next boot, and the mechanism is that the daemon
remembers nothing.** `Runtime`'s `superseded` and `declared` maps are
per-process and in memory; there is no session store. A fresh process
therefore:

1. re-sends all 100 retractions — a no-op against topics the broker has
   already cleared, and harmless against any that survived;
2. publishes the document, because the dedup gate has no record of it.

No manual step, no broker surgery. Asserted end to end by
`TestTheCrashWindowHealsOnTheNextBoot`, which puts one process in the
window (the retractions land, the document's write is refused), then drives
a second process over a fresh runtime and asserts it both **re-retracts
every topic** and **publishes the document**. Both halves are asserted: a
boot that trusted a previous process's retraction is a boot that publishes
into a conflict.

**Within one process** the same window closes by itself too: an aborted
`PublishBundle` leaves `discoverySent` false, and `discoveryRepublisher`
retries the batch every 5 s.

**The one thing that does not heal is a document that is never valid**, and
that is why the validation happens where it does — see §4.

---

## 4. `buildBundle`: validate first, and withhold everything on a failure

`discovery.Validate` runs **once, at build time, before anything is
published**, and a blocking issue leaves `haBundle` nil — which makes
`publishDiscovery` publish *nothing at all*.

The library explicitly warns that wiring `Validate` into a publish path is
fail-closed: one bad component costs a device all of its entities, and it
documents a real fleet where that would have withheld a sixth of its
devices. That trade is made here **on purpose**, because the alternative is
worse by exactly one irreversible step: publishing an invalid document
means the 100 working per-entity configs have already been retracted and
Home Assistant then discards the replacement in silence. Withholding means
the installed base keeps the configs it already has and goes on working.

The failure is deterministic (the catalogue is compiled in), so
`discoverySent` is left true and `discoveryRepublisher` does not spin on
it; the diagnosis is one `ERROR` naming the node id and the issues.
Warnings — keys Home Assistant accepts and then rewrites — are logged and
the document is published.

**The sweep refuses outright when there is no document** — the guard lives
in `sweepOrphans` itself, so it travels with the code it protects — and
this is not a nicety. After this release the daemon publishes no
four-segment config at all, so every one the sweep's window finds is an
orphan by its own rule — which is right when the document went out and
catastrophic when it did not: it would delete the working entities of the
release being upgraded from and put nothing in their place.

Both bundles validate clean today, `en` and `de`
(`TestTheBundleValidatesWithTheNodeIDItIsPublishedUnder`), including the
nine duplicated `unique_id`s (legal since v0.32.0) and F12's empty
`unit_of_measurement`.

---

## 5. The sweep changed meaning, and one guard went inert

`published` is now one topic, the document's. Consequences, all of them
deliberate:

- **The sweep retracts every four-segment `MTEC_` config it finds** that
  `Discovery.IsOwnConfig` also claims. Post-migration that is correct: they
  are all superseded. In production `PublishBundle` has already cleared the
  100 by the time the window opens, so what is left for the sweep is the
  **residue** — a config whose entity left the catalogue in an *earlier*
  release, which is in no document and which `SupersededTopics` therefore
  cannot name. Only the sweep can reach it
  (`TestSweepClearsTheLegacyConfigsTheBundleReplaces`).
- **The claim subtraction is now inert.** The only topic this daemon claims
  is of the bundle form, which `hass.OwnsConfigTopic` declines, so no
  config the window offers is ever spared by a claim. The guard is kept —
  it is the net under `publisher.Runtime.PublishComponent` (the documented
  rollback direction) and under any future per-entity publish, and a guard
  deleted while it is inert is a guard nobody puts back. Its two tests now
  mint the claim directly on the runtime instead of relying on the call
  graph, and each still fails if its half of the subtraction is removed.
  Said out loud here rather than left for a reader to notice.
- **`OwnsConfigTopic` was deliberately NOT widened to the bundle form.**
  Doing so would let this daemon retract `homeassistant/device/*/config` —
  a topic space shared with every Tasmota-style writer and, more to the
  point, with a **sibling mtec instance's entire fleet**. openccu-loom's
  #817 is repairing exactly that class of defect. Mutation-verified: the
  widening turns `TestSweepRetractsOnlyOurOwnOrphans` red.

---

## 6. F16 — the node-id collision, worked out rather than assumed

**The question**: two default-configured instances are *completely*
mutually indistinguishable in the per-entity form (§4.3 of the step-4/5
results). A document is one topic per device. If two instances derived the
same node id they would not merely overwrite — each would retract and
republish the other's document, forever.

**They do not.** `discovery.NodeID` is `topic.Slug(dev.UID())`, and this
bridge's `NewDevice` gives the device a single identifier with **no
namespace and the bare serial as its value**. So:

```
node id = topic.Slug(<inverter serial>)      e.g. "mt1234567890"
topic   = homeassistant/device/<node id>/config
```

- **Per-inverter.** Two serials, two topics. Neither instance can reach the
  other's document, and the sweep declines the form anyway (§5).
- **Stable.** Derived from the STATIC read and from nothing configurable:
  not `DEVICE_NAME`, not `MQTT_TOPIC`, not `HASS_UNIQUE_ID_INCLUDE_SERIAL`.
  A moved node id would leave the old document retained, announcing the
  same device from a second topic.
- **A legal topic segment**, because `topic.Slug` makes it one and
  `discovery.Validate` refuses a node id that is not already sanitised —
  and a blocking validation issue withholds the whole migration (§4). An
  exotic serial therefore produces a slugged topic or no document, never a
  malformed one.

All four asserted by `TestBundleNodeIDIsTheSerialAndNothingElse`.

**So the bundle does not require `HASS_UNIQUE_ID_INCLUDE_SERIAL`, and it
makes the topic half of F16 strictly better than the form it replaces.**

**What it does not fix, and this is the finding**: the identities *inside*
the two documents. With the flag off — the shipped default — two instances
publish two different documents declaring the **same 91 `unique_id`s**
(100 of 100 `(platform, unique_id)` pairs shared;
`TestTwoDefaultInstancesStillShareEveryUniqueID`). Home Assistant keys its
entity registry on `(domain, platform, unique_id)` and binds each identity
to whichever device declared it first. The failure mode therefore *changes
shape*: from two writers silently overwriting one set of topics, to two
devices where the second one's entities do not materialise. Louder, still
broken, and the documented answer is still the flag — now stated in the
README and the changelog as something to set on **both** instances
**before** upgrading, and with the collision described as total rather than
partial.

**One residual risk, recorded**: `topic.Slug` case-folds, so two inverters
whose serials differ *only* in case would share a document. Accepted, and
named in the test rather than left to be discovered.

---

## 7. `Bundle.Tombstones` and `RemoveComponents`


**What the library actually does**, verified in code rather than read off
the doc comment (`TestOmittingAComponentDoesNotRemoveIt`):

- A component **omitted** from a new document is **not removed** by Home
  Assistant. It keeps the entity. `SupersededTopics` renders no legacy
  topic for it either, so its old per-entity config is not retracted.
- The removal has to be **expressed**: an entry that is present and carries
  a platform and nothing else (`{"platform":"sensor"}`). An empty object is
  not enough; an absent key is not enough.
- `Bundle.Tombstones` (`json:"-"`) remembers what the removed component
  *was*, outside the payload, because the tombstone's own entry cannot
  carry a `unique_id` — putting it back would un-remove the entity. That
  remembered `unique_id` is what lets `SupersededTopics` render the legacy
  retraction for a removed entity under
  `publisher.LegacyTopicByUniqueID`, which is this fleet's form.
- `Bundle.RemoveComponents(was, keys...)` is the call for the case that
  actually occurs — an entity that left the catalogue is not rendered at
  all, so the new document never held it and `Bundle.Remove` has nothing to
  remember.

**What this daemon relies on**: that a component present in the document is
declared, and that the 100 present components are the whole of what has to
be retracted. That is all §2.1 needs, and it holds.

<a id="f17"></a>
**F17 — this daemon cannot express a removal, and that is a capability
lost by this release.** `RemoveComponents` needs the *previous* document.
This daemon has no memory of it: the catalogue is compiled in, nothing is
persisted across a restart, and the orphan sweep runs *after* the document
is published (so its snapshot contains the document this boot just wrote,
not the previous one). So when a future release withdraws a register, the
new document simply omits it and Home Assistant goes on showing it as a
permanently unavailable phantom.

Under the per-entity form the sweep handled exactly this. The move loses
it.

**Not fixed here, deliberately.** The fix is a pre-publish read-back of the
retained document — a new subscribe and a ~2 s window inserted *before* the
one publish of the whole release that cannot be undone. Adding that to the
irreversible step trades a known, bounded, cosmetic defect for an unknown
risk on the path that matters most. The workaround is to delete the entity
in Home Assistant. It is named in the changelog, in the README, and pinned
in code so the day this daemon grows that memory the contract it has to
satisfy is already written down.

The phase-5 pilot did not address this either.

---

## 8. The downgrade judgement, made explicitly

**A user who rolls back to 1.9.x after migrating gets no entities**, unless
they clear the retained document first. The document stays on the broker,
the old release republishes per-entity configs, and the same symmetry
refuses them with the same single `WARNING`.

**This is accepted.** The reasoning, stated so it is not implicit:

- The manual step is *one command* and it is documented in three places
  (`README.md`, `changelog.md`, `addon/DOCS.md`) with the exact topic
  spelled out:
  `mosquitto_pub -t homeassistant/device/<serial>/config -r -n`.
- Because nothing was re-keyed, the old release then re-adopts the **same
  entities with their history**. The rollback is lossless once the topic
  is cleared.
- The alternative — a rollback path inside the daemon
  (`Runtime.PublishComponent` retracts the document before writing each
  per-entity config, and the library provides it) — would ship a rollback
  that only the **new** binary can execute, which is precisely the binary a
  rolling-back user has stopped running. It would be code that cannot run
  when it is needed.
- The remaining option, publishing both forms, is the one thing Home
  Assistant refuses outright.

The pilot (go-zendure2mqtt #45) made the same call for the same reasons.

---

## 9. Mutation verification

See the PR body for the table. Every new assertion was verified to fail
under a perturbation of the production code it covers; each mutation was
reverted from a filesystem **copy**, never with `git checkout --`, and the
work was committed before the harness ran.

---

## 10. What could not be settled without a live broker

The refusal itself, and Home Assistant's acceptance of the resulting
document. Everything here proves the retraction list is right, that it is
written first, that a failure withholds the document, that an invalid
document withholds the retraction, that the crash window heals and that the
payloads are identical — but that Home Assistant *accepts* the document is
still the 2026-09-10/11 measurement's result, reproduced against no broker
in CI. Also unverified in CI: that a renamed, re-iconed entity survives the
move in a real registry.
