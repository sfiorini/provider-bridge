#!/usr/bin/env bash
# Build and start the Provider Bridge server in background.
# Saves connection info to .provider-bridge.env for client scripts to consume.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_FILE="${PROVIDER_BRIDGE_CONFIG:-"${ROOT_DIR}/config.yml"}"
SERVER_BIN="${ROOT_DIR}/.cache/providerbridge/providerbridge"
CACHE_DIR="${ROOT_DIR}/.cache/providerbridge"
LOG_FILE="${ROOT_DIR}/logs/providerbridge.log"
ENV_FILE="${ROOT_DIR}/.provider-bridge.env"

source "${ROOT_DIR}/scripts/lib/common.sh"
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then :; else echo "Do not source this script; run it directly." >&2; return 1; fi

require_command go
check_config_file

mkdir -p "$(dirname "$LOG_FILE")" "$CACHE_DIR"
setup_build_cache

build_providerbridge "$SERVER_BIN"

extract_server_metadata
ensure_port_free

# Write env file for client scripts.
cat > "$ENV_FILE" <<EOF
PROVIDER_BRIDGE_ADDR="${ADDR}"
PROVIDER_BRIDGE_MODE="${MODE}"
PROVIDER_BRIDGE_DEFAULT_MODEL="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-default-model 2>/dev/null || true)"
PROVIDER_BRIDGE_CODEX_MODEL="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-codex-model 2>/dev/null || true)"
PROVIDER_BRIDGE_CLAUDE_MODEL="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-claude-model 2>/dev/null || true)"
PROVIDER_BRIDGE_CONFIG_FILE="${CONFIG_FILE}"
PROVIDER_BRIDGE_SERVER_BIN="${SERVER_BIN}"
PROVIDER_BRIDGE_LOG_FILE="${LOG_FILE}"
EOF

cleanup() {
  cleanup_server
  rm -f "$ENV_FILE"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

start_server_background

log "Server running in background (PID ${SERVER_PID}). Press Ctrl+C to stop."
wait "$SERVER_PID"
