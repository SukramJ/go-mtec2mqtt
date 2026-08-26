// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import "time"

// MQTT keys for pseudo-register inputs and outputs. Hard-coded here
// because they are part of the coordinator's contract with downstream
// MQTT consumers — moving them to YAML would let an unrelated catalog
// edit silently break the calculations.
//
// Output keys also have to match the corresponding pseudo-register
// entries in registers.yaml so the HA discovery layer picks them up
// (they carry hass_value_template / hass_device_class metadata there).
const (
	// BASE inputs
	keyInverterAC = "inverter"
	keyGridPower  = "grid_power"

	// BASE outputs
	keyConsumption = "consumption"
	keyAPIDate     = "api_date"

	// DAY inputs
	keyPVDay               = "pv_day"
	keyGridPurchaseDay     = "grid_purchase_day"
	keyBatteryDischargeDay = "battery_discharge_day"
	keyGridFeedDay         = "grid_feed_day"
	keyBatteryChargeDay    = "battery_charge_day"

	// DAY outputs
	keyConsumptionDay    = "consumption_day"
	keyAutarkyRateDay    = "autarky_rate_day"
	keyOwnConsumptionDay = "own_consumption_day"

	// TOTAL inputs
	keyPVTotal               = "pv_total"
	keyGridPurchaseTotal     = "grid_purchase_total"
	keyBatteryDischargeTotal = "battery_discharge_total"
	keyGridFeedTotal         = "grid_feed_total"
	keyBatteryChargeTotal    = "battery_charge_total"

	// TOTAL outputs
	keyConsumptionTotal    = "consumption_total"
	keyAutarkyRateTotal    = "autarky_rate_total"
	keyOwnConsumptionTotal = "own_consumption_total"
)

// apiDateLayout matches the Python `strftime("%Y-%m-%d %H:%M:%S")`
// used for the api_date pseudo-register. Lowercase day-of-month +
// 24h clock; no timezone marker (publishes local wall-clock time).
const apiDateLayout = "2006-01-02 15:04:05"

// PseudoRegisters computes the calculated registers for a group and
// returns them as the same key→value map the coordinator publishes,
// plus the keys it had to skip because an input register was missing
// from this read cycle. Returns nil, nil when the group has no
// pseudo-registers — callers can safely range over the result either
// way.
//
// A pseudo-register is only emitted when every input of its formula is
// present. A partial group read (cluster timeout, reconnect window)
// used to feed the formulas a silent 0 for the absent registers, so
// e.g. a failed inverter cluster published consumption = 0 - (-500) =
// 500 W instead of 3500 W — a plausible-looking wrong number that lands
// in Home Assistant's long-term statistics. Skipping the value instead
// leaves the last good one in place.
//
// `now` is injected for tests; pass time.Now() in production.
func PseudoRegisters(group string, data map[string]any, now time.Time) (values map[string]any, skipped []string) {
	switch group {
	case "now-base":
		return basePseudos(data, now)
	case "day":
		return periodPseudos(
			data,
			keyPVDay, keyGridPurchaseDay, keyBatteryDischargeDay,
			keyGridFeedDay, keyBatteryChargeDay,
			keyConsumptionDay, keyAutarkyRateDay, keyOwnConsumptionDay,
		)
	case "total":
		return periodPseudos(
			data,
			keyPVTotal, keyGridPurchaseTotal, keyBatteryDischargeTotal,
			keyGridFeedTotal, keyBatteryChargeTotal,
			keyConsumptionTotal, keyAutarkyRateTotal, keyOwnConsumptionTotal,
		)
	}
	return nil, nil
}

// basePseudos computes the two BASE-group pseudo-registers:
// consumption (current household draw) and api_date (timestamp).
// api_date has no register inputs, so it is always emitted; consumption
// needs both the inverter AC power and the grid power.
func basePseudos(data map[string]any, now time.Time) (values map[string]any, skipped []string) {
	out := map[string]any{
		keyAPIDate: now.Format(apiDateLayout),
	}
	inverterAC, haveInverter := numeric(data, keyInverterAC)
	gridPower, haveGrid := numeric(data, keyGridPower)
	if !haveInverter || !haveGrid {
		return out, []string{keyConsumption}
	}
	consumption := inverterAC - gridPower
	if consumption < 0 {
		consumption = 0
	}
	out[keyConsumption] = consumption
	return out, nil
}

// periodPseudos computes the three period (day / total) energy
// pseudo-registers. The arithmetic is identical for both buckets;
// only the input/output key names differ — passing them in keeps the
// function single-purpose without duplicating the formulas.
//
// Formulas mirror the Python coordinator:
//
//	consumption = pv + grid_purchase + battery_discharge
//	              - grid_feed - battery_charge
//	autarky_rate = 100 * (1 - grid_purchase / consumption)   when consumption > 0
//	own_consumption = 100 * (1 - grid_feed / pv)             when pv > 0
//
// Each output is gated on the inputs its own formula reads:
// consumption and autarky_rate need all five energy counters,
// own_consumption only needs pv and grid_feed.
func periodPseudos(
	data map[string]any,
	pvKey, purchaseKey, dischargeKey, feedKey, chargeKey string,
	consumptionKey, autarkyKey, ownKey string,
) (values map[string]any, skipped []string) {
	pv, havePV := numeric(data, pvKey)
	purchase, havePurchase := numeric(data, purchaseKey)
	discharge, haveDischarge := numeric(data, dischargeKey)
	feed, haveFeed := numeric(data, feedKey)
	charge, haveCharge := numeric(data, chargeKey)

	out := make(map[string]any, 3)

	haveAll := havePV && havePurchase && haveDischarge && haveFeed && haveCharge
	if haveAll {
		consumption := pv + purchase + discharge - feed - charge
		if consumption < 0 {
			consumption = 0
		}
		var autarky float64
		if consumption > 0 {
			autarky = 100 * (1 - purchase/consumption)
			if autarky < 0 {
				autarky = 0
			}
		}
		out[consumptionKey] = consumption
		out[autarkyKey] = autarky
	} else {
		skipped = append(skipped, consumptionKey, autarkyKey)
	}

	if havePV && haveFeed {
		var own float64
		if pv > 0 {
			own = 100 * (1 - feed/pv)
			if own < 0 {
				own = 0
			}
		}
		out[ownKey] = own
	} else {
		skipped = append(skipped, ownKey)
	}

	return out, skipped
}

// numeric looks a register up and coerces it (int from an unscaled
// register, float64 from a scaled one) into a float64. The second
// return reports whether the key was present with a usable numeric
// type; callers must not substitute a 0 for a missing input — see
// [PseudoRegisters].
func numeric(data map[string]any, key string) (float64, bool) {
	v, ok := data[key]
	if !ok {
		return 0, false
	}
	return toFloat(v)
}
