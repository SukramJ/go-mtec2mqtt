// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-mtec2mqtt/internal/mqtt"
	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

// spawnPolls registers one goroutine per polling cadence on g. Each
// goroutine runs until the run context is cancelled; a single
// per-cycle error never aborts the goroutine — only ctx cancellation
// does.
//
// Group → cadence mapping mirrors the Python coordinator exactly so
// the on-the-wire request pattern is unchanged.
func (c *Coordinator) spawnPolls(g *errgroup.Group, ctx context.Context) {
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
		idx := int(c.secondaryIdx.Load()) % len(secondaryGroups)
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

// publishGroupOnce is the single-cycle worker: read the group from
// Modbus, process values, compute pseudo-registers, publish each
// value to its MQTT topic. Always returns — errors land in the log.
func (c *Coordinator) publishGroupOnce(ctx context.Context, log *slog.Logger, group registers.Group) {
	if c.topicBase == "" {
		// Initialisation hasn't completed yet — nothing to publish under.
		return
	}
	raw, readErr := c.deps.Reader.ReadGroup(ctx, group)
	if readErr != nil {
		log.Warn("coordinator.read_failed", slog.String("err", readErr.Error()))
	}
	if len(raw) == 0 {
		return
	}
	processed := processValues(c.deps.Catalog, raw)
	if pseudo := PseudoRegisters(string(group), processed, c.deps.Now()); pseudo != nil {
		for k, v := range pseudo {
			processed[k] = v
		}
	}
	for key, val := range processed {
		topic := fmt.Sprintf("%s/%s/%s/state", c.topicBase, group, key)
		payload := formatValue(val, c.deps.Cfg.GoFloatVerb())
		if err := c.deps.MQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, false); err != nil {
			log.Warn("coordinator.publish_failed",
				slog.String("topic", topic),
				slog.String("err", err.Error()))
		}
	}
	log.Debug("coordinator.published", slog.Int("count", len(processed)))
}
