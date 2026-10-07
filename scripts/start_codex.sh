#!/usr/bin/env bash
# Start Codex CLI using an already-running Provider Bridge server.
# Requires: start_provider-bridge.sh to have been run first (or .provider-bridge.env present).
set -euo pipefail

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  :  # running directly
else
  echo "Do not source this script; run it directly." >&2
  return 1
fi

usage() {
  cat <<EOF
Usage: $(basename "$0") --project-directory <dir> [--codex-home <dir>]

Required:
  --project-directory <dir>   Workspace to launch Codex in

Optional:
  --codex-home <dir>          Codex home directory (default: ${CODEX_HOME:-$HOME/.codex})
EOF
  exit 1
}

CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
PROJECT_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --codex-home)        CODEX_HOME_DIR="$2"; shift 2 ;;
    --project-directory) PROJECT_DIR="$2";     shift 2 ;;
    *)                   usage ;;
  esac
done

if [[ -z "$PROJECT_DIR" ]]; then
  echo "Error: --project-directory is required"
  usage
fi

# Resolve relative paths to absolute
CODEX_HOME_DIR="$(cd "$CODEX_HOME_DIR" && pwd 2>/dev/null || echo "$CODEX_HOME_DIR")"
PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd 2>/dev/null || echo "$PROJECT_DIR")"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ROOT_DIR}/.provider-bridge.env"
LOG_FILE="${ROOT_DIR}/logs/codex.log"

source "${ROOT_DIR}/scripts/lib/common.sh"

require_command codex
mkdir -p "$CODEX_HOME_DIR" "$(dirname "$LOG_FILE")"

load_env_file "$ENV_FILE"
MODE="${PROVIDER_BRIDGE_MODE:-}"
validate_mode "$MODE" Transform CaptureResponse

SERVER_BIN="${PROVIDER_BRIDGE_SERVER_BIN:-}"
CONFIG_FILE="${PROVIDER_BRIDGE_CONFIG_FILE:-}"

verify_providerbridge_alive

MODEL_ALIAS="${PROVIDER_BRIDGE_CODEX_MODEL:-${PROVIDER_BRIDGE_DEFAULT_MODEL:-}}"


MODEL_ALIAS="${MODEL_ALIAS##*/}"
if [[ -z "$MODEL_ALIAS" ]]; then
  log_error "no model alias configured for Codex"
  exit 1
fi

if [[ -x "$SERVER_BIN" && -f "$CONFIG_FILE" ]]; then
  generate_codex_config "$MODEL_ALIAS" "$CODEX_HOME_DIR" "http://${HOST}:${PORT}/v1"
else
  log_error "cannot generate Codex config: server binary or config not found"
  exit 1
fi

export CODEX_HOME="$CODEX_HOME_DIR"

log "Starting Codex with CODEX_HOME=${CODEX_HOME_DIR}"
log "Workspace: ${PROJECT_DIR}"
log "Mode: ${MODE}"
log "Model: ${MODEL_ALIAS}"

codex_args=(
  --sandbox workspace-write
  --ask-for-approval on-request
  --cd "$PROJECT_DIR"
)

set +e
codex "${codex_args[@]}"
CODEX_STATUS=$?
set -e

log "Codex exited with status ${CODEX_STATUS}"
exit "$CODEX_STATUS"
