// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package state

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

func TestUpdateGroupSnapshotIsolated(t *testing.T) {
	s := New()
	vals := map[string]any{"power": 1234.0, "soc": 80}
	s.UpdateGroup("now-base", vals, t0)

	// Mutating the caller's map must not affect the stored copy.
	vals["power"] = 9999.0

	snap := s.Snapshot()
	g, ok := snap.Groups["now-base"]
	if !ok {
		t.Fatal("group missing from snapshot")
	}
	if g.Values["power"] != 1234.0 {
		t.Errorf("store kept a reference, got %v", g.Values["power"])
	}
	if !g.UpdatedAt.Equal(t0) {
		t.Errorf("UpdatedAt = %v, want %v", g.UpdatedAt, t0)
	}

	// Mutating the returned snapshot must not affect the store.
	g.Values["power"] = 0
	if s.Snapshot().Groups["now-base"].Values["power"] != 1234.0 {
		t.Error("snapshot shares state with store")
	}
}

func TestSetStatic(t *testing.T) {
	s := New()
	s.SetStatic("SN123", "V1.2", "Model-X", t0)
	serial, fw, equip := s.Static()
	if serial != "SN123" || fw != "V1.2" || equip != "Model-X" {
		t.Errorf("static mismatch: %q %q %q", serial, fw, equip)
	}
	snap := s.Snapshot()
	if snap.Serial != "SN123" || snap.Firmware != "V1.2" || snap.Equipment != "Model-X" {
		t.Errorf("snapshot static mismatch: %+v", snap)
	}
}

func TestGroupHealthAge(t *testing.T) {
	s := New()
	s.UpdateGroup("day", map[string]any{"a": 1, "b": 2}, t0)
	gh := s.GroupHealth(t0.Add(30 * time.Second))
	d, ok := gh["day"]
	if !ok {
		t.Fatal("missing group health")
	}
	if d.AgeSeconds != 30 {
		t.Errorf("age = %d, want 30", d.AgeSeconds)
	}
	if d.Count != 2 {
		t.Errorf("count = %d, want 2", d.Count)
	}
}

func TestSubscribeReceivesAndCancels(t *testing.T) {
	s := New()
	ch, cancel := s.Subscribe()

	s.UpdateGroup("now-base", map[string]any{"x": 1}, t0)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber not notified")
	}

	// Coalescing: two rapid updates collapse to (at most) one pending wake.
	s.UpdateGroup("now-base", map[string]any{"x": 2}, t0)
	s.UpdateGroup("now-base", map[string]any{"x": 3}, t0)
	<-ch // drain the single pending wake
	select {
	case <-ch:
		t.Error("expected coalesced wake, got a second immediately")
	default:
	}

	cancel()
	// After cancel the channel is closed; a receive must not block.
	select {
	case _, open := <-ch:
		if open {
			t.Error("channel should be closed after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("receive on cancelled channel blocked")
	}

	// Cancel is idempotent — a second call must not panic.
	cancel()
}

func TestNotifyDoesNotBlockOnFullSubscriber(t *testing.T) {
	s := New()
	_, cancel := s.Subscribe() // never drained
	defer cancel()
	// Many updates against a never-draining subscriber must not deadlock.
	done := make(chan struct{})
	go func() {
		for i := range 100 {
			s.UpdateGroup("g", map[string]any{"i": i}, t0)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify blocked on a full subscriber")
	}
}
