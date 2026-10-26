// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/registers"
)

const evccBase = "MTEC/status/MTEC-TEST-001/evcc/"

// evccClock is a settable clock for the battery mode timeout.
type evccClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *evccClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *evccClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// buildEVCC is buildDeps with the profile on and an inverter whose
// operation mode (register 52000 in the test catalog) follows writes.
func buildEVCC(t *testing.T, mode int) (*Coordinator, *stubReader, *stubMQTT, *evccClock) {
	t.Helper()
	c, reader, mqttStub, _ := buildDeps(t, false)
	c.deps.Cfg.EVCCEnable = true
	clock := &evccClock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	c.deps.Now = clock.Now
	reader.regs = map[string]any{"52000": mode}
	reader.afterWrite = func(key, value string) {
		if key == "mode" {
			n, _ := strconv.Atoi(value)
			reader.regs["52000"] = n
		}
	}
	reader.groupData[registers.GroupBase] = map[string]any{
		"inverter":   3000,
		"grid_power": -500, // importing 500 W
	}
	return c, reader, mqttStub, clock
}

// startRun runs c until the returned stop is called, once the command
// route is up and the serial is known.
func startRun(t *testing.T, c *Coordinator, mqttStub *stubMQTT) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mqttStub.mu.Lock()
		ready := len(mqttStub.handlers) > 0
		mqttStub.mu.Unlock()
		if ready && c.loadTopicBase() != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() {
		cancel()
		<-done
	}
}

// waitFor polls cond for up to a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEVCCPublishesMetersWithEVCCSigns(t *testing.T) {
	c, _, mqttStub, _ := buildEVCC(t, 257)
	stop := startRun(t, c, mqttStub)
	defer stop()
	waitFor(t, "grid power", func() bool {
		return hasStatus(mqttStub.snapshotPublishes(), evccBase+"grid/power", 500.0)
	})
	mqttStub.mu.Lock()
	_, routed := mqttStub.handlers["MTEC/set/+/evcc/+/+"]
	mqttStub.mu.Unlock()
	if !routed {
		t.Error("evcc command route not subscribed")
	}
	if !hasStatus(mqttStub.snapshotPublishes(), evccBase+"battery/batterymode", "normal") {
		t.Errorf("batterymode not published as normal at start: %s", summariseTopics(mqttStub.snapshotPublishes()))
	}
}

func TestEVCCOffPublishesNothing(t *testing.T) {
	c, _, mqttStub, _ := buildDeps(t, false)
	runFor(t, c, 200*time.Millisecond)
	for _, p := range mqttStub.snapshotPublishes() {
		if startsWith(p.topic, evccBase) {
			t.Fatalf("published %s with EVCC_ENABLE off", p.topic)
		}
	}
	mqttStub.mu.Lock()
	defer mqttStub.mu.Unlock()
	for f := range mqttStub.handlers {
		if f == "MTEC/set/+/evcc/+/+" {
			t.Fatal("evcc route subscribed with EVCC_ENABLE off")
		}
	}
}

// Hold and charge write the operation mode once; normal restores the
// mode the inverter had before, and repeats write nothing.
func TestEVCCBatteryModeRestoresAndIsIdempotent(t *testing.T) {
	c, reader, mqttStub, _ := buildEVCC(t, 1) // "Eco" in the test catalog
	stop := startRun(t, c, mqttStub)
	defer stop()
	set := func(p string) {
		mqttStub.deliver("MTEC/set/MTEC-TEST-001/evcc/battery/batterymode", []byte(p))
	}
	writes := func() []writeCall { return reader.snapshotWrites() }

	set("1") // normal while normal: nothing to do
	set("2") // hold
	waitFor(t, "hold", func() bool { return len(writes()) == 1 })
	set("2")
	set("hold")
	set("3") // charge
	waitFor(t, "charge", func() bool { return len(writes()) == 2 })
	set("1") // back to normal: restore Eco
	waitFor(t, "restore", func() bool { return len(writes()) == 3 })
	set("1")
	time.Sleep(50 * time.Millisecond)

	want := []writeCall{{"mode", "258"}, {"mode", "259"}, {"mode", "1"}}
	if got := writes(); !slices.Equal(got, want) {
		t.Fatalf("writes %v, want %v", got, want)
	}
	waitFor(t, "status normal", func() bool {
		return hasStatus(mqttStub.snapshotPublishes(), evccBase+"battery/batterymode", "charge") &&
			hasStatus(mqttStub.snapshotPublishes(), evccBase+"battery/batterymode", "normal")
	})
}

// An inverter already in the target mode is not written, and the mode it
// is in is what normal restores.
func TestEVCCBatteryModeSkipsModeAlreadySet(t *testing.T) {
	c, reader, mqttStub, _ := buildEVCC(t, 258)
	stop := startRun(t, c, mqttStub)
	defer stop()
	mqttStub.deliver("MTEC/set/MTEC-TEST-001/evcc/battery/batterymode", []byte("2"))
	waitFor(t, "hold status", func() bool {
		return hasStatus(mqttStub.snapshotPublishes(), evccBase+"battery/batterymode", "hold")
	})
	mqttStub.deliver("MTEC/set/MTEC-TEST-001/evcc/battery/batterymode", []byte("1"))
	time.Sleep(100 * time.Millisecond)
	if got := reader.snapshotWrites(); len(got) != 0 {
		t.Fatalf("writes %v, want none", got)
	}
}

func TestEVCCBatteryModeRejectsUnknown(t *testing.T) {
	c, reader, mqttStub, _ := buildEVCC(t, 257)
	stop := startRun(t, c, mqttStub)
	defer stop()
	for _, p := range []string{"4", "discharge", `{"mode":2}`} {
		mqttStub.deliver("MTEC/set/MTEC-TEST-001/evcc/battery/batterymode", []byte(p))
	}
	mqttStub.deliver("MTEC/set/MTEC-TEST-001/evcc/battery/soc", []byte("50"))
	mqttStub.deliver("MTEC/set/OTHER/evcc/battery/batterymode", []byte("2"))
	time.Sleep(100 * time.Millisecond)
	if got := reader.snapshotWrites(); len(got) != 0 {
		t.Fatalf("writes %v, want none", got)
	}
}

// Without a re-sent command, hold falls back to normal after the timeout.
func TestEVCCBatteryModeTimeout(t *testing.T) {
	old := evccWatchdogMinTick
	evccWatchdogMinTick = 5 * time.Millisecond
	t.Cleanup(func() { evccWatchdogMinTick = old })

	c, reader, mqttStub, clock := buildEVCC(t, 257)
	c.deps.Cfg.EVCCBatteryModeTimeout = 1
	// The tick is timeout/4 clamped to [min, max]; shorten the max too.
	oldMax := evccWatchdogMaxTick
	evccWatchdogMaxTick = 10 * time.Millisecond
	t.Cleanup(func() { evccWatchdogMaxTick = oldMax })

	stop := startRun(t, c, mqttStub)
	defer stop()
	mqttStub.deliver("MTEC/set/MTEC-TEST-001/evcc/battery/batterymode", []byte("2"))
	waitFor(t, "hold", func() bool { return len(reader.snapshotWrites()) == 1 })

	clock.advance(500 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if n := len(reader.snapshotWrites()); n != 1 {
		t.Fatalf("fell back before the timeout: %v", reader.snapshotWrites())
	}
	clock.advance(time.Second)
	waitFor(t, "fallback", func() bool { return len(reader.snapshotWrites()) == 2 })
	want := []writeCall{{"mode", "258"}, {"mode", "257"}}
	if got := reader.snapshotWrites(); !slices.Equal(got, want) {
		t.Fatalf("writes %v, want %v", got, want)
	}
}
