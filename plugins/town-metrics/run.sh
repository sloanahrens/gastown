#!/usr/bin/env bash
# The daemon dispatches script plugins, so this wrapper exists and does nothing
# else: the lock, the snapshot and the analysis are Go (gt metrics run, with
# its tests in internal/metrics and internal/cmd). What it does, and when it
# skips: plugin.md beside this script (gt-tctk4, gt-hg64t).

set -euo pipefail

cd "${GT_TOWN_ROOT:-${GT_ROOT:-$HOME/gt}}"
exec gt metrics run
