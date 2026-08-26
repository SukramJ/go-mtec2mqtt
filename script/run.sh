#!/usr/bin/with-contenv bashio
# SPDX-License-Identifier: MIT
# Home Assistant add-on entrypoint for go-mtec2mqtt.
#
# Reads the user's add-on options (/data/options.json) via bashio, maps them
# onto the daemon's MTEC_* environment variables, wires up Ingress, and
# finally exec's the binary so it becomes PID 1 and receives signals directly.
# The daemon ships no config file in the add-on image — every setting is
# supplied through MTEC_* env here (main.go falls back to env-only when no
# config.yaml is found).
#
# Every `bashio::config` read is assigned to a plain variable BEFORE it is
# exported. `export VAR="$(cmd)"` in one step discards the exit status of
# the command substitution (SC2155) — a failing bashio call would silently
# produce an empty value instead of aborting the script. Splitting the
# assignment from the export lets `set -e` see and act on the failure.
set -euo pipefail

bashio::log.info "Starting go-mtec2mqtt add-on..."

# --- Modbus (M-TEC inverter / espressif gateway) ---
MTEC_MODBUS_IP="$(bashio::config 'modbus_ip')"
MTEC_MODBUS_PORT="$(bashio::config 'modbus_port')"
MTEC_MODBUS_SLAVE="$(bashio::config 'modbus_slave')"
MTEC_MODBUS_TIMEOUT="$(bashio::config 'modbus_timeout')"
MTEC_MODBUS_RETRIES="$(bashio::config 'modbus_retries')"
MTEC_MODBUS_FRAMER="$(bashio::config 'modbus_framer')"
export MTEC_MODBUS_IP MTEC_MODBUS_PORT MTEC_MODBUS_SLAVE MTEC_MODBUS_TIMEOUT MTEC_MODBUS_RETRIES MTEC_MODBUS_FRAMER

# --- MQTT ---
# Zero-config: when mqtt_server is left empty, borrow the broker the
# Supervisor already knows about (the HA MQTT integration / core-mosquitto
# add-on) via the mqtt service. An explicit mqtt_server always wins; if
# nothing is set and no service is offered, fall back to core-mosquitto:1883.
if bashio::config.has_value 'mqtt_server'; then
  MTEC_MQTT_SERVER="$(bashio::config 'mqtt_server')"
  MTEC_MQTT_PORT="$(bashio::config 'mqtt_port')"
  MTEC_MQTT_LOGIN="$(bashio::config 'mqtt_login')"
  MTEC_MQTT_PASSWORD="$(bashio::config 'mqtt_password')"
  export MTEC_MQTT_SERVER MTEC_MQTT_PORT MTEC_MQTT_LOGIN MTEC_MQTT_PASSWORD
elif bashio::services.available 'mqtt'; then
  bashio::log.info "mqtt_server empty; using the Home Assistant MQTT service."
  MTEC_MQTT_SERVER="$(bashio::services 'mqtt' 'host')"
  MTEC_MQTT_PORT="$(bashio::services 'mqtt' 'port')"
  MTEC_MQTT_LOGIN="$(bashio::services 'mqtt' 'username')"
  MTEC_MQTT_PASSWORD="$(bashio::services 'mqtt' 'password')"
  export MTEC_MQTT_SERVER MTEC_MQTT_PORT MTEC_MQTT_LOGIN MTEC_MQTT_PASSWORD
else
  bashio::log.warning "mqtt_server empty and no MQTT service offered; falling back to core-mosquitto:1883."
  export MTEC_MQTT_SERVER="core-mosquitto"
  export MTEC_MQTT_PORT="1883"
fi
MTEC_MQTT_TOPIC="$(bashio::config 'mqtt_topic')"
export MTEC_MQTT_TOPIC

# --- Home Assistant discovery ---
MTEC_HASS_ENABLE="$(bashio::config 'hass_enable')"
MTEC_HASS_UNIQUE_ID_INCLUDE_SERIAL="$(bashio::config 'hass_unique_id_include_serial')"
export MTEC_HASS_ENABLE MTEC_HASS_UNIQUE_ID_INCLUDE_SERIAL

# Optional device name — only export when set so an empty field keeps the
# generic device identity instead of overriding it with a blank name.
if bashio::config.has_value 'device_name'; then
  MTEC_DEVICE_NAME="$(bashio::config 'device_name')"
  export MTEC_DEVICE_NAME
fi

# --- Charge / discharge "active" switch fallback values ---
MTEC_CHARGE_ACTIVE_VALUE="$(bashio::config 'charge_active_value')"
MTEC_DISCHARGE_ACTIVE_VALUE="$(bashio::config 'discharge_active_value')"
export MTEC_CHARGE_ACTIVE_VALUE MTEC_DISCHARGE_ACTIVE_VALUE

# --- Misc ---
MTEC_LANGUAGE="$(bashio::config 'language')"
MTEC_DEBUG="$(bashio::config 'debug')"
export MTEC_LANGUAGE MTEC_DEBUG

# --- Diagnostic web UI / Ingress ---
# Bind to all interfaces on 8080 so the Supervisor's Ingress proxy can reach
# the UI (the daemon's 127.0.0.1 default is unreachable from the proxy). The
# SPA uses relative URLs, so it works behind the ingress path prefix.
MTEC_WEB_ENABLE="$(bashio::config 'web_enable')"
export MTEC_WEB_ENABLE
export MTEC_WEB_BIND="0.0.0.0:8080"

bashio::log.info "Configuration prepared; MQTT server: ${MTEC_MQTT_SERVER}:${MTEC_MQTT_PORT}, Modbus: ${MTEC_MODBUS_IP}:${MTEC_MODBUS_PORT}"
bashio::log.info "Web UI bound to ${MTEC_WEB_BIND} (served via Ingress)."

# Hand off to the daemon (becomes PID 1). registers.yaml sits in the WORKDIR
# (/app) so the daemon's catalog locator finds it.
exec /usr/bin/mtec2mqtt
