// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mtec2mqtt/internal/evcc"
	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
)

// This file implements the shared evcc profile (internal/evcc): the grid,
// PV and battery meters derived from every group read, and the battery
// mode command.
//
// A battery mode other than normal puts the inverter into the operation
// mode evcc's own template uses for it (Economic to hold, UPS to charge
// from the grid). The mode the inverter was in before is remembered when
// it leaves normal and written back when evcc returns to normal, so the
// operator's own setting survives. The memory lives in the process: after
// a restart in hold or charge there is nothing to restore, and normal
// writes nothing.

// modeKey is the MQTT key of the inverter operation mode (register 50000).
const modeKey = "mode"

// batteryModeWriteKey routes a battery mode through the write queue. It
// cannot collide with a register key: those have no slash.
var batteryModeWriteKey = evcc.Group + "/" + evcc.BatteryMode.String()

// evccState is the battery mode as commanded through the profile.
type evccState struct {
	mu sync.Mutex
	// mode is the battery mode last applied; "" until the first one.
	mode evcc.Mode
	// saved is the operation mode the inverter was in when it left
	// normal, to be restored on the way back; valid while hasSaved.
	saved    int
	hasSaved bool
	// lastCommand is when a battery mode command last arrived, for
	// EVCC_BATTERYMODE_TIMEOUT.
	lastCommand time.Time
}

// current returns the battery mode in force, normal before the first one.
func (s *evccState) current() evcc.Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == "" {
		return evcc.ModeNormal
	}
	return s.mode
}

// publishEVCC publishes the profile values derivable from one group read,
// with the read's timestamp.
func (c *Coordinator) publishEVCC(ctx context.Context, log *slog.Logger, tp topicParts, processed map[string]any, now time.Time) {
	if !c.deps.Cfg.EVCCEnable {
		return
	}
	for item, v := range evcc.Derive(processed) {
		// Rounded like the source register's own value.
		reg := c.deps.Catalog.FindByMQTT(evcc.SourceKey(item))
		c.publishEVCCValue(ctx, log, tp, item, reg.Round(v), now)
	}
}

func (c *Coordinator) publishEVCCValue(ctx context.Context, log *slog.Logger, tp topicParts, item evcc.Item, v any, now time.Time) {
	topic := hass.EVCCStateTopic(tp.root, tp.serial, item.Role, item.Key)
	if _, err := c.deps.StatePlane.PublishStatus(ctx, topic, publisher.Observation{Value: v, At: now}); err != nil {
		log.Warn("coordinator.publish_failed", slog.String("topic", topic), slog.String("err", err.Error()))
	}
}

// publishBatteryMode publishes the battery mode in force.
func (c *Coordinator) publishBatteryMode(ctx context.Context) {
	tp, ok := c.loadTopicParts()
	if !ok || !c.deps.Cfg.EVCCEnable {
		return
	}
	c.publishEVCCValue(ctx, c.deps.Logger, tp, evcc.BatteryMode, string(c.evcc.current()), c.deps.Now())
}

// evccStatusTopics lists every topic the profile publishes, for the
// self-echo guard.
func (c *Coordinator) evccStatusTopics(tp topicParts) []string {
	if !c.deps.Cfg.EVCCEnable {
		return nil
	}
	var out []string
	for _, it := range evcc.StatusItems() {
		out = append(out, hass.EVCCStateTopic(tp.root, tp.serial, it.Role, it.Key))
	}
	return out
}

// onEVCCSet handles one routed `<name>/set/<serial>/evcc/<role>/<key>`.
//
// evcc re-sends the battery mode (through its watchdog plugin), so an
// unchanged mode only refreshes the timeout and goes no further: the
// inverter is written only when the mode changes.
func (c *Coordinator) onEVCCSet(_ context.Context, cmd publisher.Command, v publisher.SetValue) {
	tp, ok := c.loadTopicParts()
	if !ok || len(cmd.Wildcards) != 3 || cmd.Wildcards[0] != tp.serial {
		return
	}
	reject := func(err error) {
		c.deps.Logger.Warn("coordinator.set_rejected",
			slog.String("topic", cmd.Topic),
			slog.String("payload", string(cmd.Payload)),
			slog.String("err", err.Error()))
	}
	if (evcc.Item{Role: cmd.Wildcards[1], Key: cmd.Wildcards[2]}) != evcc.BatteryMode {
		reject(fmt.Errorf("%w: unknown command %s/%s", evcc.ErrCommand, cmd.Wildcards[1], cmd.Wildcards[2]))
		return
	}
	if v.Structured() {
		reject(errStructuredSet)
		return
	}
	mode, err := evcc.ParseMode(v.Text)
	if err != nil {
		reject(err)
		return
	}
	c.evcc.mu.Lock()
	c.evcc.lastCommand = c.deps.Now()
	unchanged := mode == c.evcc.mode || (c.evcc.mode == "" && mode == evcc.ModeNormal)
	c.evcc.mu.Unlock()
	if unchanged {
		return
	}
	_ = c.enqueueWrite(writeReq{
		mqttKey: batteryModeWriteKey,
		value:   string(mode),
		topic:   cmd.Topic,
		payload: string(cmd.Payload),
	})
}

// applyBatteryMode runs on the write worker. It reads the operation mode
// first, so a mode the inverter already has is not written again, and so
// the mode to restore is the one the inverter really had.
func (c *Coordinator) applyBatteryMode(ctx context.Context, value string) error {
	target, err := evcc.ParseMode(value)
	if err != nil {
		return fmt.Errorf("%w: %w", modbus.ErrValueParse, err)
	}
	from := c.evcc.current()
	if target == from {
		return nil
	}
	code, err := c.readMode(ctx)
	if err != nil {
		return err
	}

	c.evcc.mu.Lock()
	saved, hasSaved := c.evcc.saved, c.evcc.hasSaved
	c.evcc.mu.Unlock()
	if from == evcc.ModeNormal {
		saved, hasSaved = code, true
	}

	want, ok := target.Code()
	switch {
	case ok:
	case hasSaved:
		want = saved
	default:
		c.deps.Logger.Info("coordinator.evcc_batterymode_nothing_to_restore",
			slog.Int("mode", code))
		want = code
	}
	if want != code {
		if err := c.deps.Reader.WriteRegisterByMQTT(ctx, modeKey, strconv.Itoa(want)); err != nil {
			return err
		}
	}

	c.evcc.mu.Lock()
	c.evcc.mode = target
	c.evcc.saved, c.evcc.hasSaved = saved, hasSaved && target != evcc.ModeNormal
	c.evcc.mu.Unlock()
	c.deps.Logger.Info("coordinator.evcc_batterymode",
		slog.String("from", string(from)),
		slog.String("to", string(target)),
		slog.Int("operation_mode", want))
	c.publishBatteryMode(ctx)
	return nil
}

// readMode reads the inverter operation mode code.
func (c *Coordinator) readMode(ctx context.Context) (int, error) {
	reg := c.deps.Catalog.FindByMQTT(modeKey)
	if reg == nil || !reg.Writable {
		return 0, fmt.Errorf("%w: the battery mode needs the writable %q register in registers.yaml",
			modbus.ErrUnknownRegister, modeKey)
	}
	raw, err := c.deps.Reader.ReadRegister(ctx, reg.Key)
	if err != nil {
		return 0, err
	}
	code, ok := toInt(raw)
	if !ok {
		return 0, fmt.Errorf("%w: operation mode %v is not a number", modbus.ErrValueParse, raw)
	}
	return code, nil
}

// evccWatchdog returns the battery to normal when hold or charge has not
// been re-sent within EVCC_BATTERYMODE_TIMEOUT, so an evcc that stopped
// does not leave the battery locked.
func (c *Coordinator) evccWatchdog(ctx context.Context) error {
	timeout := c.deps.Cfg.EVCCBatteryModeTimeoutDuration()
	tick := min(max(timeout/4, evccWatchdogMinTick), evccWatchdogMaxTick)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		c.evcc.mu.Lock()
		mode := c.evcc.mode
		idle := c.deps.Now().Sub(c.evcc.lastCommand)
		expired := mode != "" && mode != evcc.ModeNormal && idle > timeout
		if expired {
			// Restarts the clock, so the fallback is queued once.
			c.evcc.lastCommand = c.deps.Now()
		}
		c.evcc.mu.Unlock()
		if !expired {
			continue
		}
		c.deps.Logger.Warn("coordinator.evcc_batterymode_timeout",
			slog.String("mode", string(mode)),
			slog.Duration("idle", idle))
		_ = c.enqueueWrite(writeReq{mqttKey: batteryModeWriteKey, value: string(evcc.ModeNormal)})
	}
}

// evccWatchdogMinTick / evccWatchdogMaxTick bound the watchdog's check
// interval. Vars so tests can shorten them.
var (
	evccWatchdogMinTick = time.Second
	evccWatchdogMaxTick = 5 * time.Second
)
