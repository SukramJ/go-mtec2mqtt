// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package evcc

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
)

// TestDocsSnippetMatchesTheProfile is the contract the shared profile
// asks for: every topic the evcc.yaml in docs/evcc.md names is one this
// bridge publishes (status) or routes (set), every getter reads `.val`,
// every setter writes a command topic, and every command is documented
// and every status item used.
func TestDocsSnippetMatchesTheProfile(t *testing.T) {
	t.Parallel()
	md, err := os.ReadFile("../../docs/evcc.md")
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may carry CRLF line endings.
	md = bytes.ReplaceAll(md, []byte("\r\n"), []byte("\n"))
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllSubmatch(md, -1)
	if len(blocks) != 1 {
		t.Fatalf("docs/evcc.md has %d yaml blocks, want 1", len(blocks))
	}
	var doc struct {
		Meters []map[string]any `yaml:"meters"`
	}
	if err := yaml.Unmarshal(blocks[0][1], &doc); err != nil {
		t.Fatalf("snippet is not YAML: %v", err)
	}
	if len(doc.Meters) != 3 {
		t.Fatalf("want grid, pv and battery meters, got %d", len(doc.Meters))
	}

	const serial = "MTEC0123456789"
	status := map[string]Item{}
	for _, it := range StatusItems() {
		status[hass.EVCCStateTopic("mtec", serial, it.Role, it.Key)] = it
	}
	set := map[string]Item{}
	for _, it := range Commands() {
		set[hass.EVCCCommandTopic("mtec", serial, it.Role, it.Key)] = it
	}

	var read, written []string
	var walk func(field string, v any)
	walk = func(field string, v any) {
		switch src := v.(type) {
		case []any:
			for _, e := range src {
				walk(field, e)
			}
		case map[string]any:
			if src["source"] != "mqtt" {
				for k, e := range src {
					walk(k, e)
				}
				return
			}
			topic, _ := src["topic"].(string)
			if it, ok := status[topic]; ok {
				if src["jq"] != ".val" {
					t.Errorf("%s reads %s without jq: .val", field, topic)
				}
				read = append(read, it.String())
				return
			}
			if it, ok := set[topic]; ok {
				if _, ok := src["jq"]; ok {
					t.Errorf("%s writes %s with a jq filter", field, topic)
				}
				written = append(written, it.String())
				return
			}
			t.Errorf("%s: %q is neither a published status nor a routed command topic", field, topic)
		}
	}
	for _, m := range doc.Meters {
		if m["type"] != "custom" {
			t.Errorf("meter %v is not type custom", m["name"])
		}
		for field, v := range m {
			walk(field, v)
		}
	}

	wantRead := make([]string, 0, len(StatusItems()))
	wantWritten := make([]string, 0, len(Commands()))
	for _, it := range StatusItems() {
		if it != BatteryMode { // a command's status, not an evcc getter
			wantRead = append(wantRead, it.String())
		}
	}
	for _, it := range Commands() {
		wantWritten = append(wantWritten, it.String())
	}
	for _, s := range [][]string{read, written, wantRead, wantWritten} {
		slices.Sort(s)
	}
	if !slices.Equal(read, wantRead) {
		t.Errorf("snippet reads %v, want every meter value %v", read, wantRead)
	}
	if !slices.Equal(written, wantWritten) {
		t.Errorf("snippet writes %v, want every command %v", written, wantWritten)
	}
}
