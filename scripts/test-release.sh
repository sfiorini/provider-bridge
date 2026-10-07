#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/release.lib.sh"
fails=0
assert_eq() { # <got> <want> <label>
    [ "$1" = "$2" ] || { echo "FAIL $3: got '$1' want '$2'"; fails=$((fails+1)); }
}
assert_ok()  { "$@" >/dev/null 2>&1 || { echo "FAIL: $* should succeed"; fails=$((fails+1)); }; }
assert_fail(){ "$@" >/dev/null 2>&1 && { echo "FAIL: $* should fail";  fails=$((fails+1)); }; return 0; }

assert_eq "$(bump_version 0.1.0 patch)" "0.1.1" "patch"
assert_eq "$(bump_version 0.1.0 minor)" "0.2.0" "minor"
assert_eq "$(bump_version 0.1.0 major)" "1.0.0" "major"
assert_eq "$(bump_version 0.1.0 0.1.0)" "0.1.0" "explicit-same"
assert_eq "$(bump_version 1.2.9 patch)" "1.2.10" "numeric-rollover"
assert_eq "$(bump_version 0.9.9 minor)" "0.10.0" "minor-rollover"
assert_fail bump_version 0.1.0 bogus
assert_fail bump_version 0.1 patch
bump_version 0.1.0 bogus >/dev/null 2>&1 && rc=0 || rc=$?
assert_eq "$rc" "1" "invalid-mode-exit-1"
OUT="$(printf 'feat: a\nfix: b\n' | render_changelog_entry 1.2.3 2026-10-07)"
assert_eq "$OUT" "$(printf '## [Unreleased]\n\n## [1.2.3] - 2026-10-07\n\n- feat: a\n- fix: b')" "render-entry"
OUT="$(printf '' | render_changelog_entry 1.2.3 2026-10-07)"
case "$OUT" in *"## [1.2.3] - 2026-10-07"*"- Version bump"*) ;; *) echo "FAIL render-empty"; fails=$((fails+1));; esac
TMP="$(mktemp -d)"; printf '# Changelog\n\n## [Unreleased]\n\n### Added\n\n- thing\n' > "$TMP/C.md"
printf -- '- feat: x\n' > "$TMP/body"
changelog_update "$TMP/C.md" 2.0.0 2026-10-07 "$TMP/body"
grep -q '^## \[2.0.0\] - 2026-10-07' "$TMP/C.md" || { echo "FAIL changelog section"; fails=$((fails+1)); }
grep -q '^## \[Unreleased\]' "$TMP/C.md"  || { echo "FAIL unreleased kept";  fails=$((fails+1)); }
rm -rf "$TMP"

[ "$fails" -eq 0 ] && echo "release.lib tests: OK" || { echo "$fails failures"; exit 1; }
