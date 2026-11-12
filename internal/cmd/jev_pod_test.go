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

	"github.com/steveyegge/gastown/internal/jev"
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

// outsidePodEnv is the environment lookup of a session on the Mac, where
// KUBERNETES_SERVICE_HOST is unset.
func outsidePodEnv() func(string) string {
	return func(string) string { return "" }
}

// jevPodTestTown is a town root the finder resolves, for a test that chdirs
// into it and runs a command's production entry point.
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
//
//testpolicy:allow parallel — the spend, scorecard and conflict-report flag sets are process-wide
func TestJevRequirePodRefusesOutsideThePodAndNamesThePodCommand(t *testing.T) {
	newHealth := func(args ...string) *cobra.Command {
		cmd := newJevHealthCmd(townFinder(""), jevHealthFixedNow, os.Stat, outsidePodEnv())
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage = true
		return cmd
	}
	newDataset := func(args ...string) *cobra.Command {
		cmd := newJevDatasetCmd(jevDatasetDeps{findTownRoot: townFinder(""), getenv: outsidePodEnv()})
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage = true
		return cmd
	}

	t.Run("the message states where the logs live and names the command", func(t *testing.T) {
		err := newHealth().Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
		assert.True(t, strings.HasSuffix(err.Error(), jevPodCommandLine+"health"),
			"the refusal = %q, want it to end with the pod command", err)
	})

	t.Run("the subcommand's flags are the caller's", func(t *testing.T) {
		assert.True(t, strings.HasSuffix(mustExecute(t, newHealth("--json")), jevPodCommandLine+"health --json"))

		got := mustExecute(t, newDataset("--battery", scorecard.FixtureBattery,
			"--question", "needs", "--since", "30d"))
		assert.True(t, strings.HasSuffix(got, jevPodCommandLine+"dataset --battery=fixture --question=needs --since=30d"),
			"the refusal = %q, want the caller's flags in the pod command", got)

		// A flag left at its default is not named: the pod's command takes the
		// same default.
		assert.NotContains(t, got, "--out=")
	})

	t.Run("the other three commands name their own subcommand", func(t *testing.T) {
		require.NoError(t, jevSpendCmd.ParseFlags([]string{"--hours", "168", "--usd-per-billion-tokens", "3", "--json"}))
		spendErr := jevSpendRun(jevSpendCmd, outsidePodEnv())
		require.Error(t, spendErr)
		assert.Contains(t, spendErr.Error(), jevPodCommandLine+"spend ")
		assert.Contains(t, spendErr.Error(), "--hours=168")
		assert.Contains(t, spendErr.Error(), "--usd-per-billion-tokens=3")
		assert.Contains(t, spendErr.Error(), "--json")

		require.NoError(t, jevScorecardCmd.ParseFlags([]string{"--battery", scorecard.FixtureBattery}))
		scoreErr := jevScorecardCommand(jevScorecardCmd, outsidePodEnv(), scoreJevBattery, scorecard.Cache{})
		require.Error(t, scoreErr)
		assert.Contains(t, scoreErr.Error(), jevPodCommandLine+"scorecard ")
		assert.Contains(t, scoreErr.Error(), "--battery=fixture")

		require.NoError(t, jevConflictReportCmd.ParseFlags([]string{"--since", "7d", "--json"}))
		var out bytes.Buffer
		conflictErr := jevConflictReportRun(jevConflictReportCmd, &out, townFinder(""),
			"7d", true, jevConflictNow, outsidePodEnv())
		require.Error(t, conflictErr)
		assert.Contains(t, conflictErr.Error(), jevPodCommandLine+"conflict-report ")
		assert.Contains(t, conflictErr.Error(), "--since=7d")
		assert.Contains(t, conflictErr.Error(), "--json")
		assert.Zero(t, out.Len())
	})

	t.Run("inside the pod the guard passes", func(t *testing.T) {
		cmd := newJevHealthCmd(townFinder(""), jevHealthFixedNow, os.Stat, inPodEnv())
		assert.NoError(t, jevRequirePod(inPodEnv(), cmd))
	})

	t.Run("an unwired lookup refuses rather than reading the copy", func(t *testing.T) {
		cmd := newJevHealthCmd(townFinder(""), jevHealthFixedNow, os.Stat, nil)
		err := jevRequirePod(nil, cmd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
	})
}

// mustExecute runs cmd and returns the refusal an outside-pod run must exit
// with.
func mustExecute(t *testing.T, cmd *cobra.Command) string {
	t.Helper()
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
	return err.Error()
}

// TestJevHealthSpendAndScorecardRefuseOutsideThePodBeforeReadingAnyFile drives
// each command's production entry point on an outside-pod environment, with a
// town whose logs cannot be read, so reaching a read would return a different
// error.
//
//testpolicy:allow parallel — the cwd and the spend and scorecard flag sets are process-wide
func TestJevHealthSpendAndScorecardRefuseOutsideThePodBeforeReadingAnyFile(t *testing.T) {
	t.Run("health", func(t *testing.T) {
		townCalled := false
		cmd := newJevHealthCmd(func() (string, error) {
			townCalled = true
			return "", errors.New("the town lookup must not run")
		}, jevHealthFixedNow, os.Stat, outsidePodEnv())
		cmd.SetArgs([]string{"--json"})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage = true

		err := cmd.Execute()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
		assert.False(t, townCalled, "the guard refused after the town lookup")
		assert.Zero(t, out.Len(), "the refusal wrote to stdout")
	})

	t.Run("spend", func(t *testing.T) {
		town := jevPodTestTown(t)
		// A directory where the ledger belongs: a command that read the ledger
		// would fail naming it instead of refusing.
		require.NoError(t, os.MkdirAll(jev.UsageLedgerPath(town), 0o700))
		//testpolicy:allow no-chdir — the cwd is the input under test: jevSpendRun finds the town from it
		t.Chdir(town)
		require.NoError(t, jevSpendCmd.ParseFlags([]string{"--usd-per-billion-tokens", "3"}))
		var out bytes.Buffer
		jevSpendCmd.SetOut(&out)

		err := jevSpendRun(jevSpendCmd, outsidePodEnv())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
		assert.NotContains(t, err.Error(), "usage ledger")
		assert.Zero(t, out.Len(), "the refusal wrote to stdout")
	})

	t.Run("scorecard", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeScorecardFixture(t, town)
		logPath := scorecard.FixtureLogPath(town)
		require.NoError(t, os.Remove(logPath))
		// A directory where the battery's log belongs: a command that read it
		// would fail naming it instead of refusing.
		require.NoError(t, os.Mkdir(logPath, 0o700))
		//testpolicy:allow no-chdir — the cwd is the input under test: jevScorecardCommand finds the town from it
		t.Chdir(town)
		require.NoError(t, jevScorecardCmd.ParseFlags([]string{"--battery", scorecard.FixtureBattery, "--json"}))
		var out bytes.Buffer
		jevScorecardCmd.SetOut(&out)

		err := jevScorecardCommand(jevScorecardCmd, outsidePodEnv(), scoreJevBattery, scorecard.Cache{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
		assert.Zero(t, out.Len(), "the refusal wrote to stdout")
	})
}

// TestJevConflictReportAndDatasetRefuseOutsideThePodBeforeReadingAnyFile holds
// the same rule for the two commands whose town lookup is injected.
//
//testpolicy:allow parallel — the conflict-report command's flag set is process-wide
func TestJevConflictReportAndDatasetRefuseOutsideThePodBeforeReadingAnyFile(t *testing.T) {
	t.Run("conflict-report", func(t *testing.T) {
		townCalled := false
		findTown := func() (string, error) {
			townCalled = true
			return "", errors.New("the town lookup must not run")
		}
		var out bytes.Buffer

		err := jevConflictReportRun(jevConflictReportCmd, &out, findTown, "", true, jevConflictNow, outsidePodEnv())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
		assert.False(t, townCalled, "the guard refused after the town lookup")
		assert.Zero(t, out.Len(), "the refusal wrote to stdout")
	})

	t.Run("dataset", func(t *testing.T) {
		townCalled := false
		cmd := newJevDatasetCmd(jevDatasetDeps{
			findTownRoot: func() (string, error) {
				townCalled = true
				return "", errors.New("the town lookup must not run")
			},
			getenv: outsidePodEnv(),
		})
		cmd.SetArgs([]string{"--all"})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage = true

		err := cmd.Execute()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
		assert.False(t, townCalled, "the guard refused after the town lookup")
		assert.Zero(t, out.Len(), "the refusal wrote to stdout")
	})
}

// TestJevCommandsRunUnchangedInsideThePod runs all five commands with
// KUBERNETES_SERVICE_HOST set: each reaches its normal path and no refusal
// appears.
//
//testpolicy:allow parallel — the cwd and the spend and scorecard flag sets are process-wide
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
		//testpolicy:allow no-chdir — the cwd is the input under test: jevSpendRun finds the town from it
		t.Chdir(town)
		require.NoError(t, jevSpendCmd.ParseFlags([]string{"--hours", "1", "--usd-per-billion-tokens", "3", "--json"}))
		var out bytes.Buffer
		jevSpendCmd.SetOut(&out)

		require.NoError(t, jevSpendRun(jevSpendCmd, inPodEnv()))

		assert.Contains(t, out.String(), "triage")
	})

	t.Run("scorecard", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeScorecardFixture(t, town)
		//testpolicy:allow no-chdir — the cwd is the input under test: jevScorecardCommand finds the town from it
		t.Chdir(town)
		require.NoError(t, jevScorecardCmd.ParseFlags([]string{"--battery", scorecard.FixtureBattery, "--json=false"}))
		var out bytes.Buffer
		jevScorecardCmd.SetOut(&out)

		require.NoError(t, jevScorecardCommand(jevScorecardCmd, inPodEnv(), scoreJevBattery, scorecard.Cache{}))

		assert.Contains(t, out.String(), scorecard.FixtureBattery)
	})

	t.Run("conflict-report", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeJevConflictLog(t, town, jevConflictLogLine(t, "a", "b", jevConflictNow.Add(-time.Hour), 0.9))
		// The landings file exists and records no landing, so the pair is
		// unlabeled and the report needs no clone.
		require.NoError(t, os.WriteFile(jevConflictLandingsPath(t, town), nil, 0o644))
		var out bytes.Buffer

		require.NoError(t, jevConflictReportRun(jevConflictReportCmd, &out, townFinder(town), "", false, jevConflictNow, inPodEnv()))

		assert.Contains(t, out.String(), "0.6")
	})

	t.Run("dataset", func(t *testing.T) {
		town := jevPodTestTown(t)
		writeScorecardFixture(t, town)
		outDir := filepath.Join(town, "dataset")
		cmd := newJevDatasetCmd(jevDatasetDeps{findTownRoot: townFinder(town), getenv: inPodEnv()})
		cmd.SetArgs([]string{"--battery", scorecard.FixtureBattery, "--question", "needs", "--out", outDir})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage = true

		require.NoError(t, cmd.Execute())

		assert.Contains(t, out.String(), "wrote")
		_, err := os.Stat(filepath.Join(outDir, "fixture.needs.csv"))
		assert.NoError(t, err)
	})
}

// TestJevRefusalKeepsJSONStdoutEmptyAndExitsOne holds the --json contract: the
// refusal is a plain error with exit status 1, and no JSON document reaches
// stdout.
//
//testpolicy:allow parallel — the spend, scorecard and conflict-report flag sets are process-wide
func TestJevRefusalKeepsJSONStdoutEmptyAndExitsOne(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T) (string, error)
	}{
		{"health", func(t *testing.T) (string, error) {
			cmd := newJevHealthCmd(townFinder(""), jevHealthFixedNow, os.Stat, outsidePodEnv())
			cmd.SetArgs([]string{"--json"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			return out.String(), cmd.Execute()
		}},
		{"spend", func(t *testing.T) (string, error) {
			require.NoError(t, jevSpendCmd.ParseFlags([]string{"--usd-per-billion-tokens", "3", "--json"}))
			var out bytes.Buffer
			jevSpendCmd.SetOut(&out)
			return out.String(), jevSpendRun(jevSpendCmd, outsidePodEnv())
		}},
		{"scorecard", func(t *testing.T) (string, error) {
			require.NoError(t, jevScorecardCmd.ParseFlags([]string{"--battery", scorecard.FixtureBattery, "--json"}))
			var out bytes.Buffer
			jevScorecardCmd.SetOut(&out)
			return out.String(), jevScorecardCommand(jevScorecardCmd, outsidePodEnv(), scoreJevBattery, scorecard.Cache{})
		}},
		{"conflict-report", func(t *testing.T) (string, error) {
			var out bytes.Buffer
			err := jevConflictReportRun(jevConflictReportCmd, &out, townFinder(""),
				"", true, jevConflictNow, outsidePodEnv())
			return out.String(), err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.run(t)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the Jev logs live in the daemon pod")
			assert.Empty(t, strings.TrimSpace(out), "the refusal wrote a document to stdout")
			assert.Equal(t, 1, exitCodeForError(err))
		})
	}
}
