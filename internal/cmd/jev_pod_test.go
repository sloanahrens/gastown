package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// inPodEnv is the environment lookup of a session inside the daemon pod:
// Kubernetes sets KUBERNETES_SERVICE_HOST in every pod.
func inPodEnv() func(string) string {
	return func(name string) string {
		if name == podEnv {
			return "10.96.0.1"
		}
		return ""
	}
}

// outsidePodEnv is the environment lookup of a session outside the daemon pod,
// where KUBERNETES_SERVICE_HOST is unset.
func outsidePodEnv() func(string) string {
	return func(string) string { return "" }
}

// jevGuardedCommands is every gt jev subcommand that reads the town's log
// directory, with a constructor over a town lookup and an environment lookup so
// each test case gets a command of its own. The patrol writers are not here:
// they run in the pod and are named in jevPatrolWriters.
var jevGuardedCommands = []struct {
	name        string
	refusalArgs []string
	newCmd      func(findTown func() (string, error), getenv func(string) string) *cobra.Command
}{
	{"health", []string{"--json"}, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevHealthCmd(findTown, jevHealthFixedNow, os.Stat, getenv)
	}},
	{"spend", []string{"--usd-per-billion-tokens", "3", "--json"}, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevSpendCmd(jevSpendDeps{findTownRoot: findTown, getenv: getenv})
	}},
	{"scorecard", []string{"--battery", scorecard.FixtureBattery, "--json"}, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevScorecardCmd(jevScorecardDeps{findTownRoot: findTown, getenv: getenv})
	}},
	{"conflict-report", []string{"--json"}, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevConflictReportCmd(jevConflictReportDeps{findTownRoot: findTown, getenv: getenv})
	}},
	{"dataset", []string{"--all"}, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevDatasetCmd(jevDatasetDeps{findTownRoot: findTown, getenv: getenv})
	}},
	{"noise", []string{"--json"}, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevNoiseCmd(jevNoiseDeps{findTownRoot: findTown, getenv: getenv})
	}},
	{"requests", nil, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevRequestsCmd(jevRequestsDeps{findTownRoot: findTown, getenv: getenv})
	}},
	{"scrubcheck", []string{"--json"}, func(findTown func() (string, error), getenv func(string) string) *cobra.Command {
		return newJevScrubcheckCmd(jevScrubcheckDeps{findTownRoot: findTown, getenv: getenv})
	}},
}

// jevPatrolWriters is every gt jev subcommand that writes the logs instead of
// reading them. They run as the daemon's patrols inside the pod, so they are
// not guarded. A new writer added to gt jev must be listed here, and a new
// reader must be in jevGuardedCommands, or the enumeration test fails.
var jevPatrolWriters = map[string]bool{
	"attention":      true,
	"dedupe":         true,
	"dispatch":       true,
	"landing":        true,
	"submission":     true,
	"review-targets": true,
}

// jevPodTestTown is a town root a command's tests resolve through their town
// lookup, with no chdir and no other file: a command that needs logs writes
// them under it.
func jevPodTestTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(town, "mayor"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o600))
	return town
}

// jevPodCommandLine is the kubectl exec line every refusal starts with.
const jevPodCommandLine = "kubectl --context colima-gt-control -n gt exec deploy/daemon -c daemon -- gt jev "

// TestJevRequirePodRefusesOutsideThePodAndNamesThePodCommand holds the refusal
// text: the logs' home, and the pod command that runs the same subcommand with
// the flags the caller gave.
func TestJevRequirePodRefusesOutsideThePodAndNamesThePodCommand(t *testing.T) {
	t.Run("the message states where the logs live and names the command", func(t *testing.T) {
		err := runJevPodCase(t, "health", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
		assert.True(t, strings.HasSuffix(err.Error(), jevPodCommandLine+"health"),
			"the refusal = %q, want it to end with the pod command", err)
	})

	t.Run("the subcommand's flags are the caller's", func(t *testing.T) {
		assert.True(t, strings.HasSuffix(runJevPodCase(t, "health", []string{"--json"}).Error(),
			jevPodCommandLine+"health --json"))

		got := runJevPodCase(t, "dataset", []string{"--battery", scorecard.FixtureBattery,
			"--question", "needs", "--since", "30d"}).Error()
		assert.True(t, strings.HasSuffix(got, jevPodCommandLine+"dataset --battery=fixture --question=needs --since=30d"),
			"the refusal = %q, want the caller's flags in the pod command", got)

		// A flag left at its default is not named: the pod's command takes the
		// same default.
		assert.NotContains(t, got, "--out=")
	})

	t.Run("every log-reading subcommand names its own name", func(t *testing.T) {
		for _, c := range jevGuardedCommands {
			t.Run(c.name, func(t *testing.T) {
				err := runJevPodCase(t, c.name, nil)
				require.Error(t, err)
				assert.True(t, strings.HasSuffix(err.Error(), jevPodCommandLine+c.name),
					"the refusal = %q, want it to end with gt jev %s", err, c.name)
			})
		}
	})

	t.Run("inside the pod the guard passes", func(t *testing.T) {
		cmd := newJevHealthCmd(townFinder(""), jevHealthFixedNow, os.Stat, inPodEnv())
		assert.NoError(t, jevRequirePod(inPodEnv(), cmd))
	})

	t.Run("an unwired lookup refuses rather than reading a copy", func(t *testing.T) {
		cmd := newJevHealthCmd(townFinder(""), jevHealthFixedNow, os.Stat, nil)
		err := jevRequirePod(nil, cmd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
	})
}

// runJevPodCase builds the named guarded command for an outside-pod session and
// executes it with args, returning the refusal it must fail with.
func runJevPodCase(t *testing.T, name string, args []string) error {
	t.Helper()
	cmd := jevGuardedCommand(t, name, townFinder(""), outsidePodEnv())
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	return cmd.Execute()
}

// jevGuardedCommand builds the named guarded command over findTown and getenv.
func jevGuardedCommand(t *testing.T, name string, findTown func() (string, error), getenv func(string) string) *cobra.Command {
	t.Helper()
	for _, c := range jevGuardedCommands {
		if c.name == name {
			return c.newCmd(findTown, getenv)
		}
	}
	t.Fatalf("no guarded command named %q", name)
	return nil
}

// TestJevLogReadingCommandsRefuseOutsideThePodBeforeReadingAnyFile drives each
// guarded command's production entry point on an outside-pod environment with a
// town lookup that fails, so a command that reached its lookup would return a
// different error.
func TestJevLogReadingCommandsRefuseOutsideThePodBeforeReadingAnyFile(t *testing.T) {
	for _, c := range jevGuardedCommands {
		t.Run(c.name, func(t *testing.T) {
			townCalled := false
			cmd := c.newCmd(func() (string, error) {
				townCalled = true
				return "", errors.New("the town lookup must not run")
			}, outsidePodEnv())
			cmd.SetArgs(c.refusalArgs)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true

			err := cmd.Execute()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
			assert.False(t, townCalled, "the guard refused after the town lookup")
			assert.Zero(t, out.Len(), "the refusal wrote to stdout")
		})
	}
}

// TestEveryJevSubcommandIsGuardedOrListedAsAPatrolWriter fails when a gt jev
// subcommand is neither in the guarded list nor listed as a patrol writer, so a
// new reader cannot ship unguarded. It reads the subcommand names only; no test
// parses a flag on the shared command.
func TestEveryJevSubcommandIsGuardedOrListedAsAPatrolWriter(t *testing.T) {
	guarded := map[string]bool{}
	for _, c := range jevGuardedCommands {
		guarded[c.name] = true
	}
	patrol := jevPatrolWriters

	seen := map[string]bool{}
	for _, sub := range jevCmd.Commands() {
		name := sub.Name()
		// cobra adds its own help command to a command it executes.
		if name == "help" || name == "completion" {
			continue
		}
		seen[name] = true
		switch {
		case guarded[name] && patrol[name]:
			t.Errorf("gt jev %s is both guarded and listed as a patrol writer; it can be only one", name)
		case !guarded[name] && !patrol[name]:
			t.Errorf("gt jev %s is neither guarded by jevRequirePod nor listed as a patrol writer: guard it, or list it in jevPatrolWriters", name)
		}
	}
	for name := range guarded {
		if !seen[name] {
			t.Errorf("the guarded list names %q, which gt jev does not register", name)
		}
	}
	for name := range patrol {
		if !seen[name] {
			t.Errorf("the patrol-writer list names %q, which gt jev does not register", name)
		}
	}
	assert.Len(t, guarded, 8, "the bead names eight log-reading subcommands")
	assert.Len(t, patrol, 6, "the bead names six patrol writers")
}

// TestJevPodCommandQuotesFlagValues holds the pod line's shell quoting: a value
// holding a space, quote, dollar sign or backtick reaches the pod command as
// one word, and a value the shell reads plainly stays bare.
func TestJevPodCommandQuotesFlagValues(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"one plain word", []string{"--since", "7d"}, "--since=7d"},
		{"a space", []string{"--since", "30 days"}, "--since='30 days'"},
		{"a single quote", []string{"--out", "/tmp/it's here"}, `--out='/tmp/it'\''s here'`},
		{"a dollar sign", []string{"--since", "$HOME"}, "--since='$HOME'"},
		{"a backtick", []string{"--since", "a`b"}, "--since='a`b'"},
		{"a double quote", []string{"--since", `a"b`}, `--since='a"b'`},
		{"a bare bool", []string{"--json"}, "--json"},
		{"a false bool", []string{"--json=false"}, "--json=false"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newJevPodFlagCmd(t, tc.args)
			argv := jevPodArgv(cmd)
			assert.Equal(t, []string{"gt", "jev", "probe", tc.want}, argv)
		})
	}

	t.Run("a flag left at its default is not printed", func(t *testing.T) {
		cmd := newJevPodFlagCmd(t, nil)
		assert.Equal(t, []string{"gt", "jev", "probe"}, jevPodArgv(cmd))
	})
}

// newJevPodFlagCmd builds a command with the flags the quoting cases set, so
// each case parses into fresh flag values of its own.
func newJevPodFlagCmd(t *testing.T, args []string) *cobra.Command {
	t.Helper()
	var since, out string
	var asJSON bool
	cmd := &cobra.Command{Use: "probe"}
	cmd.Flags().StringVar(&since, "since", "14d", "")
	cmd.Flags().StringVar(&out, "out", "", "")
	cmd.Flags().BoolVar(&asJSON, "json", false, "")
	require.NoError(t, cmd.ParseFlags(args))
	return cmd
}

// TestJevCommandsRunUnchangedInsideThePod runs every guarded command with
// KUBERNETES_SERVICE_HOST set: each reaches its normal path and no refusal
// appears.
func TestJevCommandsRunUnchangedInsideThePod(t *testing.T) {
	t.Run("health", func(t *testing.T) {
		now := jevHealthFixedNow()
		out, err := runJevHealthCommand(t, newJevHealthTown(t, now, 5*time.Minute), now, false)
		require.NoError(t, err)
		assert.Contains(t, out, "BATTERY")
	})

	t.Run("spend", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeJevSpendLedger(t, town, jevSpendLine(time.Now().Add(-time.Minute), "triage", 1000, 500))
		var out bytes.Buffer
		cmd := newJevSpendCmd(jevSpendDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--hours", "1", "--usd-per-billion-tokens", "3", "--json"})

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), "triage")
	})

	t.Run("scorecard", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeScorecardFixture(t, town)
		var out bytes.Buffer
		cmd := newJevScorecardCmd(jevScorecardDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--battery", scorecard.FixtureBattery, "--json"})

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), scorecard.FixtureBattery)
	})

	t.Run("conflict-report", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeJevConflictLog(t, town, jevConflictLogLine(t, "a", "b", time.Now().Add(-time.Hour), 0.9))
		// The landings file exists and records no landing, so the pair is
		// unlabeled and the report needs no clone.
		require.NoError(t, os.WriteFile(jevConflictLandingsPath(t, town), nil, 0o644))
		var out bytes.Buffer
		cmd := newJevConflictReportCmd(jevConflictReportDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetOut(&out)

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), "cut 0.6")
	})

	t.Run("dataset", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeScorecardFixture(t, town)
		outDir := filepath.Join(town, "dataset")
		var out bytes.Buffer
		cmd := newJevDatasetCmd(jevDatasetDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--battery", scorecard.FixtureBattery, "--question", "needs", "--out", outDir})

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), "wrote")
		_, err := os.Stat(filepath.Join(outDir, "fixture.needs.csv"))
		assert.NoError(t, err)
	})

	t.Run("noise", func(t *testing.T) {
		town := jevPodTestTown(t)
		var out bytes.Buffer
		cmd := newJevNoiseCmd(jevNoiseDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetOut(&out)

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), "no repeat log")
	})

	t.Run("requests", func(t *testing.T) {
		town := jevPodTestTown(t)
		var out bytes.Buffer
		cmd := newJevRequestsCmd(jevRequestsDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), "no requests logged yet")
	})

	t.Run("scrubcheck", func(t *testing.T) {
		town := jevPodTestTown(t)
		var out bytes.Buffer
		cmd := newJevScrubcheckCmd(jevScrubcheckDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetOut(&out)

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), "no scrub check log")
	})
}

// TestJevRefusalKeepsJSONStdoutEmptyAndExitsOne holds the --json contract: the
// refusal is a plain error with exit status 1, and no JSON document reaches
// stdout. requests has no --json flag, so it is driven plain.
func TestJevRefusalKeepsJSONStdoutEmptyAndExitsOne(t *testing.T) {
	for _, c := range jevGuardedCommands {
		t.Run(c.name, func(t *testing.T) {
			cmd := c.newCmd(townFinder(""), outsidePodEnv())
			cmd.SetArgs(c.refusalArgs)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true

			err := cmd.Execute()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
			assert.Empty(t, strings.TrimSpace(out.String()), "the refusal wrote a document to stdout")
			assert.Equal(t, 1, exitCodeForError(err))
		})
	}
}

// TestJevPodHelpNamesNoHostOrDate holds the help paragraph to the invariant
// that it cannot go stale: no host, install, date or build is named.
func TestJevPodHelpNamesNoHostOrDate(t *testing.T) {
	for _, stale := range []string{"Mac", "cut-over", "2026", "colima", "launchd"} {
		assert.NotContains(t, jevPodHelp, stale)
	}
}

// TestJevRefusalNamesNoHostOrDate holds the refusal to the same invariant.
func TestJevRefusalNamesNoHostOrDate(t *testing.T) {
	err := runJevPodCase(t, "health", nil)
	require.Error(t, err)
	for _, stale := range []string{"Mac", "cut-over", "2026", "launchd"} {
		assert.NotContains(t, err.Error(), stale)
	}
}
