#!/usr/bin/env bash
# Cut a release: bump VERSION, fold [Unreleased] into a versioned CHANGELOG
# section, commit "release: vX.Y.Z", tag vX.Y.Z, push branch + tag.
# Dependency-free; works without local Go (tests are deferred to tag CI).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/release.lib.sh"

BUMP="" DRY_RUN=0 ASSUME_YES=0 SYNC_CHECK=1
while [ $# -gt 0 ]; do
    case "$1" in
        --bump) BUMP="${2:-}"; shift 2 ;;
        --dry-run) DRY_RUN=1; shift ;;
        --yes|-y) ASSUME_YES=1; shift ;;
        --no-sync-check) SYNC_CHECK=0; shift ;;
        -h|--help) sed -n '2,5p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
done

[ -f "$SCRIPT_DIR/../VERSION" ] || { echo "VERSION file missing" >&2; exit 1; }
CUR="$(tr -d '[:space:]' < "$SCRIPT_DIR/../VERSION")"
valid_version "$CUR" || { echo "VERSION must be x.y.z, got: $CUR" >&2; exit 1; }

if [ -z "$BUMP" ]; then
    printf 'Bump which part? 1) patch  2) minor  3) major : '
    read -r choice
    case "$choice" in 1) BUMP=patch ;; 2) BUMP=minor ;; 3) BUMP=major ;; *) echo "invalid choice" >&2; exit 2 ;; esac
fi
NEW="$(bump_version "$CUR" "$BUMP")"
DATE="$(date -u +%F)"

echo "Current: v$CUR -> New: v$NEW"
echo "Tag v$NEW will be created; CI builds, pushes GHCR image, publishes the GitHub Release."
[ "$DRY_RUN" -eq 1 ] && { echo "dry-run: no changes made"; exit 0; }

# ---- pre-flight ----
[ -z "$(git status --porcelain)" ] || { echo "working tree not clean" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/v$NEW" >/dev/null && { echo "tag v$NEW already exists" >&2; exit 1; }
if [ "$SYNC_CHECK" -eq 1 ]; then
    BRANCH="$(git branch --show-current)"
    git fetch -q origin
    [ "$(git rev-parse HEAD)" = "$(git rev-parse "origin/$BRANCH")" ] || { echo "not in sync with origin/$BRANCH" >&2; exit 1; }
fi
if command -v go >/dev/null 2>&1; then
    (cd "$SCRIPT_DIR/.." && CGO_ENABLED=0 go test ./...) || { echo "tests failed" >&2; exit 1; }
else
    echo "note: go not on PATH — tests deferred to tag CI"
fi
[ "$ASSUME_YES" -eq 1 ] || { printf 'Proceed with v%s? [y/N] ' "$NEW"; read -r ans; case "$ans" in y|Y) ;; *) echo aborted; exit 1 ;; esac; }

# ---- mutate (with rollback) ----
ROLLBACK_TAG=0 ROLLBACK_COMMIT=0
rollback() {
    [ "$ROLLBACK_TAG" -eq 1 ] && git tag -d "v$NEW" 2>/dev/null || true
    [ "$ROLLBACK_COMMIT" -eq 1 ] && git reset --soft HEAD~1 2>/dev/null || true
    git checkout HEAD -- VERSION CHANGELOG.md 2>/dev/null || true
}
trap 'rollback; exit 1' ERR

printf '%s\n' "$NEW" > "$SCRIPT_DIR/../VERSION"
LAST_TAG="$(git describe --tags --match 'v[0-9]*' --abbrev=0 2>/dev/null || true)"
if [ -n "$LAST_TAG" ]; then
    git log "$LAST_TAG..HEAD" --pretty=format:'%s' --no-merges > /tmp/pb-rel-body.$$ 2>/dev/null || : > /tmp/pb-rel-body.$$
else
    git log --pretty=format:'%s' --no-merges > /tmp/pb-rel-body.$$ || : > /tmp/pb-rel-body.$$
fi
[ -f "$SCRIPT_DIR/../CHANGELOG.md" ] || printf '# Changelog\n\n## [Unreleased]\n' > "$SCRIPT_DIR/../CHANGELOG.md"
[ -s "/tmp/pb-rel-body.$$" ] || printf -- '- Version bump\n' > "/tmp/pb-rel-body.$$"
changelog_update "$SCRIPT_DIR/../CHANGELOG.md" "$NEW" "$DATE" "/tmp/pb-rel-body.$$"
rm -f "/tmp/pb-rel-body.$$"

git add VERSION CHANGELOG.md
git commit -m "release: v$NEW"
ROLLBACK_COMMIT=1
git tag "v$NEW"
ROLLBACK_TAG=1
BRANCH="${BRANCH:-$(git branch --show-current)}"
git push origin "$BRANCH"
git push origin "v$NEW"
trap - ERR
echo "Released v$NEW. CI will build, publish GHCR image and the GitHub Release."
