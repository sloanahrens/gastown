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
bash -n scripts/install-after-merge.sh
bash scripts/install-after-merge_test.sh
bash -n plugins/dolt-log-rotate/run.sh
bash -n plugins/dolt-log-rotate/run_test.sh
bash plugins/dolt-log-rotate/run_test.sh
bash -n plugins/stuck-agent-dog/run.sh
bash -n plugins/stuck-agent-dog/run_test.sh
bash plugins/stuck-agent-dog/run_test.sh
bash -n plugins/compactor-dog/run.sh
bash -n plugins/compactor-dog/run_test.sh
bash plugins/compactor-dog/run_test.sh
bash -n plugins/stuck-work-dog/run.sh
bash -n plugins/stuck-work-dog/run_test.sh
bash plugins/stuck-work-dog/run_test.sh
bash -n plugins/rebuild-gt/run.sh
bash -n plugins/rebuild-gt/run_test.sh
bash plugins/rebuild-gt/run_test.sh
bash -n plugins/gitignore-reconcile/run.sh
bash -n plugins/git-hygiene/run.sh
bash -n plugins/submodule-commit/run.sh
bash -n plugins/submodule-commit/run_test.sh
bash plugins/submodule-commit/run_test.sh
bash -n plugins/rig-list-consumers/run_test.sh
bash plugins/rig-list-consumers/run_test.sh
bash -n plugins/quality-review/run.sh
bash -n plugins/quality-review/run_test.sh
bash plugins/quality-review/run_test.sh
bash -n plugins/seat-refill/run.sh
bash -n plugins/seat-refill/run_test.sh
bash plugins/seat-refill/run_test.sh
bash -n scripts/docs-lint.sh
bash scripts/docs-lint_test.sh
bash -n scripts/test-makefile.sh
bash -n scripts/makefile-gate_test.sh
bash scripts/makefile-gate_test.sh
