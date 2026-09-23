package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// setupFakePluginTown builds a minimal Gas Town workspace (mayor/town.json)
// with one plugin under <townRoot>/plugins/<name>, chdirs into it so
// getPluginScanner and workspace.FindFromCwdOrError resolve it, and restores
// the original cwd on cleanup. NOTE: callers cannot use t.Parallel() — this
// mutates the process cwd.
func setupFakePluginTown(t *testing.T, name, pluginMD, runScript string) {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(townRoot, "plugins", name)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.md"), []byte(pluginMD), 0644); err != nil {
		t.Fatal(err)
	}
	if runScript != "" {
		if err := os.WriteFile(filepath.Join(pluginDir, "run.sh"), []byte(runScript), 0755); err != nil {
			t.Fatal(err)
		}
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })
}

// setupFakeBD puts a fake `bd` on PATH that logs every invocation and
// answers `list` with listOutput (default "[]") and `create` with a fixed
// id, mirroring the technique in internal/plugin/recording_test.go. Returns
// the log file path.
func setupFakeBD(t *testing.T, listOutput string) string {
	t.Helper()
	if listOutput == "" {
		listOutput = "[]"
	}
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd-args.log")
	bdPath := filepath.Join(binDir, "bd")
	fakeBD := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"$BD_ARGS_LOG\"\n" +
		"case \"$1\" in\n" +
		"  create) printf '{\"id\":\"gt-test-run\"}\\n' ;;\n" +
		"  close) exit 0 ;;\n" +
		"  list) printf '%s' \"$BD_LIST_OUTPUT\" ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bdPath, []byte(fakeBD), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_ARGS_LOG", logPath)
	t.Setenv("BD_LIST_OUTPUT", listOutput)
	return logPath
}

func readBDLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	return string(data)
}

// bdLogHasCall reports whether any logged bd invocation is the named
// subcommand (its first word) — not merely a substring of the arguments, so
// "create" does not spuriously match a "--created-after=..." flag.
func bdLogHasCall(log, subcommand string) bool {
	for _, line := range strings.Split(log, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == subcommand {
			return true
		}
	}
	return false
}

// A closed cooldown gate used to be recorded as a `result:skipped` receipt.
// That receipt is itself a type:plugin-run bead, so it counted toward the
// daemon's own CountRunsSince query and pushed the daemon's next scheduled
// dispatch further out on every refused manual invocation (gt-o1z7,
// gt-wisp-1h80 finding 875706a6c45c). A refusal must record nothing.
func TestRunPluginRun_ClosedGateRecordsNoReceipt(t *testing.T) {
	setupFakePluginTown(t, "cooldown-plugin", `+++
name = "cooldown-plugin"
description = "test plugin"

[gate]
type = "cooldown"
duration = "1h"
+++

# Instructions

Do the thing.
`, "")

	// One prior run inside the cooldown window closes the gate.
	logPath := setupFakeBD(t, `[{"id":"gt-prev","title":"Plugin run: cooldown-plugin","created_at":"2020-01-01T00:00:00Z","labels":["type:plugin-run","plugin:cooldown-plugin","result:success"]}]`)

	pluginRunForce = false
	pluginRunDryRun = false
	t.Cleanup(func() { pluginRunForce = false; pluginRunDryRun = false })

	if err := runPluginRun(&cobra.Command{}, []string{"cooldown-plugin"}); err != nil {
		t.Fatalf("runPluginRun: %v", err)
	}

	log := readBDLog(t, logPath)
	if !bdLogHasCall(log, "list") {
		t.Fatalf("expected a gate-check list call, got:\n%s", log)
	}
	if bdLogHasCall(log, "create") {
		t.Fatalf("closed-gate refusal recorded a receipt, extending the daemon's own cooldown window:\n%s", log)
	}
}

// The receipt for a merely-printed run must not read as success: a success
// receipt for work nobody did was the original fail-open bug (gt-o1z7,
// gt-wisp-1h80 finding df02feb0c9ed).
func TestRunPluginRun_PrintedInstructionsRecordsPrintedNotSuccess(t *testing.T) {
	setupFakePluginTown(t, "manual-plugin", `+++
name = "manual-plugin"
description = "test plugin"

[gate]
type = "manual"
+++

# Instructions

Do the thing by hand.
`, "")

	logPath := setupFakeBD(t, "[]")

	pluginRunForce = false
	pluginRunDryRun = false
	t.Cleanup(func() { pluginRunForce = false; pluginRunDryRun = false })

	if err := runPluginRun(&cobra.Command{}, []string{"manual-plugin"}); err != nil {
		t.Fatalf("runPluginRun: %v", err)
	}

	log := readBDLog(t, logPath)
	if !strings.Contains(log, "-l result:printed") {
		t.Fatalf("expected a result:printed receipt, got:\n%s", log)
	}
	if strings.Contains(log, "-l result:success") {
		t.Fatalf("printed-instructions run recorded result:success, the fail-open bug:\n%s", log)
	}
}

// A script-type plugin on a gate the daemon never dispatches (event, in this
// case) used to have no manual trigger at all: `gt plugin run` refused
// outright (gt-wisp-1h80 finding df0b394715a8). It must now actually run
// run.sh and record the real, honest outcome.
func TestRunPluginRun_ScriptPluginRunsAndRecordsHonestResult(t *testing.T) {
	setupFakePluginTown(t, "script-plugin", `+++
name = "script-plugin"
description = "test plugin"

[gate]
type = "event"
on = "startup"

[execution]
type = "script"
+++

# Instructions

(unused for a script plugin)
`, "#!/usr/bin/env bash\nexit 0\n")

	logPath := setupFakeBD(t, "[]")

	pluginRunForce = false
	pluginRunDryRun = false
	t.Cleanup(func() { pluginRunForce = false; pluginRunDryRun = false })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPluginRun(cmd, []string{"script-plugin"}); err != nil {
		t.Fatalf("runPluginRun: %v", err)
	}

	log := readBDLog(t, logPath)
	if !strings.Contains(log, "-l result:success") {
		t.Fatalf("expected run.sh's exit 0 to record result:success, got:\n%s", log)
	}
}

// A failing run.sh must record result:failure and return an error, not a
// silently-swallowed success (gt-o1z7).
func TestRunPluginRun_ScriptPluginFailureRecordsFailure(t *testing.T) {
	setupFakePluginTown(t, "script-plugin-fail", `+++
name = "script-plugin-fail"
description = "test plugin"

[gate]
type = "event"
on = "startup"

[execution]
type = "script"
+++

# Instructions

(unused for a script plugin)
`, "#!/usr/bin/env bash\nexit 1\n")

	logPath := setupFakeBD(t, "[]")

	pluginRunForce = false
	pluginRunDryRun = false
	t.Cleanup(func() { pluginRunForce = false; pluginRunDryRun = false })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runPluginRun(cmd, []string{"script-plugin-fail"})
	if err == nil {
		t.Fatal("expected an error from a failing run.sh, got nil")
	}

	log := readBDLog(t, logPath)
	if !strings.Contains(log, "-l result:failure") {
		t.Fatalf("expected run.sh's exit 1 to record result:failure, got:\n%s", log)
	}
	if strings.Contains(log, "-l result:success") {
		t.Fatalf("failing run.sh recorded result:success:\n%s", log)
	}
}

// gt plugin run refuses a plugin that declares script execution but ships no
// run.sh, rather than silently falling back to the print-instructions path
// (gt-wisp-1h80 finding df0b394715a8: the old check ignored HasRunScript).
func TestRunPluginRun_ScriptWithoutRunShRefuses(t *testing.T) {
	setupFakePluginTown(t, "script-no-runsh", `+++
name = "script-no-runsh"
description = "test plugin"

[gate]
type = "manual"

[execution]
type = "script"
+++

# Instructions
`, "")

	logPath := setupFakeBD(t, "[]")

	pluginRunForce = false
	pluginRunDryRun = false
	t.Cleanup(func() { pluginRunForce = false; pluginRunDryRun = false })

	err := runPluginRun(&cobra.Command{}, []string{"script-no-runsh"})
	if err == nil {
		t.Fatal("expected an error for a script plugin with no run.sh, got nil")
	}

	log := readBDLog(t, logPath)
	if bdLogHasCall(log, "create") {
		t.Fatalf("refusal for a missing run.sh recorded a receipt:\n%s", log)
	}
}
