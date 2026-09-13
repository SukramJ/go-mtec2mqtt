// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
)

// reconcileCollectWindow is how long the sweep listens to the broker's
// retained discovery configs before judging. The broker replays the
// retained set right after the subscribe, so a brief window captures all
// of them; a window that ends early does not mis-delete — the pass
// retracts strictly what it saw — it only leaves orphans for the next
// boot.
//
// A var, not a const, so tests can shorten the wait.
var reconcileCollectWindow = 2 * time.Second

// reconcileOrphans clears this daemon's retained Home Assistant discovery
// configs that are no longer in the just-published set (entities that were
// removed, renamed or re-platformed across catalog or daemon versions), so
// they do not linger as unavailable entities in Home Assistant.
//
// It runs asynchronously and is gated by a TryLock: a re-entrant call while
// one pass is in flight is skipped, since discovery changes are infrequent
// and a single pass suffices. (The library serialises its own snapshot
// windows too — two windows on one filter leave the second handler
// installed over the first and the first teardown unsubscribes for both —
// but that gate makes a second caller WAIT where this one returns.)
func (c *Coordinator) reconcileOrphans(ctx context.Context, published map[string]bool) {
	if c.deps.HASS == nil || !c.reconcileGate.TryLock() {
		return
	}
	go func() {
		defer c.reconcileGate.Unlock()
		c.sweepOrphans(ctx, published)
	}()
}

// sweepOrphans runs one report-only snapshot of the discovery tree and
// retracts what it owns and no longer publishes.
//
// # Why report-only, and then retracted here
//
// [publisher.SweepRequest.ReportOnly] runs the window without retracting
// anything: every owned config is parsed and handed to Inspect, the result
// names what a retracting pass WOULD have cleared, and not one message
// goes out. This daemon then decides and retracts through
// [publisher.Runtime.Retract] with a list it chose itself.
//
// That is not caution for its own sake. The library's retracting pass
// judges a topic on [publisher.SweepRequest.Owns] alone, which sees the
// parsed topic and nothing else — while this daemon's ownership rule has
// always been the stronger one: the retained PAYLOAD must carry a
// unique_id in this bridge's namespace AND a state topic under this
// bridge's MQTT root ([hass.Discovery.IsOwnConfig]). Retracting on the
// topic namespace alone would widen what this daemon is willing to delete
// from a discovery tree it shares with every other integration, inside a
// step whose whole claim is that nothing moved.
//
// The width of an ownership predicate is a live hazard rather than a
// hypothetical one: openccu-loom's PR #817 is repairing exactly this class
// of defect — its retraction prefixes turned out to own 100 % of a sibling
// daemon's configs. So ownership is checked twice here, on the topic
// ([hass.OwnsConfigTopic]: five platforms, the "MTEC_" namespace and the
// four-segment form only) and on the body, and a config is retracted only
// if both agree and neither claim set names it.
//
// # What the library owns that the hand-rolled version did not
//
//   - One snapshot window at a time per runtime, held across the
//     retractions as well, so a later pass sees the tree an earlier one
//     left behind.
//   - The subscription is taken down on every exit path, a cancelled
//     context and a broker that refuses the UNSUBSCRIBE included. The
//     hand-rolled version retried the unsubscribe three times and then
//     gave up loudly, leaving the daemon subscribed for the process
//     lifetime with a map nobody reads still growing.
//   - All three discovery topic forms parsed, so a device document or a
//     five-segment config is recognised and declined rather than misread.
//   - The in-flight claim: a config still inside its own Publish call is
//     already on the broker and already delivered to this window, while
//     the declared set records it only afterwards. Without that check a
//     sweep running concurrently with a publish retracts the config that
//     publish just wrote.
func (c *Coordinator) sweepOrphans(ctx context.Context, published map[string]bool) {
	log := c.deps.Logger
	// Refused outright when there is no device document. Since the move to
	// the bundle this daemon publishes no four-segment per-entity config at
	// all, so every one the window finds is an orphan by the rule below —
	// which is exactly right when the document went out, and catastrophic
	// when it did not: it would delete the working entities of the release
	// being upgraded from and put nothing in their place. buildBundle
	// leaves haBundle nil precisely so a document that does not validate
	// withholds the whole migration, and that has to include this pass.
	if c.haBundle == nil {
		log.Warn("coordinator.reconcile_sweep_skipped",
			slog.String("reason", "no device document was published"))
		return
	}
	prefix := c.deps.HARuntime.Prefix()

	var (
		mu    sync.Mutex
		owned []string
	)
	res, err := c.deps.HARuntime.Sweep(ctx, publisher.SweepRequest{
		ReportOnly: true,
		Window:     reconcileCollectWindow,
		Owns:       hass.OwnsConfigTopic,
		Inspect: func(t publisher.ConfigTopic, body []byte) {
			// Called on the transport's read loop: cheap, and it publishes
			// nothing. The retraction happens after Sweep returns.
			if !c.deps.HASS.IsOwnConfig(body) {
				return // another writer's config in the same namespace
			}
			// Rebuilt through the library's own renderer for this fleet's
			// topic form rather than by string concatenation, because it is
			// the same function step 6's bundle must retract with — 100 of
			// 100 of this bridge's retained configs, measured against the
			// pins. Exercising it here is what keeps the constant honest.
			topic := hass.LegacyConfigTopic(prefix, hass.Platform(t.Platform), t.ObjectID)
			if topic == "" {
				return
			}
			mu.Lock()
			owned = append(owned, topic)
			mu.Unlock()
		},
	})
	if err != nil {
		log.Warn("coordinator.reconcile_sweep_failed", slog.String("err", err.Error()))
		return
	}

	// Two claim sets, and both are needed. published is what this boot's
	// discovery batch minted, which is the question an orphan actually
	// answers — and it includes an entry whose publish FAILED, so a
	// transient broker error never makes the sweep clear an entity this
	// daemon still intends to publish. declared is what the runtime has
	// written for the whole process, and subtracting it is the safety net
	// the library's own retracting pass applies and a report-only pass
	// does not: SweepResult.Owned lists every owned topic the window saw,
	// claimed or not.
	claimed := make(map[string]bool, len(published))
	for topic := range published {
		claimed[topic] = true
	}
	for _, topic := range c.deps.HARuntime.Declared() {
		claimed[topic] = true
	}
	mu.Lock()
	orphans := make([]string, 0, len(owned))
	for _, topic := range owned {
		if !claimed[topic] {
			orphans = append(orphans, topic)
		}
	}
	mu.Unlock()

	// res.Inspected is logged beside the orphan count because the pair is
	// what makes a silent sweep diagnosable: zero inspected means the
	// window saw none of this daemon's retained configs at all, which is a
	// completely different fault from a window that saw all 100 and
	// correctly found nothing orphaned. Both look like "0 cleared" in a log
	// line that reports only the second number.
	if len(orphans) == 0 {
		log.Debug("coordinator.discovery_orphans_none", slog.Int("inspected", res.Inspected))
		return
	}
	if err := c.deps.HARuntime.Retract(ctx, orphans...); err != nil {
		log.Warn("coordinator.reconcile_clear_failed", slog.String("err", err.Error()))
		return
	}
	log.Info("coordinator.discovery_orphans_cleared",
		slog.Int("inspected", res.Inspected),
		slog.Int("count", len(orphans)))
}
