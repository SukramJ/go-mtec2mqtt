// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/SukramJ/go-mqtt"
)

// reconcileCollectWindow is how long we collect retained discovery configs
// after subscribing; the broker replays the retained set right after the
// subscribe, so a brief window captures all of them.
const reconcileCollectWindow = 2 * time.Second

// reconcileOrphans clears this daemon's retained Home Assistant discovery
// configs that are no longer in the just-published set (entities that were
// removed, renamed or re-platformed across catalog or daemon versions), so
// they do not linger as unavailable entities in Home Assistant.
//
// It subscribes to the discovery-config filter, collects the retained configs
// the broker replays for [reconcileCollectWindow], then publishes an empty
// retained payload to each topic that is ours ([hass.Discovery.IsOwnConfig])
// and absent from published.
//
// It runs asynchronously and is gated by a TryLock: a re-entrant call while
// one reconcile is in flight is skipped, since discovery changes are
// infrequent and a single pass suffices.
func (c *Coordinator) reconcileOrphans(ctx context.Context, published map[string]bool) {
	if c.deps.HASS == nil || !c.reconcileGate.TryLock() {
		return
	}
	go func() {
		defer c.reconcileGate.Unlock()
		log := c.deps.Logger
		filter := c.deps.HASS.ConfigFilter()

		var mu sync.Mutex
		retained := map[string][]byte{}
		if err := c.deps.MQTT.Subscribe(ctx, filter, mqtt.QoS0, func(topic string, payload []byte, _ bool) {
			mu.Lock()
			retained[topic] = append([]byte(nil), payload...)
			mu.Unlock()
		}); err != nil {
			log.Warn("coordinator.reconcile_subscribe_failed", slog.String("err", err.Error()))
			return
		}

		// Retained configs arrive right after subscribe; collect briefly.
		select {
		case <-ctx.Done():
			// ctx is cancelled here; derive a non-cancelled child so the
			// unsubscribe still goes out without breaking the context chain.
			_ = c.deps.MQTT.Unsubscribe(context.WithoutCancel(ctx), filter)
			return
		case <-time.After(reconcileCollectWindow):
		}
		_ = c.deps.MQTT.Unsubscribe(ctx, filter)

		mu.Lock()
		orphans := c.orphanTopics(retained, published)
		mu.Unlock()

		cleared := 0
		for _, topic := range orphans {
			// An empty retained payload tells the broker to drop the config.
			if err := c.deps.MQTT.Publish(ctx, topic, nil, mqtt.QoS0, true); err == nil {
				cleared++
			}
		}
		if cleared > 0 {
			log.Info("coordinator.discovery_orphans_cleared", slog.Int("count", cleared))
		}
	}()
}

// orphanTopics returns the retained config topics that are ours
// ([hass.Discovery.IsOwnConfig]) and no longer present in the published set.
// An already-cleared (empty) topic, a still-current entity, or a foreign
// integration's config is never returned, so cleanup only ever touches our
// own stale entities.
func (c *Coordinator) orphanTopics(retained map[string][]byte, published map[string]bool) []string {
	var out []string
	for topic, payload := range retained {
		if len(payload) == 0 || published[topic] {
			continue // already cleared, or still a current entity
		}
		if !c.deps.HASS.IsOwnConfig(payload) {
			continue // belongs to another integration — never touch it
		}
		out = append(out, topic)
	}
	return out
}
