# gt-szucfu: delete internal/shell and the doctor's shell-hook checks

## Acceptance -> wiring -> red test
1. internal/shell deleted, nothing imports it
   - Wiring: delete internal/shell/{integration.go,integration_test.go,main_test.go};
     remove the only import in internal/doctor/global_state_check.go.
   - Proof: `go build ./...` fails if an importer remains. No Go test can name a
     deleted package, so build + the grep below are the evidence.
2. Doctor global-state check no longer inspects the shell hook; remaining checks keep tests
   - Wiring: Run keeps state Load / enabled / version / machine id; delete
     hasShellIntegration, checkSourceChain, extractShellVars, isShellVarName,
     unquoteShell, expandShellVars, resolveSourcePaths, replaceUnresolvedVars,
     and all their tests.
   - New Run tests (internal/doctor/global_state_check_test.go): missing state
     (warning + FixHint without the retired --shell), enabled (OK + details),
     disabled (warning), corrupt state (error). A fifth test plants a
     shell-hook.sh and an rc marker under a temp HOME and asserts no shell
     detail appears: red against the old code, green now.
   - state.ShellIntegration / SetShellIntegration have no caller once the
     doctor stops reading the rc chain (install --shell was gt-lpsnp5): delete
     the field, the setter and its half of TestShellIntegrationAndDoctorRun
     (keep RecordDoctorRun coverage). Gate names ./internal/state, so that is
     in scope.
3. git grep 'shell-hook|shell integration|rigs.cache' outside docs/CHANGELOG empty
   - Comment-only rewordings: install_test.go:872, rig_quick_add.go (its caller,
     the hook, is gone), scripts/deep-review_test.sh:588 (tier-name phrase, not
     the hook). Everything else lives in the deleted package/doctor helpers.
4. go build ./... and the Gate tests pass.

## Refusal branches the design needs
- state.Load error that is not ErrNotExist must stay StatusError (never "healthy").
- Missing state stays a warning with a hint that names a command `gt install`
  still has; `--shell` is retired.

## Most likely production failure
State files written by old installs still carry "shell_integration": the JSON
decoder ignores the unknown field on load and drops it on the next save. Nothing
reads it. Verified by the enabled-state test reading a state file that has it.

## Out of scope (report, do not fix)
- gt rig quick-add is now called by nothing; only its stale hook comments are
  touched here.
