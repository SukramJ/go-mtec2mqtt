// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package web serves the optional embedded status/health dashboard.
//
// It is a thin HTTP layer over a [Backend] (implemented by the
// coordinator): a small REST API plus a Server-Sent-Events stream feed a
// single-page app that is compiled into the binary via go:embed. The
// package depends only on the standard library and the shared
// internal/state DTOs, so enabling the UI adds no third-party
// dependencies and keeps the daemon a single static binary.
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
