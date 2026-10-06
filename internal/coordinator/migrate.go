// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SukramJ/go-hamqtt/topic"
	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// legacySweepWindow is how long the start-up sweep listens to the broker's
// retained old-layout topics before judging. The broker replays the
// retained set right after the subscribe, so a brief window captures it; a
// window that ends early does not mis-delete — the pass clears strictly
// what it saw — it only leaves the rest for the next start.
//
// A var, not a const, so tests can shorten the wait.
var legacySweepWindow = 2 * time.Second

// sweepLegacyTopics clears, once per process, the retained topics the
// pre-2.0 layout left for THIS instance: `<root>/<serial>/<group>/<key>/state`
// (and a retained `…/set` somebody published) for every register and
// synthetic switch this daemon publishes, and `<root>/bridge/status`
// (openccu-loom ADR 0083, "The retained sweep").
//
// It runs in the background so the poll loops start at once, and it stays
// for the life of the 2.x line: idempotent, so it also cleans after a
// rollback and re-upgrade.
func (c *Coordinator) sweepLegacyTopics(ctx context.Context) {
	tp, ok := c.loadTopicParts()
	if !ok {
		return
	}
	c.legacySweepGate.Do(func() {
		go c.runLegacySweep(ctx, tp)
	})
}

// legacyTopics is the exact set of pre-2.0 topics this instance owns: the
// old state and command topic of every register and synthetic switch it
// publishes, under its own root and its own serial, and the old bridge
// marker of its root.
//
// Exact topics and nothing else. A prefix rule is what ADR 0070's
// homeconnect review measured deleting 510 live components of a sibling
// instance; here a sibling inverter on the same root has another serial,
// so none of its topics is in this set and all of them survive.
func (c *Coordinator) legacyTopics(tp topicParts) map[string]bool {
	owned := map[string]bool{hass.LegacyBridgeStatusTopic(tp.root): true}
	add := func(group registers.Group, key string) {
		owned[hass.LegacyStateTopic(tp.root, tp.serial, group, key)] = true
		owned[hass.LegacyCommandTopic(tp.root, tp.serial, group, key)] = true
	}
	for _, r := range c.deps.Catalog.All {
		if r.Group == "" {
			continue
		}
		// The reader's map key, and so the topic key: the mqtt suffix,
		// the register name when there is none.
		key := r.MQTT
		if key == "" {
			key = r.Name
		}
		add(r.Group, key)
	}
	for _, v := range c.deps.Virtual {
		add(registers.Group(v.Group), v.Key)
	}
	return owned
}

// legacySweepTargets judges what one window saw: a retained topic is
// cleared only when it is in owned, and never when its second level — the
// first below the root — is a function name, because that is the 2.0
// layout (`<name>/status/…`, `<name>/connected`, …), which this sweep must
// not touch whatever owned says. Serials cannot spell a function name, so
// the second rule removes nothing the first would have kept; it is there so
// the sweep stays safe if the owned set is ever widened.
func legacySweepTargets(root string, retained []string, owned map[string]bool) []string {
	var out []string
	for _, t := range retained {
		rest, ok := strings.CutPrefix(t, root+"/")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, "/")
		if topic.IsFunction(first) || !owned[t] {
			continue
		}
		out = append(out, t)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// legacySweepFilters are the two subscriptions the sweep's window opens:
// the old layout's tree under this serial, and the old bridge marker.
//
// Not `<root>/#`, which the ADR names: that filter overlaps this daemon's
// own command routes (`<root>/set/+/+/+`, `<root>/maintenance/set/#`), and
// a broker delivers one copy per matching subscription, which go-mqtt then
// hands to every matching handler — a `set` arriving during the window
// would be written to the inverter twice (the overlap go-hamqtt's router
// refuses for its own routes). Every topic the sweep may clear lies under
// these two, so nothing it owns is out of sight; it also does not receive
// this daemon's own new status tree.
func legacySweepFilters(tp topicParts) []string {
	return []string{tp.root + "/" + tp.serial + "/#", hass.LegacyBridgeStatusTopic(tp.root)}
}

func (c *Coordinator) runLegacySweep(ctx context.Context, tp topicParts) {
	log := c.deps.Logger
	if topic.IsFunction(tp.serial) {
		// `<root>/<serial>/#` would then be a function tree of the new
		// layout, overlapping the command routes. No inverter reports such
		// a serial; refusing is cheaper than reasoning about one.
		log.Warn("coordinator.legacy_sweep_skipped",
			slog.String("serial", tp.serial),
			slog.String("reason", "the serial spells an mqtt-smarthome function name"))
		return
	}
	owned := c.legacyTopics(tp)
	filters := legacySweepFilters(tp)

	var (
		mu       sync.Mutex
		retained []string
	)
	handler := func(msg *mqtt.Message) {
		// The read loop: collect, publish nothing. An empty retained
		// payload is already a clear.
		if !msg.Retain || len(msg.Payload) == 0 {
			return
		}
		mu.Lock()
		retained = append(retained, msg.Topic)
		mu.Unlock()
	}
	unsubscribe := func(subscribed []string) {
		unsubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, f := range subscribed {
			if err := c.deps.MQTT.Unsubscribe(unsubCtx, f); err != nil {
				log.Warn("coordinator.legacy_sweep_unsubscribe_failed",
					slog.String("filter", f), slog.String("err", err.Error()))
			}
		}
	}
	for i, f := range filters {
		if _, err := c.deps.MQTT.Subscribe(ctx, f, mqtt.QoS0, handler); err != nil {
			log.Warn("coordinator.legacy_sweep_failed",
				slog.String("filter", f), slog.String("err", err.Error()))
			unsubscribe(filters[:i])
			return
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(legacySweepWindow):
	}
	unsubscribe(filters)
	if ctx.Err() != nil {
		return
	}

	mu.Lock()
	seen := slices.Clone(retained)
	mu.Unlock()
	targets := legacySweepTargets(tp.root, seen, owned)
	cleared := 0
	for _, t := range targets {
		if err := c.deps.MQTT.Publish(ctx, t, nil, mqtt.QoS0, true); err != nil {
			log.Warn("coordinator.legacy_sweep_clear_failed",
				slog.String("topic", t), slog.String("err", err.Error()))
			continue
		}
		cleared++
	}
	// inspected beside cleared, so "0 cleared" tells a window that saw
	// nothing apart from one that found nothing of the old layout.
	log.Info("coordinator.legacy_sweep",
		slog.Int("inspected", len(seen)),
		slog.Int("cleared", cleared))
}
