// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package registers

import "sort"

// gapThreshold is the maximum number of address-word holes the
// clusterer will tolerate before starting a new Modbus read. The value
// (10) is inherited from the Python coordinator and reflects the
// real-world trade-off between wasted bytes-on-the-wire and the
// overhead of a separate request — anything bigger hurts the inverter
// (which serialises requests internally), anything smaller fragments
// adjacent reads needlessly.
const gapThreshold = 10

// maxReadWords is the FC03 read limit (Modbus spec: 125 holding
// registers per request). It mirrors the unexported maxReadCount in
// internal/modbus/protocol — duplicated here because the registers
// package must stay below the modbus layer in the dependency graph.
// The loader rejects longer registers and the clusterer refuses to
// merge windows past it, so one oversized catalog entry can never make
// the encoder reject a whole cluster of healthy neighbours.
const maxReadWords = 125

// Cluster is one Modbus read window that covers a contiguous block of
// register addresses. The Members slice is sorted by address so the
// caller can compute each register's offset within the read response
// without a map lookup.
type Cluster struct {
	Start   uint16      // first holding-register address in the read
	Count   uint16      // number of 16-bit words the read returns
	Members []*Register // registers in this cluster, sorted by address
}

// Clusterize groups the given registers into one or more Modbus read
// windows, merging runs whose address gap is ≤ gapThreshold so the
// daemon issues the smallest possible number of FC03 requests per
// publication cycle.
//
// Pseudo-registers (Address == 0 and key != "0") are silently dropped
// — they are computed by the coordinator, not read from the device.
// Duplicate addresses keep only the first definition (matches the
// Python behaviour where the register-map dict overwrites on collision
// but the iteration order keeps the first hit).
func Clusterize(regs []*Register) []Cluster {
	type entry struct {
		reg    *Register
		end    int    // address + length in int — immune to uint16 wraparound
		length uint16 // clamped to 1..0xFFFF
	}
	entries := make([]entry, 0, len(regs))
	seen := make(map[uint16]bool, len(regs))
	for _, r := range regs {
		if !r.IsModbus() {
			continue
		}
		if seen[r.Address] {
			continue
		}
		seen[r.Address] = true
		length := uint16(1)
		if r.Length > 1 && r.Length <= 0xFFFF {
			length = uint16(r.Length)
		}
		entries = append(entries, entry{
			reg:    r,
			length: length,
			end:    int(r.Address) + int(length),
		})
	}
	if len(entries) == 0 {
		return nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].reg.Address < entries[j].reg.Address
	})

	clusters := []Cluster{{
		Start:   entries[0].reg.Address,
		Count:   entries[0].length,
		Members: []*Register{entries[0].reg},
	}}
	curEnd := entries[0].end

	for _, e := range entries[1:] {
		cur := &clusters[len(clusters)-1]
		gap := int(e.reg.Address) - curEnd
		words := max(curEnd, e.end) - int(cur.Start)
		// Merge only while the combined window stays within the FC03
		// read limit — otherwise one oversized register would drag its
		// healthy neighbours into an unencodable request.
		if gap <= gapThreshold && words >= 1 && words <= maxReadWords {
			cur.Members = append(cur.Members, e.reg)
			curEnd = int(cur.Start) + words
			cur.Count = uint16(words)
			continue
		}
		clusters = append(clusters, Cluster{
			Start:   e.reg.Address,
			Count:   e.length,
			Members: []*Register{e.reg},
		})
		curEnd = e.end
	}
	return clusters
}
