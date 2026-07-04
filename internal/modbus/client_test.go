// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package modbus

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus/protocol"
)

const testUnitID byte = 0xF7 // 247, same as the M-TEC default

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

// --- happy path -------------------------------------------------------------

func TestReadHoldingRegistersHappy(t *testing.T) {
	want := []uint16{0xFFFF, 0xFE0C} // I32 −500
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		if req[0] != protocol.FCReadHoldingRegisters {
			t.Errorf("unexpected FC: 0x%02x", req[0])
		}
		return cannedFC03(want), nil
	})
	c := newTestClient(t, srv.Port())
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadHoldingRegisters(ctx, 11000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWriteSingleRegisterHappy(t *testing.T) {
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		// Echo what the client sent, per FC06 spec.
		return cannedFC06(0xCB20 /* 52000 */, 0x0001), nil
	})
	c := newTestClient(t, srv.Port())
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteSingleRegister(ctx, 52000, 1); err != nil {
		t.Fatal(err)
	}
}

// --- exception responses ----------------------------------------------------

func TestReadHoldingExceptionBubbles(t *testing.T) {
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		return nil, &protocol.ExceptionError{
			Function:      protocol.FCReadHoldingRegisters,
			ExceptionCode: protocol.ExceptionIllegalDataAddress,
		}
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadHoldingRegisters(context.Background(), 60000, 1)
	var exc *protocol.ExceptionError
	if !errors.As(err, &exc) {
		t.Fatalf("want *ExceptionError, got %v", err)
	}
	if exc.ExceptionCode != protocol.ExceptionIllegalDataAddress {
		t.Fatalf("unexpected code: %+v", exc)
	}
}

func TestWriteSingleExceptionBubbles(t *testing.T) {
	srv := newMockServer(t, func(req []byte) ([]byte, *protocol.ExceptionError) {
		return nil, &protocol.ExceptionError{
			Function:      protocol.FCWriteSingleRegister,
			ExceptionCode: protocol.ExceptionIllegalDataAddress,
		}
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := c.WriteSingleRegister(context.Background(), 60000, 1)
	var exc *protocol.ExceptionError
	if !errors.As(err, &exc) {
		t.Fatalf("want *ExceptionError, got %v", err)
	}
	if exc.ExceptionCode != protocol.ExceptionIllegalDataAddress {
		t.Fatalf("unexpected code: %+v", exc)
	}
}

// --- connection state -------------------------------------------------------

func TestConnectFailure(t *testing.T) {
	// A cancelled context makes DialContext fail deterministically —
	// dialing a "known closed" port instead would race against the OS
	// re-issuing it to a concurrent listener.
	c := New(Config{Host: "127.0.0.1", Port: 1, UnitID: testUnitID})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Connect(ctx); err == nil {
		t.Fatal("want dial error with cancelled context, got nil")
	}
	if c.IsConnected() {
		t.Fatal("failed Connect must not mark the client connected")
	}
}

func TestWriteWithoutConnect(t *testing.T) {
	c := New(Config{Host: "127.0.0.1", Port: 1, UnitID: testUnitID})
	if err := c.WriteSingleRegister(context.Background(), 52000, 1); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}

func TestReadHoldingRegistersInvalidCount(t *testing.T) {
	c := New(Config{Host: "127.0.0.1", Port: 1, UnitID: testUnitID})
	if _, err := c.ReadHoldingRegisters(context.Background(), 0, 0); !errors.Is(err, protocol.ErrCountOutOfRange) {
		t.Fatalf("want ErrCountOutOfRange, got %v", err)
	}
}

func TestReadWithoutConnect(t *testing.T) {
	c := New(Config{Host: "127.0.0.1", Port: 1, UnitID: testUnitID})
	_, err := c.ReadHoldingRegisters(context.Background(), 0, 1)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}

func TestConnectIdempotent(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return cannedFC03([]uint16{0x0001}), nil
	})
	c := newTestClient(t, srv.Port())
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("second connect should be no-op, got %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("expected IsConnected=true")
	}
}

func TestCloseIdempotent(t *testing.T) {
	c := New(Config{Host: "127.0.0.1", Port: 1, UnitID: testUnitID})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- failure modes ---------------------------------------------------------

func TestTimeoutPoisonsConnection(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return cannedFC03([]uint16{0}), nil
	}).withDelay(200 * time.Millisecond)

	c := New(Config{
		Host:    "127.0.0.1",
		Port:    srv.Port(),
		UnitID:  testUnitID,
		Timeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadHoldingRegisters(context.Background(), 11000, 1)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// After a timeout the next request must observe ErrNotConnected so
	// callers can decide whether to reconnect — exactly the contract the
	// resilience layer relies on.
	if _, err := c.ReadHoldingRegisters(context.Background(), 11000, 1); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("after timeout, want ErrNotConnected, got %v", err)
	}
}

func TestContextDeadlineTighterThanTimeout(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return cannedFC03([]uint16{0}), nil
	}).withDelay(300 * time.Millisecond)

	c := New(Config{
		Host:    "127.0.0.1",
		Port:    srv.Port(),
		UnitID:  testUnitID,
		Timeout: 5 * time.Second, // larger than ctx deadline
	})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.ReadHoldingRegisters(ctx, 11000, 1)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from tight ctx deadline")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("ctx deadline ignored — elapsed %s", elapsed)
	}
}

func TestConnectionDropDuringResponse(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return nil, nil // signal: close without reply
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadHoldingRegisters(context.Background(), 11000, 1)
	if err == nil {
		t.Fatal("expected error on dropped connection")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		// Some platforms surface this as a generic read error; both are fine.
		t.Logf("drop surfaced as %v (acceptable)", err)
	}
}

func TestWriteSingleEchoMismatch(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		// Echo a different value than the client sent.
		return cannedFC06(0xCB20, 0x0002), nil
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := c.WriteSingleRegister(context.Background(), 52000, 1)
	if err == nil || !contains(err.Error(), "echo mismatch") {
		t.Fatalf("want echo-mismatch error, got %v", err)
	}
}

// requireNextCallNotConnected asserts the poison contract: after a
// framing/transport error the very next call must observe
// ErrNotConnected so the resilience layer knows to reconnect.
func requireNextCallNotConnected(t *testing.T, c *Client) {
	t.Helper()
	if _, err := c.ReadHoldingRegisters(context.Background(), 11000, 1); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("connection not poisoned: want ErrNotConnected, got %v", err)
	}
}

func TestTIDMismatchPoisonsConnection(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return cannedFC03([]uint16{0x0001}), nil
	}).withMutator(func(frame []byte) []byte {
		frame[0] ^= 0xFF // corrupt the transaction-id
		return frame
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadHoldingRegisters(context.Background(), 11000, 1)
	if !errors.Is(err, ErrTIDMismatch) {
		t.Fatalf("want ErrTIDMismatch, got %v", err)
	}
	requireNextCallNotConnected(t, c)
}

func TestUnitMismatchPoisonsConnection(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return cannedFC03([]uint16{0x0001}), nil
	}).withMutator(func(frame []byte) []byte {
		frame[6] ^= 0xFF // corrupt the unit-id
		return frame
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadHoldingRegisters(context.Background(), 11000, 1)
	if !errors.Is(err, ErrUnitMismatch) {
		t.Fatalf("want ErrUnitMismatch, got %v", err)
	}
	requireNextCallNotConnected(t, c)
}

func TestBadProtocolIDPoisonsConnection(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return cannedFC03([]uint16{0x0001}), nil
	}).withMutator(func(frame []byte) []byte {
		frame[2] = 0x01 // MBAP protocol-id must be 0
		return frame
	})
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadHoldingRegisters(context.Background(), 11000, 1)
	if !errors.Is(err, protocol.ErrBadProtocolID) {
		t.Fatalf("want ErrBadProtocolID, got %v", err)
	}
	requireNextCallNotConnected(t, c)
}

func TestTruncatedResponsePoisonsConnection(t *testing.T) {
	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		return cannedFC03([]uint16{0x0001}), nil
	}).withMutator(func(frame []byte) []byte {
		return frame[:len(frame)-1] // header intact, PDU one byte short
	}).withCloseAfterReply()
	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadHoldingRegisters(context.Background(), 11000, 1)
	if err == nil {
		t.Fatal("expected error on truncated response")
	}
	requireNextCallNotConnected(t, c)
}

// --- concurrency ------------------------------------------------------------

func TestConcurrentReadsAreSerialised(t *testing.T) {
	var inflight atomic.Int32
	var maxInflight atomic.Int32

	srv := newMockServer(t, func([]byte) ([]byte, *protocol.ExceptionError) {
		cur := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if cur <= m || maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inflight.Add(-1)
		return cannedFC03([]uint16{0xABCD}), nil
	})

	c := newTestClient(t, srv.Port())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	const N = 6
	errs := make(chan error, N)
	for range N {
		go func() {
			_, err := c.ReadHoldingRegisters(context.Background(), 11000, 1)
			errs <- err
		}()
	}
	for i := range N {
		if err := <-errs; err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// Only one transaction at a time should ever be on the wire,
	// otherwise we'd risk corrupting the framing on a real device.
	if got := maxInflight.Load(); got != 1 {
		t.Fatalf("transactions were not serialised: max in-flight = %d", got)
	}
}

// --- helpers ----------------------------------------------------------------

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
