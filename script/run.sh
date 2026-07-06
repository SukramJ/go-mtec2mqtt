#!/usr/bin/with-contenv bashio
# SPDX-License-Identifier: LGPL-3.0-or-later
# Home Assistant add-on entrypoint for go-mtec2mqtt.
#
# Reads the user's add-on options (/data/options.json) via bashio, maps them
# onto the daemon's MTEC_* environment variables, wires up Ingress, and
# finally exec's the binary so it becomes PID 1 and receives signals directly.
# The daemon ships no config file in the add-on image — every setting is
# supplied through MTEC_* env here (main.go falls back to env-only when no
# config.yaml is found).
set -e

bashio::log.info "Starting go-mtec2mqtt add-on..."

# --- Modbus (M-TEC inverter / espressif gateway) ---
export MTEC_MODBUS_IP="$(bashio::config 'modbus_ip')"
export MTEC_MODBUS_PORT="$(bashio::config 'modbus_port')"
export MTEC_MODBUS_SLAVE="$(bashio::config 'modbus_slave')"
export MTEC_MODBUS_TIMEOUT="$(bashio::config 'modbus_timeout')"
export MTEC_MODBUS_RETRIES="$(bashio::config 'modbus_retries')"
export MTEC_MODBUS_FRAMER="$(bashio::config 'modbus_framer')"

# --- MQTT ---
# Zero-config: when mqtt_server is left empty, borrow the broker the
# Supervisor already knows about (the HA MQTT integration / core-mosquitto
# add-on) via the mqtt service. An explicit mqtt_server always wins; if
# nothing is set and no service is offered, fall back to core-mosquitto:1883.
if bashio::config.has_value 'mqtt_server'; then
  export MTEC_MQTT_SERVER="$(bashio::config 'mqtt_server')"
  export MTEC_MQTT_PORT="$(bashio::config 'mqtt_port')"
  export MTEC_MQTT_LOGIN="$(bashio::config 'mqtt_login')"
  export MTEC_MQTT_PASSWORD="$(bashio::config 'mqtt_password')"
elif bashio::services.available 'mqtt'; then
  bashio::log.info "mqtt_server empty; using the Home Assistant MQTT service."
  export MTEC_MQTT_SERVER="$(bashio::services 'mqtt' 'host')"
  export MTEC_MQTT_PORT="$(bashio::services 'mqtt' 'port')"
  export MTEC_MQTT_LOGIN="$(bashio::services 'mqtt' 'username')"
  export MTEC_MQTT_PASSWORD="$(bashio::services 'mqtt' 'password')"
else
  bashio::log.warning "mqtt_server empty and no MQTT service offered; falling back to core-mosquitto:1883."
  export MTEC_MQTT_SERVER="core-mosquitto"
  export MTEC_MQTT_PORT="1883"
fi
export MTEC_MQTT_TOPIC="$(bashio::config 'mqtt_topic')"

# --- Home Assistant discovery ---
export MTEC_HASS_ENABLE="$(bashio::config 'hass_enable')"

# Optional device name — only export when set so an empty field keeps the
# generic device identity instead of overriding it with a blank name.
if bashio::config.has_value 'device_name'; then
  export MTEC_DEVICE_NAME="$(bashio::config 'device_name')"
fi

# --- Charge / discharge "active" switch fallback values ---
export MTEC_CHARGE_ACTIVE_VALUE="$(bashio::config 'charge_active_value')"
export MTEC_DISCHARGE_ACTIVE_VALUE="$(bashio::config 'discharge_active_value')"

# --- Misc ---
export MTEC_LANGUAGE="$(bashio::config 'language')"
export MTEC_DEBUG="$(bashio::config 'debug')"

# --- Diagnostic web UI / Ingress ---
# Bind to all interfaces on 8080 so the Supervisor's Ingress proxy can reach
# the UI (the daemon's 127.0.0.1 default is unreachable from the proxy). The
# SPA uses relative URLs, so it works behind the ingress path prefix.
export MTEC_WEB_ENABLE="$(bashio::config 'web_enable')"
export MTEC_WEB_BIND="0.0.0.0:8080"

bashio::log.info "Configuration prepared; MQTT server: ${MTEC_MQTT_SERVER}:${MTEC_MQTT_PORT}, Modbus: ${MTEC_MODBUS_IP}:${MTEC_MODBUS_PORT}"
bashio::log.info "Web UI bound to ${MTEC_WEB_BIND} (served via Ingress)."

# Hand off to the daemon (becomes PID 1). registers.yaml sits in the WORKDIR
# (/app) so the daemon's catalog locator finds it.
exec /usr/bin/mtec2mqtt
