#!/usr/bin/env bash
# secrets-audit.sh - reproducible secrets gate for provider-bridge (M8).
#
# Proves that no real operator credential (API key, access/bearer token,
# password, private key) is reachable from this repository: working tree,
# every commit reachable from any ref (history, tags, refs/pull/*), and
# every commit message.
#
# Modes:
#   (default)    report all findings: DIRTY (high-confidence), REVIEW
#                (generic credential assignment - manual look), INFO
#                (whitelisted fixture or placeholder-shaped value; never fails)
#   --gate       strict mode: suppress INFO lines; exit 1 only on DIRTY
#   --self-test  build a throwaway git repo and verify detection end-to-end
#   -h, --help   print this header
#
# Exit codes: 0 = clean / self-test OK; 1 = DIRTY finding(s); 2 = usage error.
#
# Constraints: macOS (BSD grep) and Linux safe; no grep -P. Never prints a
# full secret value (matches masked to first/last 4 chars). No network
# access; GitHub-side checks (gh secret list, gh variable list) are manual.
set -euo pipefail

SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
GATE=0 SELF_TEST=0
while [ $# -gt 0 ]; do
    case "$1" in
        --gate) GATE=1; shift ;;
        --self-test) SELF_TEST=1; shift ;;
        -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1 (usage: scripts/secrets-audit.sh [--gate] [--self-test])" >&2; exit 2 ;;
    esac
done

# High-confidence secret shapes (ERE). The sk- branch requires at least one
# digit followed by 15+ key-class chars so hyphenated prose (e.g. "risk-based")
# cannot trip it.
RE_HARD='-----BEGIN [A-Z ]*PRIVATE KEY-----|sk-[A-Za-z0-9_-]*[0-9][A-Za-z0-9_-]{15,}|tvly-[A-Za-z0-9_-]{16,}|gh[oprsu]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|Bearer [A-Za-z0-9._~+/=-]{20,}'

# Generic credential assignment (REVIEW/INFO only; NEVER fails the gate).
RE_INFO="(api_key|apikey|api_token|auth_token|access_token|secret|password|passwd)[[:space:]]*[:=][[:space:]]*[\"']?[A-Za-z0-9_./+=-]{8,}"

# Placeholder markers: a REVIEW candidate whose matched text contains one of
# these word-boundary markers (or a <...> / ${...} template) is downgraded to
# INFO instead of REVIEW - it matches a known placeholder convention
# (your-*, test-*, sk-ant-xxx, replace-with-*, config templates), not a value
# needing an eyeball. CONSERVATIVE BY CONSTRUCTION: RE_HARD is tested FIRST
# and is not affected by this rule, so a real secret is always DIRTY even
# when its line also contains a placeholder word.
RE_PLACEHOLDER='(^|[^A-Za-z0-9])(your|test|sample|example|placeholder|dummy|fake|fixture|changeme|xxx+)([^A-Za-z0-9]|$)|replace[-_ ]?with|<[^>]+>|[$][{][^}]*[}]'

# Known test fixtures (exact strings, as enumerated by the 2026-10-07 audit).
# Only WHOLE-TOKEN occurrences are stripped (see strip_whitelist): an
# occurrence counts only when the characters around it (where present) are
# not key-class ([A-Za-z0-9_-]), so a fixture embedded inside a longer token
# is KEPT (a fixture-prefixed real secret still matches RE_HARD), while a
# fixture next to a real secret on the same line is stripped and the secret
# still fires.
WHITELIST=(
    'sk-ant-test-key-12345678'
    'sk-ant-your-key-here'
    'sk-your-key-here'
    'sk-e2e-lifecycle-key'
    'sk-imported-key-12345'
    'tvly-test-key'
    'tvly-...'
    'fc-...'
    'client-placeholder-token'
    'secret-token'
    'replace-with-your-secret-token'
    'replace-with-real-openai-key'
    'replace-with-real-anthropic-key'
    'replace-with-real-api-key'
)

HARD=0 REVIEW=0 INFO=0

mask() { # <string> -> masked string on stdout
    local s="$1"
    if [ "${#s}" -le 12 ]; then
        printf '********'
    else
        printf '%s...%s' "${s:0:4}" "${s: -4}"
    fi
}

is_keyclass() { # <char> -> 0 if the char is in [A-Za-z0-9_-], else 1
    case "$1" in
        [A-Za-z0-9_-]) return 0 ;;
        *) return 1 ;;
    esac
}

strip_whitelist() { # <line> -> line with whole-token fixture occurrences removed
    # An occurrence of a fixture counts as a whole token only when the
    # character before and after it (where present) is NOT key-class
    # ([A-Za-z0-9_-]). Delimiters (space, : = " ' , < >) are not key-class,
    # so fixtures in normal assignments and quoted values are stripped;
    # a fixture directly abutting key-class chars (embedded in a longer
    # token) is KEPT, so a fixture-prefixed real secret still matches
    # RE_HARD. The char-before is taken from the already-emitted text, so
    # the boundary is recomputed per occurrence; a doubled fixture
    # separated by a non-key-class delimiter (e.g. FIXTURE+FIXTURE) has
    # whole-token occurrences and is stripped - stripping a whitelisted
    # fixture is always safe.
    local line="$1" w out pre post ch_pre ch_post
    for w in "${WHITELIST[@]}"; do
        out=""
        while :; do
            pre="${line%%"$w"*}"              # text before 1st occurrence
            if [ "$pre" = "$line" ]; then     # no (more) occurrences
                out="$out$line"
                break
            fi
            post="${line#"$pre""$w"}"         # text after 1st occurrence
            ch_pre="${out}${pre}"; ch_pre="${ch_pre: -1}"   # char before
            ch_post="${post:0:1}"             # char after
            if { [ -z "$ch_pre" ] || ! is_keyclass "$ch_pre"; } && \
               { [ -z "$ch_post" ] || ! is_keyclass "$ch_post"; }; then
                out="$out$pre"                # whole token: drop the fixture
            else
                out="$out$pre$w"              # embedded: keep the fixture text
            fi
            line="$post"
        done
        line="$out"
    done
    printf '%s\n' "$line"
}

emit() { # <origin> <location> <content-line> - classify one hit
    local origin="$1" loc="$2" line="$3" rest m
    rest="$(strip_whitelist "$line")"
    if printf '%s\n' "$rest" | grep -Eiq -- "$RE_HARD"; then
        HARD=$((HARD + 1))
        m="$(printf '%s\n' "$rest" | grep -Eio -- "$RE_HARD" | head -n 1 || true)"
        printf 'DIRTY  %-10s %s  match=%s\n' "$origin" "$loc" "$(mask "$m")"
        return 0
    fi
    if printf '%s\n' "$line" | grep -Eiq -- "$RE_INFO"; then
        if printf '%s\n' "$rest" | grep -Eiq -- "$RE_INFO"; then
            m="$(printf '%s\n' "$rest" | grep -Eio -- "$RE_INFO" | head -n 1 || true)"
            if printf '%s\n' "$m" | grep -Eiq -- "$RE_PLACEHOLDER"; then
                INFO=$((INFO + 1))
                if [ "$GATE" -eq 0 ]; then
                    printf 'INFO   %-10s %s  (placeholder-shaped value; not a gate failure)\n' "$origin" "$loc"
                fi
            else
                REVIEW=$((REVIEW + 1))
                printf 'REVIEW %-10s %s  match=%s\n' "$origin" "$loc" "$(mask "$m")"
            fi
        else
            INFO=$((INFO + 1))
            if [ "$GATE" -eq 0 ]; then
                printf 'INFO   %-10s %s  (whitelisted fixture; not a gate failure)\n' "$origin" "$loc"
            fi
        fi
    fi
    return 0
}

scan() { # scans the repository at $PWD
    local tmp hits f commit r path lineno content raw kind body
    tmp="$(mktemp -d)"
    hits="$tmp/hits"
    : > "$hits"

    echo "scan: working tree" >&2
    while IFS= read -r -d '' f; do
        [ -f "$f" ] || continue
        grep -H -I -n -i -E -e "$RE_HARD" -e "$RE_INFO" -- "$f" \
            | sed 's/^/TREE|/' >> "$hits" || true
    done < <(git ls-files -z --cached --others --exclude-standard)

    if [ -n "$(git rev-list --all)" ]; then
        echo "scan: full history ($(git rev-list --all | wc -l | tr -d ' ') commits)" >&2
        git grep -I -n -i -E -e "$RE_HARD" -e "$RE_INFO" $(git rev-list --all) -- \
            | sed 's/^/HIST|/' >> "$hits" || true
        echo "scan: commit messages" >&2
        for commit in $(git rev-list --all); do
            git log -1 --format='%B' "$commit" \
                | grep -n -i -E -e "$RE_HARD" -e "$RE_INFO" \
                | sed "s/^/MSG|$commit:/" >> "$hits" || true
        done
    fi

    while IFS= read -r raw; do
        [ -n "$raw" ] || continue
        kind="${raw%%|*}"; body="${raw#*|}"
        case "$kind" in
            TREE)
                path="${body%%:*}"; r="${body#*:}"
                lineno="${r%%:*}"; content="${r#*:}"
                emit tree "${path}:${lineno}" "$content"
                ;;
            HIST)
                commit="${body%%:*}"; r="${body#*:}"
                path="${r%%:*}"; r="${r#*:}"
                lineno="${r%%:*}"; content="${r#*:}"
                emit history "${commit:0:8}:${path}:${lineno}" "$content"
                ;;
            MSG)
                commit="${body%%:*}"; r="${body#*:}"
                lineno="${r%%:*}"; content="${r#*:}"
                emit commit-msg "${commit:0:8}:${lineno}" "$content"
                ;;
        esac
    done < "$hits"

    rm -rf "$tmp"
    if [ "$HARD" -gt 0 ]; then
        echo "SECRETS AUDIT: DIRTY - $HARD high-confidence finding(s), $REVIEW review item(s), $INFO whitelisted fixture/placeholder hit(s)"
        return 1
    fi
    echo "SECRETS AUDIT: CLEAN - 0 high-confidence findings ($REVIEW review item(s), $INFO whitelisted fixture/placeholder hit(s))"
    return 0
}

self_test() {
    local tmp fake out
    tmp="$(mktemp -d)" || return 2
    fake="sk-ant-self""test-0123456789abcdefghij"
    out="$tmp/out"
    git -C "$tmp" init -q
    git -C "$tmp" config user.email selftest@example.invalid
    git -C "$tmp" config user.name "secrets-audit self-test"

    printf 'api_key: sk-ant-test-key-12345678\napi_key: your-api-key-here\n' > "$tmp/fixture.yml"
    git -C "$tmp" add -A
    git -C "$tmp" commit -qm "fixture only"

    # 1) fixture-only repo: --gate must pass, silently (INFO suppressed).
    if ( cd "$tmp" && "$SELF" --gate ) > "$out" 2>&1; then :; else
        echo "self-test FAIL: fixture-only repo must pass the gate" >&2
        sed -n '1,10p' "$out" >&2; rm -rf "$tmp"; return 1
    fi
    grep -q '^SECRETS AUDIT: CLEAN' "$out" || { echo "self-test FAIL: expected CLEAN verdict" >&2; rm -rf "$tmp"; return 1; }
    if grep -q 'INFO' "$out"; then echo "self-test FAIL: --gate must suppress INFO lines" >&2; rm -rf "$tmp"; return 1; fi

    # 2) default mode must report the whitelisted fixture AND the
    #    placeholder-shaped assignment as INFO (never REVIEW), exit 0.
    if ( cd "$tmp" && "$SELF" ) > "$out" 2>&1; then :; else
        echo "self-test FAIL: default mode must exit 0 on fixtures" >&2; rm -rf "$tmp"; return 1
    fi
    grep -Fq '(whitelisted fixture; not a gate failure)' "$out" || { echo "self-test FAIL: expected INFO line for whitelisted fixture" >&2; rm -rf "$tmp"; return 1; }
    grep -Fq '(placeholder-shaped value; not a gate failure)' "$out" || { echo "self-test FAIL: expected INFO line for placeholder-shaped assignment" >&2; rm -rf "$tmp"; return 1; }
    if grep -q '^REVIEW' "$out"; then echo "self-test FAIL: placeholder-shaped assignment must be INFO, not REVIEW" >&2; rm -rf "$tmp"; return 1; fi

    # 3) real-shaped secret in history only (file deleted in a follow-up
    #    commit). Line 1 carries a whitelisted fixture NEXT TO the secret on
    #    the same line: stripping the fixture must still leave a DIRTY match.
    printf 'creds: sk-ant-test-key-12345678 next-to %s\nkey: %s\n' "$fake" "$fake" > "$tmp/leaked.yml"
    git -C "$tmp" add -A
    git -C "$tmp" commit -qm "oops: add leaked config"
    git -C "$tmp" rm -q leaked.yml
    git -C "$tmp" commit -qm "remove leaked config"
    if ( cd "$tmp" && "$SELF" --gate ) > "$out" 2>&1; then
        echo "self-test FAIL: history-only secret must fail the gate" >&2; rm -rf "$tmp"; return 1
    fi
    [ "$(grep -Fc 'DIRTY  history' "$out")" = "2" ] || { echo "self-test FAIL: expected exactly 2 history DIRTY lines (same-line fixture+secret must still fire)" >&2; sed -n '1,10p' "$out" >&2; rm -rf "$tmp"; return 1; }
    if grep -Fq -- "$fake" "$out"; then echo "self-test FAIL: full secret value printed (masking broken)" >&2; rm -rf "$tmp"; return 1; fi
    grep -Fq 'sk-a...ghij' "$out" || { echo "self-test FAIL: expected masked match sk-a...ghij" >&2; sed -n '1,10p' "$out" >&2; rm -rf "$tmp"; return 1; }

    # 4) secret in a commit message.
    git -C "$tmp" commit -q --allow-empty -m "debug: try token $fake"
    if ( cd "$tmp" && "$SELF" --gate ) > "$out" 2>&1; then
        echo "self-test FAIL: commit-message secret must fail the gate" >&2; rm -rf "$tmp"; return 1
    fi
    grep -Fq 'DIRTY  commit-msg' "$out" || { echo "self-test FAIL: expected commit-msg DIRTY" >&2; sed -n '1,10p' "$out" >&2; rm -rf "$tmp"; return 1; }

    # 5) untracked working-tree file carrying three shapes that must ALL
    #    be DIRTY: a Bearer secret; a real secret on a placeholder-LOOKING
    #    line (RE_HARD must win over the placeholder->INFO rule); and a
    #    real secret whose prefix is a whitelisted fixture (whole-token
    #    stripping must NOT strip the embedded fixture occurrence).
    embedded="sk-ant-test-key-12345678""9012345678901234"
    printf 'Authorization: Bearer %s\npassword: your-key-%s\napi_key: %s\n' \
        "$fake" "$fake" "$embedded" > "$tmp/scratch.txt"
    if ( cd "$tmp" && "$SELF" --gate ) > "$out" 2>&1; then
        echo "self-test FAIL: untracked tree secrets must fail the gate" >&2; rm -rf "$tmp"; return 1
    fi
    [ "$(grep -Fc 'DIRTY  tree' "$out")" = "3" ] || { echo "self-test FAIL: expected exactly 3 tree DIRTY lines (Bearer; placeholder-shaped real secret; fixture-prefixed real secret)" >&2; sed -n '1,20p' "$out" >&2; rm -rf "$tmp"; return 1; }
    if grep -Fq -- "$fake" "$out"; then echo "self-test FAIL: full secret value printed (masking broken)" >&2; rm -rf "$tmp"; return 1; fi

    rm -rf "$tmp"
    echo "secrets-audit self-test: OK"
    return 0
}

# ---- main ----
if [ "$SELF_TEST" -eq 1 ]; then
    self_test
    exit $?
fi

TOP="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "error: not a git repository" >&2; exit 2; }
cd "$TOP"
if scan; then
    exit 0
fi
exit 1
