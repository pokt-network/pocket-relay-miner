#!/usr/bin/env bash
# check-account-init.sh -- the accounts account-init imports are derived from
# the genesis (tilt/k8s/accounts.star); for the default localnet that derivation
# must give EXACTLY the list that used to be written by hand, order included.
#
# THE EXPECTED SIDE IS NOT HEAD. Once the derivation is committed, HEAD no longer
# carries a literal list, and comparing against it would compare the derivation
# with itself. The baseline is the newest commit whose account-init.Tiltfile
# still carries the literal ACCOUNTS="app1 app2 ..." line, found by walking the
# commits that touched the file -- not by `git log -S`, which only lists commits
# where the COUNT of that prefix changed, and so skips every commit that edited
# the rest of the list (four of them, measured: -S returns a 21-name list while
# the last literal one has 26).
#
# It evaluates the real accounts.star through `tilt alpha tiltfile-result`, which
# only reads files: no Kubernetes call and no local(). It needs tilt, which is
# why it is a script and not part of `go test ./...`.
#
# Usage: scripts/localnet/check-account-init.sh
#   STAR=<accounts.star> CONFIG=<dir with genesis.json + all-keys.yaml> override the inputs.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2

file=tilt/k8s/account-init.Tiltfile
literal='ACCOUNTS="app1 app2'
star="${STAR:-$PWD/tilt/k8s/accounts.star}"
config="${CONFIG:-$PWD/tilt/config}"

baseline=""
for c in $(git log --format=%H -- "$file"); do
    if git show "$c:$file" | grep -qF "$literal"; then
        baseline="$c"
        break
    fi
done
if [ -z "$baseline" ]; then
    echo "FAIL: no commit of $file carries the literal $literal... list -- the baseline this check compares against is gone"
    exit 1
fi
expected="$(git show "$baseline:$file" | grep -o 'ACCOUNTS="[^"]*"' | head -1 | sed 's/^ACCOUNTS="//; s/"$//')"
echo "baseline: $(git log -1 --format='%h %s' "$baseline") ($(wc -w <<<"$expected") accounts)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
printf 'load("%s", "staked_account_names")\nnames = staked_account_names(read_yaml("%s/all-keys.yaml"), read_json("%s/genesis.json"))\nprint("ACCOUNTS=" + " ".join(names))\n' \
    "$star" "$config" "$config" >"$work/Tiltfile"
# -v is what sends the Tiltfile's print() to stderr; without it the list is empty.
if ! tilt alpha tiltfile-result -v -f "$work/Tiltfile" >/dev/null 2>"$work/err"; then
    echo "FAIL: tilt could not evaluate $star"
    cat "$work/err"
    exit 1
fi
got="$(grep -o 'ACCOUNTS=.*' "$work/err" | head -1 | sed 's/^ACCOUNTS=//')"
if [ -z "$got" ]; then
    echo "FAIL: the derivation printed no accounts"
    exit 1
fi

if [ "$got" = "$expected" ]; then
    echo "PASS: $star derives the same $(wc -w <<<"$got") accounts, in the same order"
    exit 0
fi
echo "FAIL: the derived list differs from the one written by hand (< hand-written, > derived)"
diff <(tr ' ' '\n' <<<"$expected") <(tr ' ' '\n' <<<"$got")
exit 1
