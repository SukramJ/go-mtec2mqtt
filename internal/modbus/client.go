// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package modbus is the Modbus-TCP layer for the M-TEC Energybutler: the
// catalog-aware [Reader] on top of the shared transport in
// github.com/SukramJ/go-modbus.
//
// The transport (MBAP codec, sequential client, deadline handling) moved
// to go-modbus so go-mtec2mqtt and go-keba2mqtt share one implementation.
// The aliases below keep this package's call sites unchanged.
package modbus

import gomodbus "github.com/SukramJ/go-modbus"

// Client is the go-modbus transport.
type Client = gomodbus.Client

// Config holds the parameters for [New].
type Config = gomodbus.Config

// New constructs a Client; it does not open a TCP connection.
func New(cfg Config) *Client { return gomodbus.New(cfg) }

// Transport-level errors, re-exported for errors.Is at the call sites.
var (
	ErrNotConnected = gomodbus.ErrNotConnected
	ErrTIDMismatch  = gomodbus.ErrTIDMismatch
	ErrUnitMismatch = gomodbus.ErrUnitMismatch
)
