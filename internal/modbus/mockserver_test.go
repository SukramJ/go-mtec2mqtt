// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package modbus

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
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
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				// transient read error — just drop the connection
			}
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
		if _, err := conn.Write(out); err != nil {
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
