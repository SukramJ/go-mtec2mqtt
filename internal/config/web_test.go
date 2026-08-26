// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"strings"
	"testing"
)

// webYAML is minimumYAML plus a WEB_ENABLE block, parameterised by the
// caller-supplied extra lines.
func webYAML(extra string) string {
	return minimumYAML + "WEB_ENABLE: true\n" + extra
}

func TestWebDefaultsBindLocalhost(t *testing.T) {
	c, err := Load(strings.NewReader(minimumYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.WebBind != DefaultWebBind {
		t.Errorf("WEB_BIND default = %q, want %q", c.WebBind, DefaultWebBind)
	}
	if c.WebEnable {
		t.Error("WEB_ENABLE should default to false")
	}
}

func TestWebEnabledValidBind(t *testing.T) {
	c, err := Load(strings.NewReader(webYAML("WEB_BIND: 0.0.0.0:9099\n")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.WebEnable || c.WebBind != "0.0.0.0:9099" {
		t.Errorf("unexpected web config: enable=%v bind=%q", c.WebEnable, c.WebBind)
	}
}

func TestWebBadBindRejected(t *testing.T) {
	_, err := Load(strings.NewReader(webYAML("WEB_BIND: not-a-hostport\n")), nil)
	if err == nil || !strings.Contains(err.Error(), "WEB_BIND") {
		t.Fatalf("expected WEB_BIND error, got %v", err)
	}
}

func TestWebBadPortRejected(t *testing.T) {
	_, err := Load(strings.NewReader(webYAML("WEB_BIND: 127.0.0.1:99999\n")), nil)
	if err == nil || !strings.Contains(err.Error(), "WEB_BIND port") {
		t.Fatalf("expected WEB_BIND port error, got %v", err)
	}
}

func TestWebAuthMustBePaired(t *testing.T) {
	_, err := Load(strings.NewReader(webYAML("WEB_USER: admin\n")), nil)
	if err == nil || !strings.Contains(err.Error(), "WEB_USER and WEB_PASSWORD") {
		t.Fatalf("expected paired-auth error, got %v", err)
	}
}

func TestWebAuthPairedOK(t *testing.T) {
	c, err := Load(strings.NewReader(webYAML("WEB_USER: admin\nWEB_PASSWORD: secret\n")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.WebUser != "admin" || c.WebPassword != "secret" {
		t.Errorf("auth not parsed: user=%q", c.WebUser)
	}
}

func TestWebDisabledSkipsValidation(t *testing.T) {
	// A bogus bind must not block startup when the UI is off.
	c, err := Load(strings.NewReader(minimumYAML+"WEB_ENABLE: false\nWEB_BIND: garbage\n"), nil)
	if err != nil {
		t.Fatalf("disabled web should skip bind validation, got %v", err)
	}
	if c.WebEnable {
		t.Error("WEB_ENABLE should be false")
	}
}
