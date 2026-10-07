#!/usr/bin/env bash
# Start Claude Code using an already-running Provider Bridge server (CaptureAnthropic mode).
# Requires: start_provider-bridge.sh to have been run first (or .provider-bridge.env present).
set -euo pipefail

ROOT_DIR="$(cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)" && pwd)"
ENV_FILE="${ROOT_DIR}/.provider-bridge.env"
CLAUDE_CONFIG_DIR_VALUE="${ROOT_DIR}/FakeHome/ClaudeCode"
GLOBAL_CLAUDE_SETTINGS="${PROVIDER_BRIDGE_CLAUDE_SETTINGS:-"${HOME}/.claude/settings.json"}"
LOG_FILE="${ROOT_DIR}/logs/claude-code.log"
PROMPT="${1:-}"

source "${ROOT_DIR}/scripts/lib/common.sh"
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then :; else echo "Do not source this script; run it directly." >&2; return 1; fi

require_command claude
require_command python3
mkdir -p "$CLAUDE_CONFIG_DIR_VALUE" "$(dirname "$LOG_FILE")"

load_env_file "$ENV_FILE"
MODE="${PROVIDER_BRIDGE_MODE:-}"
validate_mode "$MODE" CaptureAnthropic

verify_providerbridge_alive

MODEL="${PROVIDER_BRIDGE_CLAUDE_MODEL:-}"

prepare_claude_settings \
  "${CLAUDE_CONFIG_DIR_VALUE}/settings.json" \
  "${CLAUDE_CONFIG_DIR_VALUE}/providerbridge-env.sh" \
  "http://${BASE_ADDR}" \
  "$GLOBAL_CLAUDE_SETTINGS" \
  "$MODEL" > >(tee -a "$LOG_FILE") 2>&1

export CLAUDE_CONFIG_DIR="$CLAUDE_CONFIG_DIR_VALUE"

log "Starting Claude Code with CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR}"
log "Workspace: ${ROOT_DIR}"
log "Anthropic base URL: http://${BASE_ADDR}"
if [[ -n "${PROVIDER_BRIDGE_EFFECTIVE_CLAUDE_MODEL:-}" ]]; then
  log "Model: ${PROVIDER_BRIDGE_EFFECTIVE_CLAUDE_MODEL}"
fi

set +e
if [[ -n "$PROMPT" ]]; then
  claude "$PROMPT"
else
  claude
fi
CLAUDE_STATUS=$?
set -e

log "Claude Code exited with status ${CLAUDE_STATUS}"
exit "$CLAUDE_STATUS"
