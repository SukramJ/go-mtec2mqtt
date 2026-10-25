// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package modbus

import (
	"testing"
	"time"
)

const testUnitID byte = 0xF7 // 247, the M-TEC default

// newTestClient returns a client for a modbustest.Server on port,
// closed when the test ends.
func newTestClient(t *testing.T, port int) *Client {
	t.Helper()
	c := New(Config{
		Host:    "127.0.0.1",
		Port:    port,
		UnitID:  testUnitID,
		Timeout: 500 * time.Millisecond,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}
