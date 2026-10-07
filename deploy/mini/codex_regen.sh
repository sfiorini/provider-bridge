#!/usr/bin/env bash
# codex_regen.sh - regenerate Codex config.toml + models_catalog.json from providerbridge.
# Run as root:  sudo /opt/docker/provider-bridge/codex_regen.sh
set -euo pipefail

ROOT="/opt/docker/provider-bridge"
CFG_YML="$ROOT/config.yml"
OUT="$ROOT/generated_configs"
IMAGE="provider-bridge:latest"
BASE_URL="http://127.0.0.1:38440/v1"
OWNER="${SUDO_USER:-root}"   # hand the generated files to whoever ran sudo

[ -f "$CFG_YML" ] || { echo "missing $CFG_YML" >&2; exit 1; }
mkdir -p "$OUT"

# Run providerbridge as root (--user 0:0) inside an ephemeral container so it can
# read config.yml and write the catalog without any uid/gid gymnastics.
MODEL="$(docker run --rm --user 0:0 \
  -v "$CFG_YML":/config/config.yml:ro \
  "$IMAGE" -config /config/config.yml -print-codex-model)"

docker run --rm --user 0:0 \
  -v "$CFG_YML":/config/config.yml:ro \
  -v "$OUT":"$OUT" \
  "$IMAGE" \
  -config /config/config.yml \
  -print-codex-config "$MODEL" \
  -codex-base-url "$BASE_URL" \
  -codex-home "$OUT" \
  > "$OUT/config.toml"

# config.toml is written by this script (stdout redirect); the others by the container.
chmod 644 "$OUT/config.toml" "$OUT/models_catalog.json"
chmod 600 "$OUT/auth.json" 2>/dev/null || true
[ "$OWNER" != "root" ] && chown -R "$OWNER":"$OWNER" "$OUT"

echo "Wrote configs for model '$MODEL' (owner: $OWNER):"
ls -la "$OUT"
