// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/SukramJ/go-mtec2mqtt/internal/modbus"
	"github.com/SukramJ/go-mtec2mqtt/internal/state"
)

// liveView is the combined payload the SSE stream pushes and that the
// /api/status endpoint mirrors: everything the dashboard needs to redraw
// in one round-trip.
type liveView struct {
	Health   state.Health   `json:"health"`
	Snapshot state.Snapshot `json:"snapshot"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Health())
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, liveView{
		Health:   s.backend.Health(),
		Snapshot: s.backend.Snapshot(),
	})
}

func (s *Server) handleRegisters(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Registers())
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Config())
}

// writeRequest is the body of POST /api/write. Value is decoded as a raw
// JSON value so the client may send a number, string or bool — all get
// normalised to the string form the Modbus write path expects.
type writeRequest struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	var req writeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "missing 'key'")
		return
	}
	value := normaliseValue(req.Value)

	err := s.backend.Write(r.Context(), req.Key, value)
	switch {
	case err == nil:
		s.log.Info("web.write_ok",
			slog.String("key", req.Key), slog.String("value", value))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "key": req.Key, "value": value,
		})
	case errors.Is(err, modbus.ErrUnknownRegister):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, modbus.ErrNotWritable),
		errors.Is(err, modbus.ErrValueParse),
		errors.Is(err, modbus.ErrPseudoUnsupported):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, modbus.ErrNotConnected):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		// Transport / framing failures — the inverter is reachable in
		// config but the round-trip failed.
		s.log.Warn("web.write_failed",
			slog.String("key", req.Key), slog.String("err", err.Error()))
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

// handleEvents streams Server-Sent Events. It pushes a full liveView on
// connect, then on every change notification, plus a periodic tick so
// the uptime counter advances and the connection stays warm through
// idle proxies.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)

	changes, cancel := s.backend.Changes()
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	send := func() bool {
		view := liveView{Health: s.backend.Health(), Snapshot: s.backend.Snapshot()}
		payload, err := json.Marshal(view)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: update\ndata: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			if !send() {
				return
			}
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

// normaliseValue renders a decoded JSON value to the string form the
// Modbus write path parses. json.Number-free: encoding/json gives us
// float64 for numbers, so format those without a trailing ".000000".
func normaliseValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		// Integers commonly arrive as float64 (e.g. 3000) — render them
		// without a decimal point so the int parse branch wins downstream.
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a small JSON error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
