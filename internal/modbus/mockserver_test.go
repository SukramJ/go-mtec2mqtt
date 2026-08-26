// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package modbus

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus/protocol"
)

// handler decides how the mock server replies to a single decoded
// request. It returns the response PDU (already including the function
// code) or a non-nil ExceptionError. If both are nil the connection is
// closed without a reply — useful for simulating mid-request drops.
type handler func(req []byte) (pdu []byte, exc *protocol.ExceptionError)

// mockServer is a minimal in-process Modbus-TCP responder used by the
// transport tests. It is not safe for production use.
type mockServer struct {
	t        *testing.T
	listener net.Listener
	addr     *net.TCPAddr
	handler  handler
	delay    time.Duration
	// mutate, when set, rewrites the fully encoded response frame just
	// before it is written — used to inject wrong transaction-ids,
	// unit-ids or truncated frames. Returning a shorter slice combined
	// with closeAfterReply simulates a peer dying mid-response.
	mutate          func(frame []byte) []byte
	closeAfterReply bool

	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

func newMockServer(t *testing.T, h handler) *mockServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mock listen: %v", err)
	}
	s := &mockServer{
		t:        t,
		listener: l,
		addr:     l.Addr().(*net.TCPAddr),
		handler:  h,
	}
	t.Cleanup(s.Close)
	s.wg.Add(1)
	go s.acceptLoop()
	return s
}

func (s *mockServer) Port() int { return s.addr.Port }

// withDelay holds back each response by d — used to provoke timeouts.
func (s *mockServer) withDelay(d time.Duration) *mockServer {
	s.delay = d
	return s
}

// withMutator installs a response-frame rewriter (see mutate field).
func (s *mockServer) withMutator(fn func(frame []byte) []byte) *mockServer {
	s.mutate = fn
	return s
}

// withCloseAfterReply drops the connection right after each response —
// combined with a truncating mutator this leaves the client waiting for
// bytes that never arrive.
func (s *mockServer) withCloseAfterReply() *mockServer {
	s.closeAfterReply = true
	return s
}

func (s *mockServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.listener.Close()
	s.wg.Wait()
}

func (s *mockServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		s.wg.Add(1)
		go s.serve(conn)
	}
}

func (s *mockServer) serve(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	for {
		header := make([]byte, protocol.HeaderLen)
		if _, err := io.ReadFull(conn, header); err != nil {
			// EOF/closed on shutdown or a transient read error —
			// either way, drop the connection.
			return
		}
		h, err := protocol.DecodeHeader(header)
		if err != nil {
			return
		}
		req := make([]byte, h.Length())
		if _, err := io.ReadFull(conn, req); err != nil {
			return
		}
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		pdu, exc := s.handler(req)
		if pdu == nil && exc == nil {
			// Caller signalled "drop the connection mid-request" — leave.
			return
		}
		respPDU := pdu
		if exc != nil {
			respPDU = []byte{req[0] | 0x80, exc.ExceptionCode}
		}
		out := make([]byte, protocol.HeaderLen+len(respPDU))
		binary.BigEndian.PutUint16(out[0:2], h.TransactionID)
		// out[2:4] protocol-id = 0
		binary.BigEndian.PutUint16(out[4:6], uint16(len(respPDU)+1))
		out[6] = h.UnitID
		copy(out[protocol.HeaderLen:], respPDU)
		if s.mutate != nil {
			out = s.mutate(out)
		}
		if _, err := conn.Write(out); err != nil {
			return
		}
		if s.closeAfterReply {
			return
		}
	}
}

// canned builds a normal FC03 response PDU from the given registers.
func cannedFC03(values []uint16) []byte {
	body := make([]byte, len(values)*2)
	for i, v := range values {
		binary.BigEndian.PutUint16(body[i*2:], v)
	}
	out := make([]byte, 2+len(body))
	out[0] = protocol.FCReadHoldingRegisters
	out[1] = byte(len(body))
	copy(out[2:], body)
	return out
}

// cannedFC06 builds a normal FC06 echo response PDU.
func cannedFC06(address, value uint16) []byte {
	out := make([]byte, 5)
	out[0] = protocol.FCWriteSingleRegister
	binary.BigEndian.PutUint16(out[1:3], address)
	binary.BigEndian.PutUint16(out[3:5], value)
	return out
}

// --- scriptedConn -----------------------------------------------------------

// scriptedConn is an in-memory net.Conn stand-in that answers every FC03
// request from Client.do and, unlike a real socket, lets a test pin the
// exact instant at which a response is "fully read". That is what makes
// the stale-cancel-hook regression reproducible: the cancellation has to
// land in the few microseconds between the last successful read and
// do()'s return.
//
// It models the one socket behaviour the cancel hook relies on: a
// deadline in the past aborts pending and subsequent I/O with
// os.ErrDeadlineExceeded, while a fresh future deadline re-arms it.
type scriptedConn struct {
	mu        sync.Mutex
	buf       []byte
	expired   bool
	expiredCh chan struct{}
	// forced counts SetDeadline calls with a non-future deadline, i.e.
	// exactly the hook's "abort now" signal.
	forced int
	// delay holds every Read back before it serves bytes — widens the
	// window in which a stray deadline can kill an in-flight read.
	delay time.Duration
	// afterFinalRead runs right after the read that drains the buffer,
	// i.e. once the client has consumed the whole response frame.
	afterFinalRead func()
	closed         bool
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{expiredCh: make(chan struct{})}
}

func (s *scriptedConn) setDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

func (s *scriptedConn) setAfterFinalRead(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterFinalRead = fn
}

// forcedDeadlines reports how often something forced an immediate
// deadline on this connection.
func (s *scriptedConn) forcedDeadlines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forced
}

func (s *scriptedConn) Read(p []byte) (int, error) {
	s.mu.Lock()
	expired, ch, delay := s.expired, s.expiredCh, s.delay
	s.mu.Unlock()
	if expired {
		return 0, os.ErrDeadlineExceeded
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ch:
			return 0, os.ErrDeadlineExceeded
		}
	}

	s.mu.Lock()
	switch {
	case s.expired:
		s.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	case s.closed, len(s.buf) == 0:
		s.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	hook := s.afterFinalRead
	drained := len(s.buf) == 0
	s.mu.Unlock()

	if drained && hook != nil {
		hook()
	}
	return n, nil
}

// Write decodes the outgoing MBAP frame and queues a matching FC03
// reply, so the client's transaction-id and unit-id checks pass.
func (s *scriptedConn) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.expired {
		s.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	if s.closed {
		s.mu.Unlock()
		return 0, net.ErrClosed
	}
	s.mu.Unlock()

	tid := binary.BigEndian.Uint16(p[0:2])
	unit := p[6]
	count := binary.BigEndian.Uint16(p[10:12])
	pdu := cannedFC03(make([]uint16, count))
	frame := make([]byte, protocol.HeaderLen+len(pdu))
	binary.BigEndian.PutUint16(frame[0:2], tid)
	binary.BigEndian.PutUint16(frame[4:6], uint16(len(pdu)+1))
	frame[6] = unit
	copy(frame[protocol.HeaderLen:], pdu)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, frame...)
	return len(p), nil
}

func (s *scriptedConn) SetDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if t.IsZero() || t.After(time.Now()) {
		if s.expired {
			s.expired = false
			s.expiredCh = make(chan struct{})
		}
		return nil
	}
	s.forced++
	if !s.expired {
		s.expired = true
		close(s.expiredCh)
	}
	return nil
}

func (s *scriptedConn) SetReadDeadline(t time.Time) error  { return s.SetDeadline(t) }
func (s *scriptedConn) SetWriteDeadline(t time.Time) error { return s.SetDeadline(t) }

func (s *scriptedConn) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *scriptedConn) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func (s *scriptedConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
}
