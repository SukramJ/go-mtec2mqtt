// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import "testing"

func TestEVCCTopics(t *testing.T) {
	t.Parallel()
	if got := EVCCStateTopic("mtec", "SN1", "battery", "soc"); got != "mtec/status/SN1/evcc/battery/soc" {
		t.Errorf("state %q", got)
	}
	if got := EVCCCommandTopic("mtec", "SN1", "battery", "batterymode"); got != "mtec/set/SN1/evcc/battery/batterymode" {
		t.Errorf("command %q", got)
	}
	if got := EVCCCommandFilter("mtec"); got != "mtec/set/+/evcc/+/+" {
		t.Errorf("filter %q", got)
	}
}
