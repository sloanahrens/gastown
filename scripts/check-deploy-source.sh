#!/usr/bin/env bash
# check-deploy-source.sh — refuse to install a gt built from unmerged code.
#
# Precondition of the Makefile's install and safe-install targets (gt-o848l).
# The installed gt is what the daemon and every town session execute, so the
# only code that may reach it is code already merged to main.
#
# Why this replaced the old check-up-to-date recipe: that compared HEAD with
# the branch's OWN upstream and skipped entirely on a detached HEAD or a branch
# with no upstream. A crew branch merely behind main was refused (so
# SKIP_UPDATE_CHECK=1 became routine) while an unmerged branch with no
# upstream installed town-wide — twice with a failing test on it (2026-09-09,
# 2026-09-18).
#
# Rules, run in the repository being built:
#   - HEAD must be an ancestor of (or equal to) the main ref (default
#     origin/main, override with DEPLOY_MAIN_REF). Detached HEADs and branches
#     without an upstream get no exemption.
#   - No tracked modifications outside .beads/ (the build would embed them).
#   - Behind main is a warning, not an error: the Makefile's downgrade check
#     guards against installing something older than what is in force.
#   - ALLOW_UNMERGED=1 overrides, loudly. It exists for an operator
#     deliberately hot-fixing the town, not for routine installs.
#   - SKIP_UPDATE_CHECK=1 only skips the fetch, so a main that moves during a
#     long build cannot fail an install of the commit that was synced
#     (rebuild-gt, gt-9jax). It no longer skips the merged check.
set -euo pipefail

main_ref="${DEPLOY_MAIN_REF:-origin/main}"
remote="${main_ref%%/*}"
branch="${main_ref#*/}"

refuse() {
  echo "ERROR: $1" >&2
  echo "  Deploy merged code only: build from an up-to-date main checkout." >&2
  echo "  To install unmerged code anyway (operator hot-fix): ALLOW_UNMERGED=1" >&2
  exit 1
}

if [[ -z "${SKIP_UPDATE_CHECK:-}" ]]; then
  git fetch "$remote" "$branch" --quiet 2>/dev/null ||
    echo "Warning: could not fetch $main_ref; checking against the local copy" >&2
fi

head="$(git rev-parse --short HEAD)"

if [[ -n "${ALLOW_UNMERGED:-}" ]]; then
  echo "WARNING: ALLOW_UNMERGED=1 — installing $head without checking that it is merged to $main_ref." >&2
  echo "WARNING: every town session will run this UNMERGED build until the next install from main." >&2
  exit 0
fi

if ! git rev-parse --verify --quiet "$main_ref" >/dev/null; then
  refuse "cannot resolve $main_ref, so cannot verify that $head is merged"
fi

if ! git merge-base --is-ancestor HEAD "$main_ref"; then
  refuse "HEAD $head is not merged into $main_ref"
fi

if [[ -n "$(git status --porcelain --untracked-files=no -- . ':(exclude).beads')" ]]; then
  refuse "tracked files outside .beads/ are modified; the build would embed unmerged changes"
fi

behind="$(git rev-list --count HEAD.."$main_ref")"
if [[ "$behind" -gt 0 ]]; then
  echo "Warning: HEAD $head is $behind commit(s) behind $main_ref; installing an older main build." >&2
fi
