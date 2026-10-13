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
# The release workflow runs it on every release tag, before any image is
# pushed; it needs git and nothing else. It is not a gate on changes: while
# they are made, the version they will ship under is not known yet. On a
# pre-release tag (vX.Y.Z-rc.N) the pins must still agree and name no moving
# tag; only the comparison with the tag is skipped, since the docs keep
# pointing at the last release until the next one ships.
#
# A pin is one of the shapes the docs name THIS product's version in: the
# image, the release download and its tarball, and the two commands that check
# the release out (`git clone --branch vX.Y.Z <this repo>`, `git checkout
# vX.Y.Z`). A mention of another version (a historical one, another product's)
# is not a pin. Prose in no pin shape ("tag vX.Y.Z") is not checked: write a
# version an operator acts on in one of these shapes, and a past version in
# none of them (or under docs/benchmarks).

set -uo pipefail

expect=""
min_pins=10
while [ $# -gt 0 ]; do
    case "$1" in
        --expect | --min)
            # A flag without its value used to loop forever (shift 2 shifts
            # nothing), and a --min that is not a number skipped its check.
            [ $# -ge 2 ] || { echo "usage: $0 [--expect vX.Y.Z] [--min N]" >&2; exit 2; }
            if [ "$1" = --expect ]; then
                expect="$2"
                printf '%s\n' "$expect" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' ||
                    { echo "--expect $expect is not vX.Y.Z or vX.Y.Z-pre" >&2; exit 2; }
            else
                min_pins="$2"
                case "$min_pins" in '' | *[!0-9]*) echo "--min $min_pins is not a number" >&2; exit 2 ;; esac
            fi
            shift 2
            ;;
        *) echo "usage: $0 [--expect vX.Y.Z] [--min N]" >&2; exit 2 ;;
    esac
done

# A pre-release suffix is part of the version: a pin on vX.Y.Z-rc.1 is not a
# pin on vX.Y.Z.
semver='v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?'
pin="pocket-relay-miner:${semver}|pocket-relay-miner/releases/download/${semver}/|pocket-relay-miner_${semver}_"
pin="${pin}|--branch ${semver} https://github.com/pokt-network/pocket-relay-miner|git checkout ${semver}"

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
        # Not -w: the tarball's _vX.Y.Z_ is inside a word. Not a longer
        # version either (vX.Y.Z0).
        grep -nE -- "${current//./\\.}([^0-9]|\$)" "${files[@]}" /dev/null | sed 's/^/  /' >&2
        echo "Update them in a PR, then delete and push the tag $expect again (nothing was published)." >&2
        exit 1
    fi
fi
echo "$count install pin(s), all $current"
