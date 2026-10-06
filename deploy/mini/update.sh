#!/usr/bin/env bash
# update.sh — pull the provider-bridge repo and redeploy.
# The local patches are merged into main of github.com/sfiorini/provider-bridge
# (private fork of an upstream project); there is no rebase step anymore.
# Run as root: sudo /opt/docker/provider-bridge/update.sh
set -euo pipefail
ROOT=/opt/docker/provider-bridge
SRC=$ROOT/src

[[ $EUID -eq 0 ]] || { echo "run as root (sudo)" >&2; exit 1; }

cd "$SRC"
echo '== pull origin/main =='
git fetch origin
git checkout main
git pull --ff-only origin main

echo '== rebuild and restart =='
cd "$ROOT"
docker compose build
docker compose up -d

echo '== health check =='
sleep 5
for i in $(seq 1 12); do
  if curl -sf -H "Authorization: Bearer $(grep -m1 'auth_token:' "$ROOT/config.yml" | awk '{print $2}')" \
      http://127.0.0.1:38440/v1/models > /dev/null 2>&1; then
    echo "healthy"
    exit 0
  fi
  sleep 5
done
echo "health check FAILED — check: docker logs provider-bridge" >&2
exit 1
