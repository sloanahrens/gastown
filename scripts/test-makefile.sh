#!/usr/bin/env bash
# The shell-script tests: the half of the unit tier that `make gate` runs
# beside the Go suite. `make test-makefile` runs them alone. They live here,
# not in the Makefile, so the gate recipe runs them without recursing through
# $(MAKE), which make would execute even under `make -n`
# (scripts/makefile-gate_test.sh reads the gate that way).
set -euo pipefail
cd "$(dirname "$0")/.."

bash scripts/check-install-path_test.sh
bash scripts/install-binary_test.sh
bash scripts/check-deploy-source_test.sh
bash -n scripts/install-gt.sh
bash -n scripts/lib/install-gt-lib.sh
bash scripts/install-gt_test.sh
bash -n scripts/docs-lint.sh
bash scripts/docs-lint_test.sh
bash scripts/flake-sweep_test.sh
bash scripts/gate-vs-load_test.sh
bash -n scripts/test-makefile.sh
bash -n scripts/lint-lock-wait.sh
bash -n scripts/makefile-gate_test.sh
bash scripts/makefile-gate_test.sh
