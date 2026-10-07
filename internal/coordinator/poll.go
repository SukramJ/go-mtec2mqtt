// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// spawnPolls registers one goroutine per polling cadence on g. Each
// goroutine runs until the run context is cancelled; a single
// per-cycle error never aborts the goroutine — only ctx cancellation
// does.
//
// Group → cadence mapping mirrors the Python coordinator exactly so
// the on-the-wire request pattern is unchanged.
func (c *Coordinator) spawnPolls(ctx context.Context, g *errgroup.Group) {
	cfg := c.deps.Cfg
	g.Go(func() error {
		return c.pollLoop(ctx, "base", registers.GroupBase, cfg.RefreshNowDuration())
	})
	g.Go(func() error {
		return c.pollLoop(ctx, "config", registers.GroupConfig, cfg.RefreshConfigDuration())
	})
	g.Go(func() error {
		return c.pollSecondary(ctx, cfg.RefreshNowDuration())
	})
	g.Go(func() error {
		return c.pollLoop(ctx, "day", registers.GroupDay, cfg.RefreshDayDuration())
	})
	g.Go(func() error {
		return c.pollLoop(ctx, "total", registers.GroupTotal, cfg.RefreshTotalDuration())
	})
	g.Go(func() error {
		return c.pollLoop(ctx, "static", registers.GroupStatic, cfg.RefreshStaticDuration())
	})
}

// pollLoop is the generic per-group ticker. Reads → processes →
// publishes the group, then waits `every` before the next cycle.
// Read or publish errors are logged but the loop continues — the
// resilience contract is "publish what you can, retry next tick."
func (c *Coordinator) pollLoop(ctx context.Context, name string, group registers.Group, every time.Duration) error {
	log := c.deps.Logger.With(slog.String("group", name))
	log.Info("coordinator.poll_start", slog.Duration("every", every))

	// One immediate cycle on startup — matches the Python coordinator,
	// which calls read_register_group before the first sleep so HA
	// has values to show within seconds of boot.
	c.publishGroupOnce(ctx, log, group)

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.publishGroupOnce(ctx, log, group)
		}
	}
}

// pollSecondary cycles GRID → INVERTER → BACKUP → BATTERY → PV on
// every tick, one group per cycle. The Python coordinator does the
// same; the round-robin spreads the read load across the slow
// inverter Modbus stack without flooding it.
func (c *Coordinator) pollSecondary(ctx context.Context, every time.Duration) error {
	log := c.deps.Logger.With(slog.String("group", "secondary"))
	log.Info("coordinator.poll_start",
		slog.Duration("every", every),
		slog.Int("rotation", len(secondaryGroups)))

	step := func() {
		idx := secondaryIndex(c.secondaryIdx.Load())
		group := registers.Group(secondaryGroups[idx])
		c.secondaryIdx.Add(1)
		c.publishGroupOnce(ctx, log.With(slog.String("subgroup", string(group))), group)
	}
	step()

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			step()
		}
	}
}

// secondaryIndex maps the round-robin tick counter to a group index.
// The counter runs for the whole process lifetime and wraps to negative
// after 2^31 ticks; masking to unsigned before the modulo keeps the
// result in range, where a plain `int(v) % len` would hand the slice a
// negative index and panic.
func secondaryIndex(v int32) int {
	return int(uint32(v) % uint32(len(secondaryGroups))) //nolint:gosec // bit-pattern reinterpretation on purpose; the modulo result is < len(secondaryGroups)
}

// publishGroupOnce is the single-cycle worker: read the group from
// Modbus, process values, compute pseudo-registers, publish each
// value to its MQTT topic. Always returns — errors land in the log.
func (c *Coordinator) publishGroupOnce(ctx context.Context, log *slog.Logger, group registers.Group) {
	topicBase, ok := c.loadTopicParts()
	if !ok {
		// Initialisation hasn't completed yet — nothing to publish under.
		return
	}
	raw, readErr := c.deps.Reader.ReadGroup(ctx, group)
	if readErr != nil {
		log.Warn("coordinator.read_failed", slog.String("err", readErr.Error()))
	}
	// A read is the one place the inverter's reachability is observed, so
	// `<name>/connected` and the device's `online` item follow it here.
	c.updateUpstream(ctx)
	if len(raw) == 0 {
		return
	}
	now := c.deps.Now()
	processed := processValues(c.deps.Catalog, raw, c.deps.Cfg.Language)
	pseudo, skipped := PseudoRegisters(string(group), processed, now)
	maps.Copy(processed, pseudo)
	if len(skipped) > 0 {
		// A partial read (cluster timeout / reconnect window) leaves one of
		// the formula inputs out; publishing the derived value anyway would
		// mean publishing a wrong number. Skip it this cycle instead.
		log.Debug("coordinator.pseudo_skipped", slog.Any("keys", skipped))
	}
	// Derive the synthetic charge/discharge "active" switch states from
	// the freshly processed limits so they ride the same store + MQTT
	// publish path as a real register.
	c.applyVirtualSwitches(string(group), processed)
	// Mirror the processed values into the web cache (if enabled) before
	// publishing — same data the UI and MQTT see, decoupled from broker
	// availability. The UI keeps the labels of the configured language;
	// the wire carries tokens (see wireValue).
	if c.deps.Store != nil {
		c.deps.Store.UpdateGroup(string(group), processed, now)
	}
	published, written := 0, 0
	for key, val := range processed {
		// One function, not a second fmt.Sprintf: hass.StateTopic is the
		// same call internal/hass makes for the config's `state_topic`.
		// Two independent expressions for it once drifted (F5 of the
		// phase-6 measurement); a divergence points every entity at a topic
		// nobody writes to, permanently `unknown`, with nothing in the log.
		topic := hass.StateTopic(topicBase.root, topicBase.serial, string(group), key)
		reg := findRegisterByOutputKey(c.deps.Catalog, key)
		wire := wireValue(reg, raw[key], val, c.deps.Cfg.RoundFloat)
		c.noteUnmapped(log, reg, raw[key], wire)
		// An empty payload on a *retained* topic is MQTT's retraction, so a
		// value that is not there is skipped rather than published; the
		// library refuses a nil status value too
		// (publisher.ErrNilStatusValue), but refusing it here keeps the
		// warning that names the topic.
		if wire == nil {
			log.Warn("coordinator.empty_payload_skipped", slog.String("topic", topic))
			continue
		}
		// A {"val","ts","lc"} status object (mqtt-smarthome §5.2), retained,
		// at coordinator.StateQoS. The gate compares `val` only: a value
		// equal to the one the broker already retains is not written again,
		// whatever its timestamp — the spec's "publish on change", which
		// collapses roughly 11 136 messages an hour to the changes. `ts` is
		// this read, `lc` the read that last changed the value.
		ok, err := c.deps.StatePlane.PublishStatus(ctx, topic, publisher.Observation{Value: wire, At: now})
		if err != nil {
			log.Warn("coordinator.publish_failed",
				slog.String("topic", topic),
				slog.String("err", err.Error()))
		}
		if ok {
			written++
		}
		published++
	}
	// written is reported beside count because the gap between them is the
	// measurable effect of the dedup gate: a steady-state group should
	// show written=0, and an operator should be able to see that.
	log.Debug("coordinator.published", slog.Int("count", published), slog.Int("written", written))
}

// maxUnmappedLogged bounds the memory of [Coordinator.noteUnmapped]. A BIT
// register can in principle take 2^32 values; past this many distinct
// unmapped ones the warn line stops rather than the set growing.
const maxUnmappedLogged = 256

// noteUnmapped logs, once per register and raw value, a value-mapped
// register whose value the catalog does not map in full: an enum code
// with no label, or a fault field with a bit no mask names. The wire
// carries the "Unknown" token for it (or the named faults alone, when
// some bits are named), which Home Assistant shows as unknown — so this
// line is the only place the raw value is visible, and the hint that the
// catalog lacks an entry.
func (c *Coordinator) noteUnmapped(log *slog.Logger, reg *registers.Register, raw, wire any) {
	if reg == nil || reg.HassValueItems == nil || raw == nil {
		return
	}
	_, unmapped := decodeCode(raw, reg.HassValueItems)
	if unmapped == "" {
		return
	}
	key := reg.MQTT + "\x00" + fmt.Sprint(raw)
	c.unmappedMu.Lock()
	_, seen := c.unmappedSeen[key]
	full := len(c.unmappedSeen) >= maxUnmappedLogged
	if !seen && !full {
		if c.unmappedSeen == nil {
			c.unmappedSeen = make(map[string]struct{})
		}
		c.unmappedSeen[key] = struct{}{}
	}
	c.unmappedMu.Unlock()
	if seen || full {
		return
	}
	log.Warn("coordinator.unmapped_value",
		slog.String("register", reg.MQTT),
		slog.String("raw", fmt.Sprint(raw)),
		slog.String("unmapped", unmapped),
		slog.String("published", fmt.Sprint(wire)))
}
