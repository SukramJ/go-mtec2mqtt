// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SukramJ/go-mtec2mqtt/internal/hass"
)

// TestReconnectPublishesTheCurrentReachability: a broker reconnect must
// not restore the inverter's reachability from before the outage.
//
// The state plane replays what the broker last ACCEPTED, and a write
// refused during the outage is never recorded. Up to 2.0.0 an inverter
// that dropped (or came back) while the broker was away was therefore
// replayed `online` true (false), and `<name>/connected` announced from the
// last observed level, until the next group read or watchdog tick — every
// entity shown available against a dead link, or unavailable against a
// live one.
func TestReconnectPublishesTheCurrentReachability(t *testing.T) {
	errBroker := errors.New("broker gone")
	online := hass.OnlineTopic("MTEC", goldenSerial)
	connected := hass.ConnectedTopic("MTEC")

	for _, tc := range []struct {
		name         string
		before, then bool
		wantLevel    string
	}{
		{name: "the inverter drops during the outage", before: true, then: false, wantLevel: "1"},
		{name: "the inverter returns during the outage", before: false, then: true, wantLevel: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, _, stub, link := contractCoordinator(t, "en")
			link.connected.Store(tc.before)
			c.PublishOnline(ctx)
			c.updateUpstream(ctx) // the first group read
			if got, ok := lastOnline(stub, online); !ok || got != tc.before {
				t.Fatalf("online = %v (%v) before the outage, want %v", got, ok, tc.before)
			}

			// The broker is unreachable; the watchdog sees the link change
			// and its writes are refused.
			stub.setPublishErr(errBroker)
			link.connected.Store(tc.then)
			c.updateUpstream(ctx)
			stub.setPublishErr(nil)

			mark := len(stub.snapshotPublishes())
			c.PublishOnline(ctx)
			after := stub.snapshotPublishes()[mark:]
			sawOnline := false
			for _, rec := range after {
				switch rec.topic {
				case online:
					sawOnline = true
					var item struct {
						Val bool `json:"val"`
					}
					if err := json.Unmarshal(rec.payload, &item); err != nil || item.Val != tc.then {
						t.Errorf("reconnect wrote online %s, want val %v", rec.payload, tc.then)
					}
				case connected:
					if string(rec.payload) != tc.wantLevel {
						t.Errorf("reconnect wrote connected %q, want %q", rec.payload, tc.wantLevel)
					}
				}
			}
			if !sawOnline {
				t.Error("the reconnect wrote no online item")
			}
		})
	}
}

func lastOnline(stub *stubMQTT, topic string) (val, ok bool) {
	for _, rec := range stub.snapshotPublishes() {
		if rec.topic != topic {
			continue
		}
		var item struct {
			Val bool `json:"val"`
		}
		ok = json.Unmarshal(rec.payload, &item) == nil
		val = item.Val
	}
	return val, ok
}
