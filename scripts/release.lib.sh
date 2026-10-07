# Pure release logic, sourced by scripts/release.sh and scripts/test-release.sh.
# No git/network/file-mutation side effects in this file.

valid_version() {  # <version>
    printf '%s' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'
}

bump_version() {   # <current> <patch|minor|major|x.y.z>  -> prints new version
    local cur="$1" mode="$2"
    valid_version "$cur" || { echo "invalid current version: $cur" >&2; return 1; }
    if valid_version "$mode"; then
        printf '%s\n' "$mode"; return 0
    fi
    local IFS='.'; set -- $cur; unset IFS
    case "$mode" in
        patch) echo "$1.$2.$((10#$3 + 1))" ;;
        minor) printf '%s.%0*d.%0*d\n' "$1" "${#2}" "$((10#$2 + 1))" "${#3}" 0 ;;
        major) printf '%0*d.%0*d.%0*d\n' "${#1}" "$((10#$1 + 1))" "${#2}" 0 "${#3}" 0 ;;
        *) echo "invalid bump mode: $mode (use patch|minor|major|x.y.z)" >&2; return 1 ;;
    esac
}

render_changelog_entry() {  # <version> <date> ; commit subjects on stdin -> section on stdout
    local ver="$1" date="$2" body
    body="$(sed 's/^/- /')"
    [ -n "$body" ] || body="- Version bump"
    printf '## [Unreleased]\n\n## [%s] - %s\n\n%s\n' "$ver" "$date" "$body"
}

changelog_update() {  # <file> <version> <date> <body-file>
    local file="$1" ver="$2" date="$3" body="$4" tmp
    tmp="$(mktemp)"
    awk -v ver="## [$ver] - $date" -v bodyfile="$body" '
        !ins && $0 == "## [Unreleased]" {
            print "## [Unreleased]"; print ""; print ver; print "";
            while ((getline line < bodyfile) > 0) print line;
            close(bodyfile); ins=1; next
        }
        { print }
    ' "$file" > "$tmp" && mv "$tmp" "$file"
}
