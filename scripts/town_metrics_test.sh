#!/usr/bin/env bash
# Unit tests for the town-metrics package (scripts/town_metrics/). Fixtures only: no
# live bd, git, gt or network. Run directly or via the shell tier, which discovers
# every *_test.sh script (the shell tier, internal/shelltier).
set -euo pipefail

cd "$(dirname "$0")"
exec python3 -I -m unittest discover -s . -p 'test_town_metrics_*.py'
