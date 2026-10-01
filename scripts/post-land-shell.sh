#!/usr/bin/env bash
# post-land-shell.sh — the post-land check (merge_queue.post_land_command).
#
# The landing gate (make gate) runs every Go test on the merged tree; what it
# does not run is the shell tier (scripts/tier-sweep.sh shell). Those tests
# exercise shell scripts, plugins, the Makefile and the git hooks against
# stubs; no Go change can move their verdict. Running them after every
# landing cost 3-8 minutes a time, and a pending upgrade restart waits for
# the run while starting no landing pass (gt-8p8h7). So this runs the shell
# tier only when main changed one of its inputs recently, and otherwise
# reports the skip and exits 0.
#
# "Recently" is every first-parent commit on main inside the window, not
# just the landed commit: the landing worker coalesces landings that finish
# while a post-land run is in flight into one run at the newest commit, so
# the inputs of every landing since the last run have to count.
#
# POST_LAND_SHELL_WINDOW (default "2 hours") is that window, in git's
# --since syntax. A history that cannot be read runs the tier: no skip
# without evidence.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

WINDOW=${POST_LAND_SHELL_WINDOW:-2 hours}
INPUTS='^(scripts/|plugins/|\.githooks/|Makefile$|internal/testpolicy/docker\.txt$)'

if ! changed=$(git log --first-parent --since="$WINDOW" --name-only --format= HEAD 2>/dev/null); then
  echo "post-land-shell: cannot read main's history; running the shell tier"
  exec bash scripts/tier-sweep.sh shell
fi
hits=$(printf '%s\n' "$changed" | grep -E "$INPUTS" | sort -u)
if [ -z "$hits" ]; then
  echo "post-land-shell: no shell-test input changed on main in the last $WINDOW; shell tier skipped"
  exit 0
fi
echo "post-land-shell: shell-test inputs changed on main in the last $WINDOW:"
printf '%s\n' "$hits" | head -20 | sed 's/^/  /'
exec bash scripts/tier-sweep.sh shell
