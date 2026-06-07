// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package state

import (
	"maps"
	"sync"
	"time"
)

// Store is the thread-safe live cache the coordinator writes to and the
// web layer reads from. The zero value is unusable — call [New].
//
// Writes happen on the poll goroutines (one per group); reads happen on
// HTTP handler goroutines. A single RWMutex guards everything; the data
// volume is tiny (a few hundred values) so contention is a non-issue.
type Store struct {
	mu        sync.RWMutex
	serial    string
	firmware  string
	equipment string
	groups    map[string]GroupView

	// subs holds one buffered channel per active SSE subscriber. A
	// change coalesces into a single pending wakeup per subscriber
	// (buffer of 1 + non-blocking send), so a burst of group updates
	// never blocks a poll goroutine on a slow client.
	subs map[chan struct{}]struct{}
}

// New returns an empty, ready-to-use Store.
func New() *Store {
	return &Store{
		groups: make(map[string]GroupView),
		subs:   make(map[chan struct{}]struct{}),
	}
}

// UpdateGroup replaces the cached values for one polling group and wakes
// every subscriber. values is copied defensively so the caller may keep
// mutating its map.
func (s *Store) UpdateGroup(group string, values map[string]any, now time.Time) {
	cp := make(map[string]any, len(values))
	maps.Copy(cp, values)
	s.mu.Lock()
	s.groups[group] = GroupView{UpdatedAt: now, Values: cp}
	s.mu.Unlock()
	s.notify()
}

// SetStatic records the inverter identity learned from the first STATIC
// read and wakes subscribers so the header updates immediately.
func (s *Store) SetStatic(serial, firmware, equipment string, _ time.Time) {
	s.mu.Lock()
	s.serial = serial
	s.firmware = firmware
	s.equipment = equipment
	s.mu.Unlock()
	s.notify()
}

// Snapshot returns a deep copy of the current live view, safe to hand to
// a JSON encoder without holding the lock.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	groups := make(map[string]GroupView, len(s.groups))
	for name, g := range s.groups {
		vals := make(map[string]any, len(g.Values))
		maps.Copy(vals, g.Values)
		groups[name] = GroupView{UpdatedAt: g.UpdatedAt, Values: vals}
	}
	return Snapshot{
		Serial:    s.serial,
		Firmware:  s.firmware,
		Equipment: s.equipment,
		Groups:    groups,
	}
}

// GroupHealth returns a freshness summary per group as of now.
func (s *Store) GroupHealth(now time.Time) map[string]GroupHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]GroupHealth, len(s.groups))
	for name, g := range s.groups {
		out[name] = GroupHealth{
			UpdatedAt:  g.UpdatedAt,
			AgeSeconds: int64(now.Sub(g.UpdatedAt).Seconds()),
			Count:      len(g.Values),
		}
	}
	return out
}

// Static returns the cached inverter identity.
func (s *Store) Static() (serial, firmware, equipment string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.serial, s.firmware, s.equipment
}

// Subscribe registers a change channel for an SSE client. The returned
// channel receives an empty struct (coalesced) whenever the store
// changes; call the returned cancel func to unregister and release it.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
	return ch, cancel
}

// notify pokes every subscriber without blocking. A subscriber that
// already has a pending wakeup (full buffer) is skipped — it will pick
// up the latest state when it next reads.
func (s *Store) notify() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
