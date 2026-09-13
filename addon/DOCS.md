# go-mtec2mqtt add-on

## Quickstart

For a standard Home Assistant install with the Mosquitto broker, only one
option is required:

1. Set **`modbus_ip`** to your M-TEC inverter's Modbus gateway address (the
   "espressif" module on your LAN).
2. Leave **`mqtt_server` empty** — the add-on auto-connects to the Home
   Assistant MQTT broker (like zigbee2mqtt), and `hass_enable` is on by
   default, so entities appear automatically via MQTT discovery.
3. **Start** the add-on and open its **Web UI** to watch live register values.

Everything else has sensible defaults; use the reference below to fine-tune.

## Options reference

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `modbus_ip` | str | `0.0.0.0` | IP / hostname of the M-TEC "espressif" Modbus TCP gateway. **Required** — set this to your inverter's address. The add-on still starts with the placeholder (the web UI stays reachable) but shows the inverter as disconnected until a real address is entered. |
| `modbus_port` | int | `502` | Modbus TCP port. `502` for firmware ≥ V27.52.4.0; older firmware needs `5743` (and `modbus_framer: rtu`). |
| `modbus_slave` | int | `247` | Modbus slave / unit id (usually no change required). |
| `modbus_timeout` | int | `5` | Modbus response timeout in seconds. |
| `modbus_retries` | int | `3` | Modbus retry count per transaction. |
| `modbus_framer` | list(ascii\|rtu\|socket\|tls) | `socket` | Modbus framer. `socket` for firmware ≥ V27.52.4.0; `rtu` for older firmware on port 5743. |
| `mqtt_server` | str | `""` | MQTT broker hostname. **Leave empty** to auto-use the Home Assistant MQTT broker (the configured MQTT integration / `core-mosquitto`). Set a value only to point at a different broker. |
| `mqtt_port` | int | `1883` | MQTT broker port. Only used when `mqtt_server` is set; the auto-detected broker brings its own port. |
| `mqtt_login` | str | `""` | MQTT username. Only used when `mqtt_server` is set (auto-detect supplies credentials). |
| `mqtt_password` | password | `""` | MQTT password. Only used when `mqtt_server` is set. |
| `mqtt_topic` | str | `MTEC` | Base MQTT topic for published register state. |
| `hass_enable` | bool | `true` | Publish Home Assistant MQTT discovery so entities appear automatically. On by default — leave enabled for the normal HA experience; disable only to manage entities manually. |
| `device_name` | str | `""` | Optional friendly name for this inverter. When set, it becomes the Home Assistant device name (instead of the generic "MTEC EnergyButler") and is slugged into every entity's entity-id seed, so HA seeds fresh entity ids like `sensor.<device_name>_grid_power` — handy to tell multiple inverters apart. The seed is the slugified English register name (matching the Python `aiomtec2mqtt` entity ids), never the localised display name, so entity ids stay language-independent; only the display name follows `language`. The entity `unique_id` is left unchanged, so enabling this on an existing install does not orphan established entities; only newly created ones pick up the nicer id. Leave empty to keep the previous behaviour. MQTT topics also stay keyed on the inverter serial. |
| `language` | list(en\|de) | `en` | UI / entity naming language. Entity ids stay language-independent, so switching never re-creates entities. |
| `web_enable` | bool | `true` | Enable the diagnostic web UI (required for the Ingress panel). |
| `charge_active_value` | int | `50` | Amperage written to the charge-limit register when the "Charge active" switch is first turned on (native register unit). |
| `discharge_active_value` | int | `50` | Amperage written to the discharge-limit register when the "Discharge active" switch is first turned on. |
| `debug` | bool | `false` | Verbose debug logging. |

Fixed by the add-on (not user-configurable): the web UI binds to `0.0.0.0:8080`
for Ingress, and `hass_base_topic` stays at Home Assistant's default
(`homeassistant`). The refresh intervals and MQTT float format use the daemon
defaults; run the standalone binary / Docker image if you need to tune those.

## Home Assistant discovery

Discovery is published as **one retained device document** at
`homeassistant/device/<node-id>/config`. **Home Assistant 2024.11 or newer is
required.**

`<node-id>` is the inverter serial slugged: lower-cased, with every character
outside `a-z`, `0-9` and `-` folded to `_`. For an ordinary serial such as
`MT1234567890` that is `mt1234567890`. The add-on pins the discovery prefix to
Home Assistant's default (`homeassistant`), so only the node id varies — and
the add-on log prints the exact topic at start-up, as
`coordinator.discovery_bundle_built topic=…`. Take it from there rather than
composing it.

Upgrading from 1.9.x or earlier moves the fleet off the previous
one-message-per-entity form (`homeassistant/<platform>/MTEC_<key>/config`).
The first start retracts those 100 retained configs and then publishes the
document, in that order — Home Assistant refuses either form while the
other is still retained, silently, with one `WARNING [mqtt.entity] Received
a conflicting MQTT discovery message` in its log.

Nothing is re-keyed: every `unique_id` is unchanged, so entity names,
areas, icons, history and automations survive untouched.

If the add-on is stopped between the two steps the entities are briefly
*absent*; starting it again repairs that by itself. Rolling back to an
older add-on version needs the document cleared first, or the old version's
per-entity configs are refused in the same silence:

```bash
mosquitto_pub -h core-mosquitto -u <user> -P <password> \
  -t homeassistant/device/<node-id>/config -r -n
```

The Supervisor's Mosquitto broker is authenticated, so `-h` and `-u`/`-P` are
not optional here — use the same credentials the add-on is configured with (or
the Mosquitto add-on's own user). Copy the topic from the start-up log line
above.

Running **two add-on instances against two inverters**: upgrade both together.
Before 1.10.0 the first one upgraded could retract the other's entire set of
per-entity configs; 1.10.0 scopes that judgement to each instance's own
inverter serial.

## TLS / secure MQTT

The **daemon** does speak TLS: `MQTT_SSL: true` dials `tls://` (port 8883 by
default) with full certificate verification, and `MQTT_SSL_INSECURE` disables
that verification for a self-signed broker. The protocol is MQTT 5.0 by
default.

The **add-on** does not expose either setting — it is wired to the
Supervisor's MQTT service on the internal network, where the traffic does not
leave the host. If you need TLS, run the standalone binary or the Docker image
and set `MTEC_MQTT_SSL=true`. (A previous revision of this page claimed the
daemon had no native TLS at all. It does; only the add-on's option list
omits it.)
