# go-mtec2mqtt — Home Assistant Add-on

This add-on runs the [go-mtec2mqtt](https://github.com/SukramJ/go-mtec2mqtt)
daemon inside Home Assistant. It bridges an **M-TEC Energybutler** (hybrid
PV / battery inverter) to MQTT over **Modbus TCP**, with optional Home
Assistant MQTT discovery and a diagnostic web UI.

## Installation

1. In Home Assistant go to **Settings → Add-ons → Add-on Store**.
2. Click the **⋮** menu (top right) → **Repositories** and add:
   `https://github.com/SukramJ/go-mtec2mqtt`
3. The **go-mtec2mqtt** add-on now appears in the store. Open it and
   click **Install**.
4. Open the **Configuration** tab and set your inverter's `modbus_ip`
   (the address of the M-TEC "espressif" Modbus gateway). Leave
   `mqtt_server` **empty** to auto-use the Home Assistant MQTT broker; set
   it only to target a different broker.
5. **Start** the add-on.

The diagnostic dashboard is served through Home Assistant **Ingress** — open
it from the add-on's **Open Web UI** button or the sidebar panel; no port
needs to be exposed.

## Firmware note (Modbus port / framer)

- M-TEC firmware **V27.52.4.0 and newer** use Modbus port **502** with the
  `socket` framer — the add-on defaults.
- **Older firmware** listens on port **5743** and needs `modbus_framer: rtu`.
  Set `modbus_port: 5743` and `modbus_framer: rtu` in that case.

## Image build paths

There are two ways the add-on image can be produced. The add-on is configured
for the **preferred** path by default.

### Preferred: pre-built GHCR add-on image

`addon/config.yaml` sets:

```yaml
image: "ghcr.io/sukramj/go-mtec2mqtt-addon-{arch}"
```

When `image:` is present, the Supervisor substitutes `{arch}`
(`amd64`/`aarch64`/`armv7`) and **pulls** that image at the tag matching the
add-on `version:`. These per-arch images bundle the HA base, bashio and the
`run.sh` entrypoint, and are published by
`.github/workflows/addon-image.yml`. This path is fast and needs no toolchain
on the Home Assistant host.

> **Note:** this is **not** the distroless daemon image
> `ghcr.io/sukramj/go-mtec2mqtt` (built by `docker-build-push.yml` for the
> standalone Docker/systemd deployment). That image runs the binary directly
> and has no `run.sh`, so it cannot translate add-on options into `MTEC_*`
> env — it is not a valid add-on image.

### Fallback: local build from source

If you want the add-on built on the host instead, remove (or comment out) the
`image:` key in `addon/config.yaml`. The Supervisor will then build
`addon/Dockerfile` using the base images from `addon/build.yaml`.

> **Caveat:** the Go sources live at the **repository root**, not inside
> `addon/`. Home Assistant's local add-on builder normally uses the add-on
> directory as the Docker build context, which does **not** contain the Go
> sources. `addon/Dockerfile` is written for a **repository-root** build
> context and can be built manually:
>
> ```sh
> docker build \
>   --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:latest \
>   -f addon/Dockerfile -t go-mtec2mqtt-addon .
> ```
>
> For normal Home Assistant installs, prefer the GHCR image path above.

## Options

See [DOCS.md](DOCS.md) for a one-line reference of every option.
