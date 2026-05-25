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
// returns them as the same key→value map the coordinator publishes.
// Returns nil when the group has no pseudo-registers — callers can
// safely range over the result either way.
//
// `now` is injected for tests; pass time.Now() in production.
func PseudoRegisters(group string, data map[string]any, now time.Time) map[string]any {
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
	return nil
}

// basePseudos computes the two BASE-group pseudo-registers:
// consumption (current household draw) and api_date (timestamp).
func basePseudos(data map[string]any, now time.Time) map[string]any {
	inverterAC := numeric(data[keyInverterAC])
	gridPower := numeric(data[keyGridPower])
	consumption := inverterAC - gridPower
	if consumption < 0 {
		consumption = 0
	}
	return map[string]any{
		keyConsumption: consumption,
		keyAPIDate:     now.Format(apiDateLayout),
	}
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
func periodPseudos(
	data map[string]any,
	pvKey, purchaseKey, dischargeKey, feedKey, chargeKey string,
	consumptionKey, autarkyKey, ownKey string,
) map[string]any {
	pv := numeric(data[pvKey])
	purchase := numeric(data[purchaseKey])
	discharge := numeric(data[dischargeKey])
	feed := numeric(data[feedKey])
	charge := numeric(data[chargeKey])

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

	var own float64
	if pv > 0 {
		own = 100 * (1 - feed/pv)
		if own < 0 {
			own = 0
		}
	}

	return map[string]any{
		consumptionKey: consumption,
		autarkyKey:     autarky,
		ownKey:         own,
	}
}

// numeric coerces a register value (which may be int from an unscaled
// register or float64 from a scaled one) into a float64. Missing keys
// fall back to zero so missing data does not crash the math —
// upstream Python uses `dict.get(key, 0)` the same way.
func numeric(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}
