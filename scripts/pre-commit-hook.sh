#!/usr/bin/env bash
#
# Pre-commit hook: the repository's fast quality gates, run before a commit
# exists rather than after CI has already rejected it.
#
# Install with `make install-hooks`. Bypass a single commit with
# `git commit --no-verify` when you know what you are doing (a WIP commit on a
# scratch branch); CI runs these anyway, so bypassing only defers the answer.
#
# The checks themselves live in scripts/gates/static.sh, which is also what
# `make gate` and CI run. This file is only the git-side wrapper: decide whether
# there is anything to check, delegate, and translate the result into a commit
# refusal. Keeping one implementation matters more than it looks -- the hook and
# the Makefile used to carry separate copies of these checks, and two copies of
# one rule diverge silently.
#
# Design notes, since they are easy to get wrong:
#
#   * These checks REPORT, they do not FIX. A hook that runs `go fmt` rewrites
#     files after git has already snapshotted the index, so the commit records
#     the unformatted version and the fix dangles in the working tree. Telling
#     you to run `make fmt` is slower to type and impossible to get wrong.
#
#   * Tests are deliberately NOT here. The suite takes minutes, and a hook that
#     takes minutes is a hook everyone passes --no-verify to. This is level 1;
#     `scripts/gates/all.sh --level 2` owns the slow ones, and so does CI.

set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1

readonly BOLD=$'\033[1m'
readonly RED=$'\033[0;31m'
readonly YELLOW=$'\033[0;33m'
readonly RESET=$'\033[0m'

# Nothing staged means nothing to check -- e.g. `git commit` during a merge
# resolution with an empty index.
if git diff --cached --quiet; then
    printf '%s==>%s nothing staged, skipping checks\n' "$BOLD" "$RESET"
    exit 0
fi

readonly GATE=scripts/gates/static.sh

# Guarded by existence so the hook still works on a branch that predates the
# gate scripts (checking out an old branch must not break committing on it).
if [ ! -x "$GATE" ]; then
    printf '%s==>%s skip%s %s not present on this branch; commit not checked\n' \
        "$BOLD" "$RESET" "$YELLOW$RESET" "$GATE"
    exit 0
fi

# --staged judges formatting on the files this commit actually contains, while
# build, vet and lint stay whole-tree: a package cannot be compiled in isolation
# from the change that breaks it.
if "$GATE" --staged; then
    exit 0
fi

echo
printf '%sCommit refused.%s Fix the above, or use %sgit commit --no-verify%s to bypass.\n' \
    "$RED" "$RESET" "$BOLD" "$RESET"
exit 1
