#!/usr/bin/env bash
#
# Gate: static checks. Seconds, no cluster, no network.
#
#   gofmt -s · go build · go vet · golangci-lint · tracked-file guard
#
# Usage:
#   scripts/gates/static.sh              # whole tree, both Go modules
#   scripts/gates/static.sh --staged     # gofmt only what this commit stages
#   PKG=miner scripts/gates/static.sh    # narrow build/vet to one package
#
# --staged exists for the pre-commit hook: formatting is judged on the files the
# commit actually contains, while build/vet/lint stay whole-tree because a
# package cannot be compiled in isolation from the change that breaks it.
#
# This repository has TWO Go modules -- the root and tilt/backend-server -- and
# `go build ./...` in the root does not reach the second one. Both are checked
# here, matching what `make lint` and `make fmt` already do.

set -uo pipefail

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

gate_repo_root

readonly BACKEND_DIR="tilt/backend-server"

staged_only=0
for arg in "$@"; do
    case "$arg" in
    --staged) staged_only=1 ;;
    -h | --help)
        sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        printf 'unknown argument: %s\n' "$arg" >&2
        exit 2
        ;;
    esac
done

pkg="$(gate_pkg_target)"

# ---------------------------------------------------------------------------
gate_step "gofmt"
if [ "$staged_only" -eq 1 ]; then
    # Added, copied, modified, renamed -- not deleted, which have nothing left
    # to format. NUL-delimited so a path containing a space survives.
    staged_go=()
    while IFS= read -r -d '' f; do
        case "$f" in
        *.go) staged_go+=("$f") ;;
        esac
    done < <(git diff --cached --name-only --diff-filter=ACMR -z)

    if [ "${#staged_go[@]}" -eq 0 ]; then
        gate_pass "no Go files staged"
    else
        unformatted="$(gofmt -s -l "${staged_go[@]}" 2>/dev/null || true)"
        if [ -n "$unformatted" ]; then
            gate_fail "these staged files are not gofmt'd:"
            gate_detail "$unformatted"
            printf '         run: %smake fmt%s, then stage the result\n' \
                "$GATE_BOLD" "$GATE_RESET"
        else
            gate_pass "staged Go files are formatted"
        fi
    fi
else
    # gofmt over TRACKED files only, never `gofmt -s -l .`: unlike the go
    # command's walker, gofmt DOES descend dot- and underscore-prefixed
    # directories, so gitignored scratch (.claude/worktrees/, an agent's
    # half-edited file, a rescue under scripts/localonly/_rescued/) can turn
    # the gate red -- and the printed remedy (`make fmt` = go fmt ./...,
    # which skips those dirs) can never fix it.
    unformatted="$(git ls-files -z '*.go' | xargs -0 -r gofmt -s -l 2>/dev/null || true)"
    if [ -n "$unformatted" ]; then
        gate_fail "these files are not gofmt'd:"
        gate_detail "$unformatted"
        printf '         run: %smake fmt%s\n' "$GATE_BOLD" "$GATE_RESET"
    else
        gate_pass "all tracked Go files are formatted"
    fi
fi

# ---------------------------------------------------------------------------
gate_step "go build"
if build_out="$(go build "$pkg" 2>&1)"; then
    gate_pass "root module builds"
else
    gate_fail "the root module does not build:"
    gate_detail "$build_out"
fi

if [ -f "$BACKEND_DIR/go.mod" ]; then
    if build_out="$(cd "$BACKEND_DIR" && go build ./... 2>&1)"; then
        gate_pass "$BACKEND_DIR builds"
    else
        gate_fail "$BACKEND_DIR does not build:"
        gate_detail "$build_out"
    fi
else
    gate_skip "$BACKEND_DIR absent on this branch"
fi

# ---------------------------------------------------------------------------
gate_step "go vet"
if vet_out="$(go vet "$pkg" 2>&1)"; then
    gate_pass "root module vet clean"
else
    gate_fail "go vet (root module):"
    gate_detail "$vet_out"
fi

# Again under the `test` tag, which is NOT a formality: this repository puts
# test-only helpers behind //go:build test, so a plain vet never compiles them.
# Without this pass a change can delete a symbol that only test code uses and
# reach a green level 1 while the suite does not build -- observed, not
# theoretical. Cheap enough to always run, and it fails minutes earlier than the
# test gate would.
if vet_out="$(go vet -tags test "$pkg" 2>&1)"; then
    gate_pass "root module vet clean (-tags test)"
else
    gate_fail "go vet -tags test (root module):"
    gate_detail "$vet_out"
fi

if [ -f "$BACKEND_DIR/go.mod" ]; then
    if vet_out="$(cd "$BACKEND_DIR" && go vet ./... 2>&1)"; then
        gate_pass "$BACKEND_DIR vet clean"
    else
        gate_fail "go vet ($BACKEND_DIR):"
        gate_detail "$vet_out"
    fi
fi

# ---------------------------------------------------------------------------
# .gitignore cannot enforce anything on a path git already tracks, which is
# exactly how .planning/ and .idea/ ended up in the repository. Guarded by
# existence so this gate still runs on branches predating the script.
gate_step "tracked files"
if [ -x ./scripts/check-tracked-files.sh ]; then
    if tracked_out="$(./scripts/check-tracked-files.sh 2>&1)"; then
        gate_pass "no local-only files tracked"
    else
        gate_fail "local-only files are tracked:"
        gate_detail "$tracked_out"
    fi
else
    gate_skip "scripts/check-tracked-files.sh not present on this branch"
fi

# ---------------------------------------------------------------------------
# Last, because it is by far the slowest.
gate_step "golangci-lint"
if ! command -v golangci-lint >/dev/null 2>&1; then
    gate_skip "golangci-lint not installed -- CI will run it"
else
    if lint_out="$(golangci-lint run 2>&1)"; then
        gate_pass "root module lint clean"
    else
        gate_fail "golangci-lint (root module):"
        gate_detail "$lint_out" 30
    fi

    if [ -f "$BACKEND_DIR/go.mod" ]; then
        if lint_out="$(cd "$BACKEND_DIR" && golangci-lint run 2>&1)"; then
            gate_pass "$BACKEND_DIR lint clean"
        else
            gate_fail "golangci-lint ($BACKEND_DIR):"
            gate_detail "$lint_out" 30
        fi
    fi
fi

gate_verdict "static"
