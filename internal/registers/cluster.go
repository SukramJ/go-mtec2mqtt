// SPDX-License-Identifier: MIT
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
		end    uint16 // address + length, used to extend clusters
		length uint16
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
		length := uint16(r.Length)
		if length == 0 {
			length = 1
		}
		entries = append(entries, entry{
			reg:    r,
			length: length,
			end:    r.Address + length,
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
		gap := int(e.reg.Address) - int(curEnd)
		if gap <= gapThreshold {
			cur.Members = append(cur.Members, e.reg)
			if e.end > curEnd {
				curEnd = e.end
			}
			cur.Count = curEnd - cur.Start
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
