// SPDX-License-Identifier: MIT
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
