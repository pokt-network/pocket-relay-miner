#!/usr/bin/env bash
#
# The docs name the version an operator installs: an image tag, a release
# tarball. This keeps those pins from going stale.
#
#   doc_versions.sh                 every pin names the same version
#   doc_versions.sh --expect vX.Y.Z  ... and that version is the tag being
#                                    released; otherwise list every doc line
#                                    that still names the old one
#
# The release workflow runs it on every final release tag, before any image is
# pushed; it needs git and nothing else. It is not a gate on changes: while
# they are made, the version they will ship under is not known yet. A
# pre-release tag (vX.Y.Z-rc.N) is not checked: the docs keep pointing at the
# last release until the next one ships.
#
# A pin is one of three shapes of THIS product's version; a mention of another
# version (a historical one, another product's) is not a pin. Prose and commands
# that name the release (`git clone --branch vX.Y.Z`, a title) are not pins
# either: --expect finds them by the old version itself.

set -uo pipefail

expect=""
min_pins=10
while [ $# -gt 0 ]; do
    case "$1" in
        --expect) expect="${2:-}"; shift 2 ;;
        --min) min_pins="${2:-}"; shift 2 ;;
        *) echo "usage: $0 [--expect vX.Y.Z] [--min N]" >&2; exit 2 ;;
    esac
done

semver='v[0-9]+\.[0-9]+\.[0-9]+'
pin="pocket-relay-miner:${semver}|pocket-relay-miner/releases/download/${semver}/|pocket-relay-miner_${semver}_"

# Tracked files only, so scratch and build output never count. .github holds
# the workflows' own templates; docs/benchmarks holds what an old release
# measured, which stays true of that release.
scanned() {
    git ls-files -- '*.md' '*.yaml' '*.yml' '*.service' '*.conf' '*.env.example' |
        grep -vE '^(\.github/|scripts/localonly/|docs/benchmarks/)'
}

files=()
while IFS= read -r f; do files+=("$f"); done < <(scanned)

# file:line:pin, one row per MATCH: a line can hold two (HOST.md's tarball).
pins=$( [ "${#files[@]}" -gt 0 ] && grep -noE "$pin" -- "${files[@]}" /dev/null)
count=$(printf '%s' "$pins" | grep -c . || true)
versions=$(printf '%s\n' "$pins" | grep -oE "$semver" | sort -u)

status=0
if [ "$count" -lt "$min_pins" ]; then
    echo "found $count install pin(s), expected at least $min_pins: the matcher is broken, not the tree" >&2
    exit 1
fi
if [ "$(printf '%s\n' "$versions" | grep -c .)" -ne 1 ]; then
    echo "install pins name more than one version ($(echo $versions)):" >&2
    printf '%s\n' "$pins" | sed 's/^/  /' >&2
    status=1
fi
latest=$(grep -nE 'pocket-relay-miner:latest' -- "${files[@]}" /dev/null)
if [ -n "$latest" ]; then
    echo "docs name a moving tag (AGENTS.md invariant 2: never use one):" >&2
    printf '%s\n' "$latest" | sed 's/^/  /' >&2
    status=1
fi
[ "$status" -eq 0 ] || exit 1

current="$versions"
if [ -n "$expect" ]; then
    case "$expect" in
        *-*) echo "$expect is a pre-release: docs stay on $current"; exit 0 ;;
    esac
    if [ "$current" != "$expect" ]; then
        echo "releasing $expect, but the docs install $current. Every line still naming $current:" >&2
        grep -nwF -- "$current" "${files[@]}" /dev/null | sed 's/^/  /' >&2
        echo "Update them in a PR, then delete and push the tag $expect again (nothing was published)." >&2
        exit 1
    fi
fi
echo "$count install pin(s), all $current"
