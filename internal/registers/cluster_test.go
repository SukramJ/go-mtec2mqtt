// SPDX-License-Identifier: LGPL-3.0-or-later
// Copyright (C) 2026 SukramJ

package registers

import (
	"testing"
)

func mkReg(addr uint16, length int) *Register {
	return &Register{
		Key:     itoa(int(addr)),
		Address: addr,
		Length:  length,
		Type:    DataU16,
		Scale:   1,
		Name:    itoa(int(addr)),
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

func TestClusterizeSingleContiguous(t *testing.T) {
	// All within the gap threshold — single Modbus read.
	regs := []*Register{
		mkReg(11000, 2), mkReg(11006, 1), mkReg(11009, 1), mkReg(11010, 1),
	}
	c := Clusterize(regs)
	if len(c) != 1 {
		t.Fatalf("want 1 cluster, got %d (%+v)", len(c), c)
	}
	if c[0].Start != 11000 || c[0].Count != 11 {
		t.Fatalf("cluster span: start=%d count=%d (want 11000/11)", c[0].Start, c[0].Count)
	}
}

func TestClusterizeSplitsLargeGap(t *testing.T) {
	// Gap of 14 (11003 to 11017) exceeds threshold → two clusters.
	regs := []*Register{mkReg(11000, 2), mkReg(11017, 1)}
	c := Clusterize(regs)
	if len(c) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(c))
	}
	if c[0].Start != 11000 || c[0].Count != 2 {
		t.Fatalf("cluster 0: %+v", c[0])
	}
	if c[1].Start != 11017 || c[1].Count != 1 {
		t.Fatalf("cluster 1: %+v", c[1])
	}
}

func TestClusterizeGapExactlyAtThresholdMerges(t *testing.T) {
	// curEnd = 11001 (addr 11000 + length 1). Next address 11011 →
	// gap = 11011 - 11001 = 10 → boundary, must merge.
	c := Clusterize([]*Register{mkReg(11000, 1), mkReg(11011, 1)})
	if len(c) != 1 || c[0].Start != 11000 || c[0].Count != 12 {
		t.Fatalf("boundary gap should merge: %+v", c)
	}
}

func TestClusterizeGapOneAboveThresholdSplits(t *testing.T) {
	// Gap 11 → split.
	c := Clusterize([]*Register{mkReg(11000, 1), mkReg(11012, 1)})
	if len(c) != 2 {
		t.Fatalf("gap 11 should split: %+v", c)
	}
}

func TestClusterizeOutOfOrderInput(t *testing.T) {
	// Sorts by address before clustering.
	c := Clusterize([]*Register{
		mkReg(11010, 1), mkReg(11000, 2), mkReg(11006, 1),
	})
	if len(c) != 1 || c[0].Start != 11000 {
		t.Fatalf("expected sort+merge, got %+v", c)
	}
	// Members must be sorted too.
	wantAddrs := []uint16{11000, 11006, 11010}
	for i, m := range c[0].Members {
		if m.Address != wantAddrs[i] {
			t.Errorf("Members[%d]=%d, want %d", i, m.Address, wantAddrs[i])
		}
	}
}

func TestClusterizeIgnoresPseudoRegisters(t *testing.T) {
	pseudo := &Register{Key: "consumption", Name: "Household"}
	c := Clusterize([]*Register{pseudo, mkReg(11000, 1)})
	if len(c) != 1 || len(c[0].Members) != 1 {
		t.Fatalf("pseudo registers must be ignored: %+v", c)
	}
}

func TestClusterizeDeduplicatesAddresses(t *testing.T) {
	c := Clusterize([]*Register{mkReg(11000, 1), mkReg(11000, 1)})
	if len(c) != 1 || len(c[0].Members) != 1 {
		t.Fatalf("duplicate addresses should be folded: %+v", c)
	}
}

func TestClusterizeOverlappingLengthsExtendCount(t *testing.T) {
	// First register has length 4 → end 11004. Second register at
	// 11003 lives inside the first; cluster Count must not regress.
	c := Clusterize([]*Register{mkReg(11000, 4), mkReg(11003, 1)})
	if len(c) != 1 {
		t.Fatalf("want single cluster, got %d", len(c))
	}
	if c[0].Count != 4 {
		t.Fatalf("overlapping reg shrank cluster: count=%d", c[0].Count)
	}
}

func TestClusterizeEmpty(t *testing.T) {
	if c := Clusterize(nil); c != nil {
		t.Fatalf("nil input should yield nil, got %v", c)
	}
}

func TestClusterizeCapsMergedWindowAtReadLimit(t *testing.T) {
	// The gap (5) is within the threshold, but merging would produce a
	// 205-word window — beyond the FC03 limit. The second register must
	// start its own cluster so one oversized entry cannot make the
	// encoder reject its healthy neighbour too.
	c := Clusterize([]*Register{mkReg(100, 100), mkReg(205, 100)})
	if len(c) != 2 {
		t.Fatalf("oversized merge should split, got %+v", c)
	}
	if c[0].Start != 100 || c[0].Count != 100 {
		t.Fatalf("cluster 0: %+v", c[0])
	}
	if c[1].Start != 205 || c[1].Count != 100 {
		t.Fatalf("cluster 1: %+v", c[1])
	}
}

func TestClusterizeSplitsDenseRunPastReadLimit(t *testing.T) {
	// 200 gap-free one-word registers: unmergeable into a single FC03
	// request. The clusterer must degrade to several valid reads that
	// still cover every member, instead of one 200-word window the
	// encoder rejects — which would take all 200 registers offline.
	var regs []*Register
	for addr := uint16(10000); addr < 10200; addr++ {
		regs = append(regs, mkReg(addr, 1))
	}
	c := Clusterize(regs)
	if len(c) < 2 {
		t.Fatalf("200-word run must split into >=2 clusters, got %d", len(c))
	}
	members := 0
	for i, cl := range c {
		if cl.Count == 0 || cl.Count > 125 {
			t.Fatalf("cluster %d Count=%d outside FC03 range 1..125", i, cl.Count)
		}
		for _, m := range cl.Members {
			if m.Address < cl.Start || int(m.Address)+m.Length > int(cl.Start)+int(cl.Count) {
				t.Errorf("cluster %d (start=%d count=%d) does not cover member %d",
					i, cl.Start, cl.Count, m.Address)
			}
		}
		members += len(cl.Members)
	}
	if members != len(regs) {
		t.Fatalf("splitting lost registers: %d members, want %d", members, len(regs))
	}
}

func TestClusterizeNoUint16Wraparound(t *testing.T) {
	// Address + length overflows uint16 (65000 + 65535 = 130535). The
	// old uint16 arithmetic wrapped curEnd below the cluster start and
	// underflowed Count for the neighbour. The window math now runs in
	// int: the oversized register keeps its own (unencodable) cluster
	// and the neighbour gets a sane one-word read.
	c := Clusterize([]*Register{mkReg(65000, 65535), mkReg(65100, 1)})
	if len(c) != 2 {
		t.Fatalf("want 2 clusters, got %+v", c)
	}
	if c[0].Start != 65000 || c[0].Count != 65535 {
		t.Fatalf("cluster 0: %+v", c[0])
	}
	if c[1].Start != 65100 || c[1].Count != 1 {
		t.Fatalf("cluster 1 corrupted by wraparound: start=%d count=%d",
			c[1].Start, c[1].Count)
	}
}

func TestClusterizeMaxCountWithinSpec(t *testing.T) {
	// Build a dense block that just fits inside one read.
	var regs []*Register
	for addr := uint16(10000); addr < 10120; addr++ {
		regs = append(regs, mkReg(addr, 1))
	}
	c := Clusterize(regs)
	if len(c) != 1 {
		t.Fatalf("dense run should be a single cluster, got %d", len(c))
	}
	if c[0].Count > 125 {
		t.Fatalf("cluster Count=%d exceeds Modbus FC03 max (125)", c[0].Count)
	}
}
