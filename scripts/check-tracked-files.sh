#!/usr/bin/env bash
#
# Fails when files that must stay local are tracked by git.
#
# The repository tracks exactly one kind of document: documentation written
# for the people who run this software. Planning notes, design specs,
# brainstorms, agent scratch, IDE config and operator-specific data are
# working artifacts. They live on disk, under scripts/localonly/ or an
# ignored directory, and never reach a commit.
#
# Run locally with `make check-tracked-files`; CI runs it on every PR.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

failed=0

# indent prints each line of its stdin under a four-space margin, so a file
# list reads as a block rather than running into the prose around it.
indent() {
    while IFS= read -r line; do
        printf '    %s\n' "$line"
    done
}

# Check 1 catches the failure mode that let .planning/ and .idea/ into the
# repository for months: .gitignore does not apply to files git already
# tracks, so adding a path to .gitignore silently does nothing if the path
# was committed earlier. Any file that is both tracked and ignored is that
# bug, whatever it is named -- which is why this check enumerates nothing.
tracked_and_ignored="$(git ls-files -i -c --exclude-standard)"
if [ -n "$tracked_and_ignored" ]; then
    echo "ERROR: these files are tracked by git, but .gitignore says they should not be:"
    echo ""
    echo "$tracked_and_ignored" | indent
    echo ""
    echo "Adding a path to .gitignore does not untrack a file that is already"
    echo "committed. Untrack them, keeping the files on disk:"
    echo ""
    echo "    git rm -r --cached <path>"
    echo ""
    failed=1
fi

# Check 2 covers working documents that are not ignored yet -- a new agent
# or tool inventing its own directory (.gsd/, docs/plans/, ...) would slip
# past check 1 on its first commit, because nothing ignores it yet.
#
# Patterns are pathspecs, matched against tracked files only. Keep them
# specific: docs/PROTOCOL_SPEC.md is a real user-facing document, so a bare
# *SPEC* pattern would be wrong.
working_doc_paths=(
    '.planning'
    '.claude'
    'docs/superpowers'
    'scripts/localonly'
    ':(glob)**/HANDOFF-*.md'
    ':(glob)**/*-PLAN.md'
    ':(glob)**/*-SUMMARY.md'
)

working_docs="$(git ls-files -- "${working_doc_paths[@]}")"
if [ -n "$working_docs" ]; then
    echo "ERROR: these working documents are tracked, but must stay local:"
    echo ""
    echo "$working_docs" | indent
    echo ""
    echo "Planning notes, specs and agent scratch are not deliverables: they go"
    echo "stale the moment the work they describe ships. Move them under"
    echo "scripts/localonly/ (gitignored) and untrack them:"
    echo ""
    echo "    git rm -r --cached <path>"
    echo ""
    echo "If this is genuinely documentation for users of this software, put it"
    echo "in docs/ under a name that says so."
    echo ""
    failed=1
fi

if [ "$failed" -ne 0 ]; then
    exit 1
fi

echo "check-tracked-files: OK (no local-only files are tracked)"
