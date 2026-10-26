# evcc

go-mtec2mqtt exposes the **shared evcc profile** — one MQTT surface that
[go-keba2mqtt](https://github.com/SukramJ/go-keba2mqtt), go-mtec2mqtt and
[go-zendure2mqtt](https://github.com/SukramJ/go-zendure2mqtt) implement
identically, so an `evcc.yaml` reads the same for every device. evcc reads
the Energybutler as three `custom` meters — grid, PV and battery — and
controls the battery over MQTT, while the bridge stays the inverter's only
Modbus client.

Enable it with `EVCC_ENABLE: true` (env `MTEC_EVCC_ENABLE`, add-on option
`evcc_enable`).

## Topics

```
<name>/status/<serial>/evcc/<role>/<key>    {"val": …, "ts": …, "lc": …}   retained
<name>/set/<serial>/evcc/<role>/<key>       plain value (or {"val": …})
```

`<name>` is `MQTT_TOPIC` (`mtec`), `<serial>` the inverter serial number (in
the log at start-up, `coordinator.static_initialised serial=…`). evcc reads
every status value with `jq: .val`.

| Status key | Unit | Source | Meaning |
| --- | --- | --- | --- |
| `grid/power` | W | `grid_power`, negated | positive while importing |
| `grid/power_l1..l3` | W | `grid_a/b/c`, negated | per phase, same sign |
| `grid/energy` | kWh | `grid_purchase_total` | imported |
| `grid/returnenergy` | kWh | `grid_feed_total` | exported |
| `pv/power` | W | `pv` | production |
| `pv/energy` | kWh | `pv_total` | produced |
| `battery/power` | W | `battery` | positive while discharging |
| `battery/soc` | % | `battery_soc` | state of charge |
| `battery/energy` | kWh | `battery_discharge_total` | discharged |
| `battery/returnenergy` | kWh | `battery_charge_total` | charged |
| `battery/batterymode` | — | — | `normal`, `hold` or `charge`, as last commanded |

Each value is published with the read of its source register: power and
state of charge every `REFRESH_NOW` (10 s), the phase powers in the
secondary rotation, the energy counters every `REFRESH_TOTAL` (300 s). Like
every status item, a value is written only when it changes.

| Command key | Payload | Effect |
| --- | --- | --- |
| `battery/batterymode` | `1`/`normal`, `2`/`hold`, `3`/`charge` | `hold` switches the inverter to *Economic mode* (no discharge), `charge` to *UPS mode* (charges from the grid); `normal` restores the operation mode the inverter had before |

The mapping is the one evcc's own `solinteg-gen3` template (which covers the
Energybutler GEN3) uses. The bridge reads the operation mode before every
change, writes only when it differs, and ignores a re-sent unchanged mode.
The mode to restore is kept in memory: if the bridge restarts while the
battery is held or charging, `normal` has nothing to restore and leaves the
operation mode as it is — set it back in Home Assistant (`mode`) then.

`EVCC_BATTERYMODE_TIMEOUT` (seconds, default `0` = off; add-on option
`evcc_batterymode_timeout`) returns the battery to normal when `hold` or
`charge` has not been re-sent for that long, so an evcc that stopped never
leaves the battery locked. evcc sends a battery mode only when it changes;
the `watchdog` in the snippet below repeats it every half `timeout`. Set
`EVCC_BATTERYMODE_TIMEOUT` to at least twice the watchdog `timeout`, e.g.
`120`.

## evcc.yaml

Replace `MTEC0123456789` with your inverter serial and `mtec` with your
`MQTT_TOPIC` if you changed it.

```yaml
mqtt:
  broker: localhost:1883
  topic: evcc

meters:
  - name: grid
    type: custom
    power:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/grid/power
      jq: .val
    energy:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/grid/energy
      jq: .val
    returnenergy:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/grid/returnenergy
      jq: .val
    powers:
      - source: mqtt
        topic: mtec/status/MTEC0123456789/evcc/grid/power_l1
        jq: .val
      - source: mqtt
        topic: mtec/status/MTEC0123456789/evcc/grid/power_l2
        jq: .val
      - source: mqtt
        topic: mtec/status/MTEC0123456789/evcc/grid/power_l3
        jq: .val
  - name: pv
    type: custom
    power:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/pv/power
      jq: .val
    energy:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/pv/energy
      jq: .val
  - name: battery
    type: custom
    power:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/battery/power
      jq: .val
    soc:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/battery/soc
      jq: .val
    energy:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/battery/energy
      jq: .val
    returnenergy:
      source: mqtt
      topic: mtec/status/MTEC0123456789/evcc/battery/returnenergy
      jq: .val
    batterymode:
      source: watchdog
      timeout: 60s
      reset: ["1"]
      set:
        source: mqtt
        topic: mtec/set/MTEC0123456789/evcc/battery/batterymode

site:
  title: Home
  meters:
    grid: grid
    pv:
      - pv
    battery:
      - battery
```

Leave out `batterymode` if evcc should only read the battery.

## Freshness and failure

- The snippet sets no `timeout` on the meters: the bridge publishes a value
  only when it changes, so a steady value (a battery at 100 %, no PV at
  night) would look outdated to evcc. Availability is `mtec/connected`
  instead: `2` while the bridge reaches the inverter, `1` when it does not,
  `0` when the bridge is down.
- If evcc stops while the battery is held or charging, the watchdog stops
  with it and `EVCC_BATTERYMODE_TIMEOUT` returns the battery to normal.

## The shared profile

The same contract holds for all three bridges (go-keba2mqtt,
go-zendure2mqtt#66): the `evcc` item group, evcc's units and signs (power
positive = import / production / discharge / consumption), `EVCC_ENABLE`
off by default, idempotent commands, and a `docs/evcc.md` whose snippet is
checked against the topics the bridge really publishes
(`internal/evcc/docs_test.go`). Devices with several roles nest them one
level deeper (`evcc/grid/power`, `evcc/battery/soc`); a wallbox has one role
and publishes directly under `evcc/`.
