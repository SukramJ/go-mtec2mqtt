#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (C) 2026 SukramJ
#
# go-mtec2mqtt — Linux installer.
#
# Downloads a release tarball from GitHub, verifies its SHA-256
# checksum, installs the binaries under /opt/go-mtec2mqtt, creates a
# dedicated `mtec` system user, runs an interactive wizard for the
# three config fields that have no usable default, and registers a
# hardened systemd service.
#
# Designed for the curl|bash idiom:
#
#   curl -sSfL https://raw.githubusercontent.com/SukramJ/go-mtec2mqtt/main/script/install.sh | sudo bash
#
# Pin a specific version:
#
#   curl -sSfL https://raw.githubusercontent.com/SukramJ/go-mtec2mqtt/main/script/install.sh | sudo bash -s -- 1.2.3
#
# The wizard prompts read from /dev/tty so they work even when the
# script itself is being piped through stdin.

set -euo pipefail

# --- knobs ------------------------------------------------------------------

REPO="${REPO:-SukramJ/go-mtec2mqtt}"
PREFIX="${PREFIX:-/opt/go-mtec2mqtt}"
CONFIG_DIR="${CONFIG_DIR:-/etc/go-mtec2mqtt}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.yaml}"
BIN_LINK_DIR="${BIN_LINK_DIR:-/usr/local/bin}"
SERVICE_USER="${SERVICE_USER:-mtec}"
SERVICE_GROUP="${SERVICE_GROUP:-mtec}"
SERVICE_NAME="${SERVICE_NAME:-go-mtec2mqtt}"
SYSTEMD_UNIT="/etc/systemd/system/${SERVICE_NAME}.service"

VERSION="${1:-}"
ASSUME_YES="${ASSUME_YES:-0}"

# --- ansi helpers -----------------------------------------------------------

if [ -t 1 ]; then
	BOLD=$(printf '\033[1m'); DIM=$(printf '\033[2m')
	RED=$(printf '\033[31m'); GREEN=$(printf '\033[32m')
	YELLOW=$(printf '\033[33m'); CYAN=$(printf '\033[36m'); RESET=$(printf '\033[0m')
else
	BOLD=""; DIM=""; RED=""; GREEN=""; YELLOW=""; CYAN=""; RESET=""
fi

info()  { printf '%s==>%s %s\n'      "${CYAN}${BOLD}" "${RESET}" "$*"; }
ok()    { printf '%s  ✓%s %s\n'      "${GREEN}"     "${RESET}" "$*"; }
warn()  { printf '%s  ! %s%s\n'      "${YELLOW}"    "$*" "${RESET}" >&2; }
die()   { printf '%s  ✗ %s%s\n'      "${RED}${BOLD}" "$*" "${RESET}" >&2; exit 1; }

# --- preconditions ----------------------------------------------------------

[ "$(id -u)" -eq 0 ] || die "run as root (use sudo)"
[ "$(uname -s)" = "Linux" ] || die "this installer only supports Linux"
command -v systemctl >/dev/null || die "systemctl not found — this installer requires systemd"

for tool in curl tar awk sha256sum install useradd; do
	command -v "$tool" >/dev/null || die "required tool missing: $tool"
done

# Architecture mapping — only ship the targets we cross-compile in the
# release pipeline. Bail out on anything else with a clear message so
# the user knows to build from source.
case "$(uname -m)" in
	x86_64|amd64) ARCH=amd64 ;;
	aarch64|arm64) ARCH=arm64 ;;
	*) die "unsupported architecture: $(uname -m) (need x86_64 or aarch64)" ;;
esac

# --- interactive prompts ----------------------------------------------------

# read-from-tty so curl|bash still gets keystrokes; falls back to stdin
# when no tty is attached (CI dry-runs).
TTY=/dev/tty
[ -r "$TTY" ] || TTY=/dev/stdin

prompt() {
	local question="$1" default="${2:-}" answer
	if [ -n "$default" ]; then
		printf '%s%s%s [%s]: ' "${BOLD}" "$question" "${RESET}" "$default" > /dev/tty 2>/dev/null \
			|| printf '%s [%s]: ' "$question" "$default"
	else
		printf '%s%s%s: ' "${BOLD}" "$question" "${RESET}" > /dev/tty 2>/dev/null \
			|| printf '%s: ' "$question"
	fi
	IFS= read -r answer <"$TTY" || answer=""
	[ -n "$answer" ] || answer="$default"
	printf '%s' "$answer"
}

prompt_yn() {
	local question="$1" default="${2:-n}" answer
	local hint="[y/N]"; [ "$default" = "y" ] && hint="[Y/n]"
	answer="$(prompt "$question $hint" "$default")"
	case "${answer,,}" in
		y|yes) return 0 ;;
		*) return 1 ;;
	esac
}

# --- version resolution -----------------------------------------------------

resolve_latest_version() {
	# Use the unauthenticated GitHub API. 60 req/hour/IP is plenty for
	# a one-shot installer; the rate limit only matters if a user is
	# spamming the script.
	local url="https://api.github.com/repos/${REPO}/releases/latest"
	local tag
	tag=$(curl -sSfL "$url" | awk -F'"' '/"tag_name":/ {print $4; exit}')
	[ -n "$tag" ] || die "could not resolve latest release from $url"
	# Strip a leading 'v' so we always compare bare semver versions.
	printf '%s' "${tag#v}"
}

if [ -z "$VERSION" ]; then
	info "resolving latest release from github.com/${REPO}"
	VERSION="$(resolve_latest_version)"
	ok "latest is ${BOLD}${VERSION}${RESET}"
else
	VERSION="${VERSION#v}"
	ok "installing requested version ${BOLD}${VERSION}${RESET}"
fi

ARCHIVE="go-mtec2mqtt-${VERSION}-linux-${ARCH}.tar.gz"
RELEASE_BASE="https://github.com/${REPO}/releases/download/${VERSION}"
ARCHIVE_URL="${RELEASE_BASE}/${ARCHIVE}"
CHECKSUM_URL="${RELEASE_BASE}/SHA256SUMS"

# --- download + verify ------------------------------------------------------

WORK="$(mktemp -d -t go-mtec2mqtt-install.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

info "downloading ${ARCHIVE}"
curl -sSfL --retry 3 -o "${WORK}/${ARCHIVE}"   "$ARCHIVE_URL"   \
	|| die "download failed: $ARCHIVE_URL"
curl -sSfL --retry 3 -o "${WORK}/SHA256SUMS"   "$CHECKSUM_URL"  \
	|| die "checksum file download failed: $CHECKSUM_URL"

info "verifying SHA-256"
(
	cd "$WORK"
	# Only verify our specific archive — SHA256SUMS lists every arch.
	grep " ${ARCHIVE}\$" SHA256SUMS \
		| sha256sum --check --strict --status \
		|| { echo "checksum mismatch for ${ARCHIVE}" >&2; exit 1; }
)
ok "checksum OK"

info "extracting"
tar -xzf "${WORK}/${ARCHIVE}" -C "$WORK"
STAGE="${WORK}/go-mtec2mqtt-${VERSION}-linux-${ARCH}"
[ -x "${STAGE}/mtec2mqtt" ] || die "extracted tree is missing mtec2mqtt binary"

# --- service user -----------------------------------------------------------

if id -u "$SERVICE_USER" >/dev/null 2>&1; then
	ok "service user '${SERVICE_USER}' already exists"
else
	info "creating system user '${SERVICE_USER}'"
	useradd --system --no-create-home --shell /usr/sbin/nologin \
		--user-group --comment "go-mtec2mqtt daemon" "$SERVICE_USER"
	ok "user '${SERVICE_USER}' created"
fi

# --- install binaries + assets ---------------------------------------------

if [ -d "$PREFIX" ]; then
	BACKUP="${PREFIX}.bak.$(date -u +%Y%m%d%H%M%S)"
	warn "${PREFIX} exists — moving to ${BACKUP}"
	mv "$PREFIX" "$BACKUP"
fi
info "installing to ${PREFIX}"
install -d -m 0755 -o root -g root "$PREFIX"
install -m 0755 -o root -g root "${STAGE}/mtec2mqtt"        "${PREFIX}/mtec2mqtt"
install -m 0755 -o root -g root "${STAGE}/mtec-util"        "${PREFIX}/mtec-util"
install -m 0644 -o root -g root "${STAGE}/registers.yaml"   "${PREFIX}/registers.yaml"
install -m 0644 -o root -g root "${STAGE}/config-template.yaml" "${PREFIX}/config-template.yaml"
install -m 0644 -o root -g root "${STAGE}/README.md"        "${PREFIX}/README.md"
install -m 0644 -o root -g root "${STAGE}/LICENSE"          "${PREFIX}/LICENSE"
install -m 0644 -o root -g root "${STAGE}/changelog.md"     "${PREFIX}/changelog.md"
ok "binaries + assets installed"

# Symlinks for PATH convenience — mtec-util in particular is more
# useful when it's just a `mtec-util` away.
ln -sf "${PREFIX}/mtec2mqtt" "${BIN_LINK_DIR}/mtec2mqtt"
ln -sf "${PREFIX}/mtec-util" "${BIN_LINK_DIR}/mtec-util"
ok "symlinks in ${BIN_LINK_DIR}"

# --- config wizard ----------------------------------------------------------

install -d -m 0755 -o root -g root "$CONFIG_DIR"

if [ -f "$CONFIG_FILE" ]; then
	ok "config already present at ${CONFIG_FILE} — leaving it untouched"
else
	info "no config at ${CONFIG_FILE} — running first-time wizard"
	printf '\n%s%sFill in the three fields that have no sensible default.%s\n' \
		"${DIM}" "" "${RESET}" > /dev/tty 2>/dev/null || true
	printf '%s%sEverything else is taken from the template and can be edited later.%s\n\n' \
		"${DIM}" "" "${RESET}" > /dev/tty 2>/dev/null || true

	MODBUS_IP="$(prompt 'M-TEC inverter IP or hostname' 'espressif.fritz.box')"
	MQTT_SERVER="$(prompt 'MQTT broker host' 'localhost')"
	if prompt_yn 'Enable Home Assistant auto-discovery?' n; then
		HASS_ENABLE=true
	else
		HASS_ENABLE=false
	fi

	# Build the live config from the shipped template by replacing
	# only the three fields we asked about. Other operators (passwords,
	# refresh intervals, etc.) can be edited later in $CONFIG_FILE.
	tmp_cfg="$(mktemp)"
	awk \
		-v ip="$MODBUS_IP" \
		-v mqtt="$MQTT_SERVER" \
		-v hass="$HASS_ENABLE" '
		/^MODBUS_IP:/  { print "MODBUS_IP: "  ip;   next }
		/^MQTT_SERVER:/ { print "MQTT_SERVER: " mqtt; next }
		/^HASS_ENABLE:/ { print "HASS_ENABLE: " hass; next }
		{ print }
	' "${STAGE}/config-template.yaml" > "$tmp_cfg"

	install -m 0640 -o root -g "$SERVICE_GROUP" "$tmp_cfg" "$CONFIG_FILE"
	rm -f "$tmp_cfg"
	ok "wrote ${CONFIG_FILE} (mode 0640, group ${SERVICE_GROUP})"
fi

# --- systemd unit -----------------------------------------------------------

info "writing systemd unit ${SYSTEMD_UNIT}"
cat > "$SYSTEMD_UNIT" <<EOF
# Generated by go-mtec2mqtt install.sh ${VERSION}
# Hand-edits survive — re-running the installer will preserve this file
# only when the contents match; otherwise it is rewritten.

[Unit]
Description=M-TEC Energybutler MQTT bridge (${SERVICE_NAME})
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
ExecStart=${PREFIX}/mtec2mqtt --config ${CONFIG_FILE} --registers ${PREFIX}/registers.yaml
Restart=on-failure
RestartSec=5
TimeoutStopSec=10

# Hardening — the daemon only needs outbound TCP to the inverter +
# broker, so everything else is closed off.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
LockPersonality=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$SYSTEMD_UNIT"
ok "unit installed"

systemctl daemon-reload
systemctl enable "${SERVICE_NAME}" >/dev/null
ok "service enabled"

# If a previous version was running, restart to pick up the new
# binary. On a fresh install, just start.
if systemctl is-active --quiet "${SERVICE_NAME}"; then
	info "restarting ${SERVICE_NAME}"
	systemctl restart "${SERVICE_NAME}"
else
	info "starting ${SERVICE_NAME}"
	systemctl start "${SERVICE_NAME}"
fi

# Give the daemon a moment to either come up or die loudly.
sleep 2

if systemctl is-active --quiet "${SERVICE_NAME}"; then
	ok "${SERVICE_NAME} is running"
else
	warn "${SERVICE_NAME} did not stay up — see: journalctl -u ${SERVICE_NAME} -n 50"
	systemctl status --no-pager "${SERVICE_NAME}" || true
	exit 1
fi

# --- done -------------------------------------------------------------------

cat <<EOF

${GREEN}${BOLD}install complete${RESET} — go-mtec2mqtt ${VERSION} (linux/${ARCH})

  binaries     ${PREFIX}/{mtec2mqtt,mtec-util}
  symlinks     ${BIN_LINK_DIR}/{mtec2mqtt,mtec-util}
  config       ${CONFIG_FILE}
  unit         ${SYSTEMD_UNIT}
  service user ${SERVICE_USER}

useful commands:
  systemctl status   ${SERVICE_NAME}
  journalctl -u      ${SERVICE_NAME} -f
  mtec-util                                  # interactive register CLI
  sudo nano ${CONFIG_FILE}                   # edit MQTT credentials, intervals, …
  sudo systemctl restart ${SERVICE_NAME}     # after editing config
EOF
