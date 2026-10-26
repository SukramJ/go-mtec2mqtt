// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package web is this bridge's side of go-bridgeui, the dashboard every
// go-*2mqtt bridge embeds: it presents the coordinator's state ([Backend],
// in this bridge's own DTOs) as go-bridgeui's contract, and names the
// page's layout and labels. The server, the API, the hardening and the
// single-page app are go-bridgeui's.
package web

import (
	"context"

	"github.com/SukramJ/go-mtec2mqtt/internal/state"
)

// Backend is the read/write surface the web server needs. The
// coordinator satisfies it structurally — defining it here (rather than
// importing the coordinator) keeps the dependency arrow pointing one
// way and makes the handlers trivially testable with a fake.
type Backend interface {
	// Snapshot returns the current live register values.
	Snapshot() state.Snapshot
	// Health returns the operational status surface.
	Health() state.Health
	// Config returns the sanitised, read-only config projection.
	Config() state.ConfigView
	// Registers returns catalog metadata for labelling and edit controls.
	Registers() []state.RegisterInfo
	// Write sets a writable register by its MQTT suffix, synchronously.
	Write(ctx context.Context, mqttKey, value string) error
	// Changes returns a coalesced change-notification channel plus a
	// cancel func to release it.
	Changes() (<-chan struct{}, func())
}
