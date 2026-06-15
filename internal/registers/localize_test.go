// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package registers

import "testing"

func TestLocalizedNameFallback(t *testing.T) {
	r := &Register{Name: "Charge limit", NameDE: "Ladestrombegrenzung"}
	if got := r.LocalizedName("de"); got != "Ladestrombegrenzung" {
		t.Errorf("de = %q", got)
	}
	if got := r.LocalizedName("en"); got != "Charge limit" {
		t.Errorf("en = %q", got)
	}
	if got := r.LocalizedName("fr"); got != "Charge limit" {
		t.Errorf("unknown lang must fall back to en, got %q", got)
	}
	// Missing German label falls back to the English name.
	bare := &Register{Name: "Grid power"}
	if got := bare.LocalizedName("de"); got != "Grid power" {
		t.Errorf("missing de must fall back, got %q", got)
	}
}

func TestLocalizedValueItemsPerCodeFallback(t *testing.T) {
	r := &Register{
		HassValueItems:   map[int]string{0: "General", 1: "Eco"},
		HassValueItemsDE: map[int]string{0: "Allgemein"}, // 1 missing
	}
	de := r.LocalizedValueItems("de")
	if de[0] != "Allgemein" {
		t.Errorf("de[0] = %q, want Allgemein", de[0])
	}
	if de[1] != "Eco" {
		t.Errorf("de[1] = %q, want Eco (per-code fallback)", de[1])
	}
	// The English map must be returned untouched for en.
	en := r.LocalizedValueItems("en")
	if en[0] != "General" {
		t.Errorf("en[0] = %q, want General", en[0])
	}
	// No value items → nil.
	if (&Register{}).LocalizedValueItems("de") != nil {
		t.Error("empty value items must yield nil")
	}
}
