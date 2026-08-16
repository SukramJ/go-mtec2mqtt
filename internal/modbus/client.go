// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

// Package modbus is the Modbus-TCP transport for the M-TEC Energybutler.
//
// The package implements only the function codes the inverter actually
// speaks (FC03 Read-Holding-Registers, FC06 Write-Single-Register) on
// top of the byte-deterministic codec in [protocol]. It is a pure
// transport: one TCP connection, sequential request/reply, no retry
// and no reconnect. Resilience patterns (backoff, circuit breaker,
// watchdog) live one layer up in the coordinator so this package
// stays trivially testable.
package modbus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus/protocol"
)

// Default timeout used when [Config.Timeout] is zero — matches the
// MODBUS_TIMEOUT default in the legacy Python config.
const defaultTimeout = 5 * time.Second

// Transport-level errors. Wrap rather than match by ==.
var (
	ErrNotConnected = errors.New("modbus: not connected")
	ErrTIDMismatch  = errors.New("modbus: transaction-id mismatch (stale response?)")
	ErrUnitMismatch = errors.New("modbus: unit-id mismatch in response")
)

// Config holds the parameters for [New].
type Config struct {
	// Host is the inverter / espressif-gateway hostname or IP.
	Host string
	// Port is usually 502 (firmware ≥ V27.52.4.0). Older firmware uses 5743.
	Port int
	// UnitID is the Modbus slave id — 247 for M-TEC by default.
	UnitID byte
	// Timeout caps the I/O wait for one full request/response round-trip.
	// Zero falls back to [defaultTimeout].
	Timeout time.Duration
	// Logger is optional; nil → slog.Default().
	Logger *slog.Logger
}

// Client is a Modbus-TCP transport. The zero value is unusable — call
// [New]. Connect and Close are idempotent guards; the underlying
// connection is the only mutable state and is protected by `mu`.
//
// Concurrent callers of ReadHoldingRegisters / WriteSingleRegister are
// serialised on the wire — the inverter does not tolerate pipelined
// transactions on its single-slot Modbus stack.
type Client struct {
	cfg    Config
	logger *slog.Logger
	addr   string

	mu      sync.Mutex
	conn    net.Conn
	nextTID atomic.Uint32 // logical uint16 — see nextTransactionID

	// connected and activeConn mirror conn for the two callers that must
	// never queue behind a wire transaction: IsConnected (polled by
	// /api/health and by every SSE event) and Close (shutdown). do holds
	// mu for a full round-trip — up to MODBUS_TIMEOUT, which operators
	// may raise to minutes — so both work off these lock-free views
	// instead. Every mutation of conn goes through setConnLocked so the
	// three stay in sync.
	connected  atomic.Bool
	activeConn atomic.Pointer[net.Conn]
}

// New constructs a Client; it does not open a TCP connection. Call
// [Client.Connect] before issuing any reads or writes.
func New(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{
		cfg:    cfg,
		logger: cfg.Logger,
		addr:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
	}
}

// Connect opens the TCP socket. Returns nil if already connected.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return nil
	}
	dialer := &net.Dialer{Timeout: c.cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("modbus: dial %s: %w", c.addr, err)
	}
	c.setConnLocked(conn)
	c.logger.Info("modbus.connected", slog.String("addr", c.addr))
	return nil
}

// Close tears down the TCP connection. Safe to call repeatedly.
//
// A transaction that is parked in Read/Write on the socket is cut short
// first: forcing an already-expired deadline aborts the blocked I/O
// immediately, so shutdown does not wait out MODBUS_TIMEOUT on mu. The
// interrupted transaction takes its usual poison path and observes the
// socket as gone.
func (c *Client) Close() error {
	if cp := c.activeConn.Load(); cp != nil {
		_ = (*cp).SetDeadline(time.Now())
	}
	c.mu.Lock()
	conn := c.conn
	c.setConnLocked(nil)
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	err := conn.Close()
	c.logger.Info("modbus.disconnected", slog.String("addr", c.addr))
	return err
}

// IsConnected reports whether the client currently holds a TCP socket.
// Note: a "yes" here means the socket exists in our bookkeeping; the
// peer may still have closed it without us noticing yet.
//
// Reads the lock-free mirror of conn on purpose: health checks and the
// SSE stream call this on every tick and must not block behind an
// in-flight wire transaction.
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// setConnLocked installs conn (nil to clear) and republishes it to the
// lock-free views used by IsConnected and Close. Caller must hold c.mu.
func (c *Client) setConnLocked(conn net.Conn) {
	c.conn = conn
	if conn == nil {
		c.activeConn.Store(nil)
		c.connected.Store(false)
		return
	}
	c.activeConn.Store(&conn)
	c.connected.Store(true)
}

// ReadHoldingRegisters issues FC03 and returns the raw 16-bit values.
// On a Modbus exception the returned error is a *protocol.ExceptionError
// — use errors.As to inspect it.
func (c *Client) ReadHoldingRegisters(ctx context.Context, address, count uint16) ([]uint16, error) {
	req, err := protocol.EncodeReadHoldingRegisters(address, count)
	if err != nil {
		return nil, err
	}
	respPDU, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	regs, err := protocol.DecodeReadHoldingResponse(respPDU)
	if err != nil {
		return nil, err
	}
	// A well-framed response may still carry a different quantity than
	// requested (stale buffer replayed under a fresh TID by a buggy
	// gateway). The frame was fully consumed, so the stream stays in
	// sync — reject without poisoning, mirroring the FC06 echo check.
	if len(regs) != int(count) {
		return nil, fmt.Errorf("modbus: FC03 returned %d registers, requested %d", len(regs), count)
	}
	return regs, nil
}

// WriteSingleRegister issues FC06. On success the device echoes the
// request; we verify the echo and surface a mismatch as an error.
func (c *Client) WriteSingleRegister(ctx context.Context, address, value uint16) error {
	req := protocol.EncodeWriteSingleRegister(address, value)
	respPDU, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	gotAddr, gotVal, err := protocol.DecodeWriteSingleResponse(respPDU)
	if err != nil {
		return err
	}
	if gotAddr != address || gotVal != value {
		return fmt.Errorf("modbus: FC06 echo mismatch: got addr=%d val=%d, sent addr=%d val=%d",
			gotAddr, gotVal, address, value)
	}
	return nil
}

// do runs one MBAP request → response round-trip under the connection
// lock. On any I/O or framing error the socket is torn down so the
// caller observes a clean "not connected" state on the next call;
// reconnect is the caller's responsibility.
func (c *Client) do(ctx context.Context, pdu []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, ErrNotConnected
	}
	// Fail fast on an already-cancelled context so calls queued on c.mu
	// (and subsequent clusters of a group read) abort immediately on
	// shutdown instead of each running a full wire transaction.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("modbus: %w", err)
	}

	tid := c.nextTransactionID()
	frame, err := protocol.EncodeFrame(tid, c.cfg.UnitID, pdu)
	if err != nil {
		return nil, err
	}

	deadline := c.effectiveDeadline(ctx)
	if err := c.conn.SetDeadline(deadline); err != nil {
		c.poisonLocked()
		return nil, fmt.Errorf("modbus: set deadline: %w", err)
	}

	// Interrupt in-flight I/O on cancellation: the run context carries
	// no deadline (signal.NotifyContext), so without this hook a blocked
	// Write/ReadFull would run until now+Timeout after SIGTERM. Forcing
	// an immediate deadline surfaces a timeout error that takes the
	// existing poisonLocked path. The hook captures the conn value — not
	// the c.conn field, which is nilled under mu by poisonLocked — and is
	// armed after the SetDeadline above so a cancellation firing in
	// between cannot be overwritten. disarm runs before mu is released
	// (defers unwind LIFO), so the hook can never reach the transaction
	// of the next caller.
	hook := armDeadlineHook(ctx, c.conn)
	defer hook.disarm()

	if _, err := c.conn.Write(frame); err != nil {
		c.poisonLocked()
		return nil, fmt.Errorf("modbus: write: %w", err)
	}

	header := make([]byte, protocol.HeaderLen)
	if _, err := io.ReadFull(c.conn, header); err != nil {
		c.poisonLocked()
		return nil, fmt.Errorf("modbus: read header: %w", err)
	}
	h, err := protocol.DecodeHeader(header)
	if err != nil {
		c.poisonLocked()
		return nil, err
	}
	if h.TransactionID != tid {
		c.poisonLocked()
		return nil, fmt.Errorf("%w: got %d, want %d", ErrTIDMismatch, h.TransactionID, tid)
	}
	if h.UnitID != c.cfg.UnitID {
		c.poisonLocked()
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnitMismatch, h.UnitID, c.cfg.UnitID)
	}

	respPDU := make([]byte, h.Length())
	if _, err := io.ReadFull(c.conn, respPDU); err != nil {
		c.poisonLocked()
		return nil, fmt.Errorf("modbus: read pdu: %w", err)
	}
	return respPDU, nil
}

// deadlineHook forces an immediate deadline on one connection when a
// transaction's context is cancelled, and can be disarmed race-free.
//
// context.AfterFunc's own stop is not enough: it reports that the hook
// had already started but does not wait for it. A cancellation arriving
// at the very tail of a transaction could therefore expire the deadline
// after do() had released mu — killing the unrelated next transaction of
// another caller on the same socket.
type deadlineHook struct {
	mu   sync.Mutex
	done bool
	stop func() bool
}

// armDeadlineHook registers the cancellation hook for conn on ctx.
func armDeadlineHook(ctx context.Context, conn net.Conn) *deadlineHook {
	h := &deadlineHook{}
	h.stop = context.AfterFunc(ctx, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.done {
			return
		}
		_ = conn.SetDeadline(time.Now())
	})
	return h
}

// disarm blocks any future hook run and waits out one already in flight.
// Once it returns, no SetDeadline from this hook can reach the socket.
func (h *deadlineHook) disarm() {
	h.mu.Lock()
	h.done = true
	h.mu.Unlock()
	h.stop()
}

// effectiveDeadline takes the tighter of (ctx deadline, now + Timeout).
// Pure helper, no mutation, no I/O.
func (c *Client) effectiveDeadline(ctx context.Context) time.Time {
	d := time.Now().Add(c.cfg.Timeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		return cd
	}
	return d
}

// poisonLocked drops the current connection so the next call surfaces
// ErrNotConnected instead of attempting another I/O on a half-broken
// socket. Caller must hold c.mu.
func (c *Client) poisonLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.setConnLocked(nil)
}

// nextTransactionID returns a non-zero uint16. We avoid 0 so the value
// can double as a "not yet assigned" sentinel in logs and debug dumps.
func (c *Client) nextTransactionID() uint16 {
	for {
		v := uint16(c.nextTID.Add(1) & 0xFFFF) //nolint:gosec // narrowed on purpose
		if v != 0 {
			return v
		}
	}
}
