// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"strings"
	"testing"
)

func TestLanguageDefaultsEnglish(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Language != "en" || DefaultLanguage != "en" {
		t.Errorf("LANGUAGE default = %q, want %q", c.Language, "en")
	}
}

func TestLanguageGermanAccepted(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAML+"LANGUAGE: de\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Language != "de" {
		t.Errorf("LANGUAGE = %q, want de", c.Language)
	}
}

func TestLanguageUnknownRejected(t *testing.T) {
	_, err := Load(strings.NewReader(minimumYAML+"LANGUAGE: fr\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "LANGUAGE") {
		t.Fatalf("expected LANGUAGE error, got %v", err)
	}
}

func TestActiveValuesDefaultTo50(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ChargeActiveValue != 50 || c.DischargeActiveValue != 50 {
		t.Errorf("active defaults = %d/%d, want 50/50",
			c.ChargeActiveValue, c.DischargeActiveValue)
	}
}

func TestActiveValuesCustom(t *testing.T) {
	c, err := Load(strings.NewReader(
		minimumYAML+"CHARGE_ACTIVE_VALUE: 30\nDISCHARGE_ACTIVE_VALUE: 25\n",
	), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ChargeActiveValue != 30 || c.DischargeActiveValue != 25 {
		t.Errorf("active values = %d/%d, want 30/25",
			c.ChargeActiveValue, c.DischargeActiveValue)
	}
}

func TestActiveValueOutOfRangeRejected(t *testing.T) {
	_, err := Load(strings.NewReader(minimumYAML+"CHARGE_ACTIVE_VALUE: 9000\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "CHARGE_ACTIVE_VALUE") {
		t.Fatalf("expected CHARGE_ACTIVE_VALUE range error, got %v", err)
	}
}
