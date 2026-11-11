package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// writeSpendTool writes the ~/.claude tool under home and returns its path.
func writeSpendTool(t *testing.T, home string) string {
	t.Helper()
	tool := filepath.Join(home, ".claude", "tools", "deepseek-spend.py")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return tool
}

// The in-cluster signal sends the spend read to the balance ledger alone. The
// Mac's tool is mounted into the pod and cannot run there, so it is never run,
// even though it is present (gt-30pmxi).
func TestResolveSpendInClusterReadsTheLedgerAndNeverRunsTheTool(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSpendLedger(t, home)
	writeSpendTool(t, home)
	homeFn := func() (string, error) { return home, nil }

	var calls [][]string
	spend, reason := resolveSpendIn(true, "", homeFn, os.Stat, spendFake(&calls, `{"from":"tool"}`), nil)
	if spend == nil || reason != "" {
		t.Fatalf("in cluster with the ledger: spend nil %v, reason %q; want the ledger and no reason", spend == nil, reason)
	}
	var report struct {
		Samples int `json:"samples"`
	}
	out := spend()
	if err := json.Unmarshal(out, &report); err != nil || report.Samples != 2 {
		t.Errorf("in cluster: served %s (err %v), want the ledger report of 2 rows", out, err)
	}
	if len(calls) != 0 {
		t.Errorf("in cluster: the tool ran as %v, want no run", calls)
	}
}

// In the cluster with no ledger there is no spend source, and the tool is not run
// in its place; the reason is the cluster's absence, as on a host with neither.
func TestResolveSpendInClusterWithoutTheLedgerSaysNotInCluster(t *testing.T) {
	t.Parallel()
	withTool := t.TempDir()
	writeSpendTool(t, withTool)
	neither := t.TempDir()
	for name, home := range map[string]string{"tool only": withTool, "neither": neither} {
		homeFn := func() (string, error) { return home, nil }
		spend, reason := resolveSpendIn(true, "", homeFn, os.Stat, spendUnused, nil)
		if spend != nil || reason != dashboard.NotInClusterReason {
			t.Errorf("in cluster, %s: spend nil %v, reason %q; want no source and %q", name, spend == nil, reason, dashboard.NotInClusterReason)
		}
	}
}

// On a Mac the tool comes first. When its run fails, the ledger answers, and the
// why-log names the fallback once, not on every poll that fails the same way.
func TestResolveSpendOnAMacFallsBackToTheLedgerWhenTheToolFails(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSpendLedger(t, home)
	tool := writeSpendTool(t, home)
	homeFn := func() (string, error) { return home, nil }

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	var calls [][]string
	failing := func(_ context.Context, argv []string) ([]byte, error) {
		calls = append(calls, argv)
		return nil, errors.New("exit status 1")
	}
	spend, reason := resolveSpendIn(false, "", homeFn, os.Stat, failing, whyLog)
	if spend == nil || reason != "" {
		t.Fatalf("on a Mac with the tool: spend nil %v, reason %q; want a source and no reason", spend == nil, reason)
	}
	for poll := range 2 {
		var report struct {
			Samples int `json:"samples"`
		}
		out := spend()
		if err := json.Unmarshal(out, &report); err != nil || report.Samples != 2 {
			t.Errorf("poll %d: served %s (err %v), want the ledger report of 2 rows after the tool fails", poll, out, err)
		}
	}
	if want := [][]string{{tool, "--json"}, {tool, "--json"}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("tool runs = %v, want the tool run first on each poll: %v", calls, want)
	}
	if want := []string{"spend tool read failed: no answer; the balance ledger answers"}; !reflect.DeepEqual(logged, want) {
		t.Errorf("why-log = %q, want the fallback logged once: %q", logged, want)
	}
}

// On a Mac the tool that answers is served, and the ledger is not read behind it.
func TestResolveSpendOnAMacServesTheToolWhenItAnswers(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSpendLedger(t, home)
	writeSpendTool(t, home)
	homeFn := func() (string, error) { return home, nil }

	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	var calls [][]string
	spend, reason := resolveSpendIn(false, "", homeFn, os.Stat, spendFake(&calls, `{"from":"tool"}`), whyLog)
	if spend == nil || reason != "" {
		t.Fatalf("on a Mac with the tool: spend nil %v, reason %q; want a source and no reason", spend == nil, reason)
	}
	if got := string(spend()); got != `{"from":"tool"}` {
		t.Errorf("tool answered: served %q, want the tool's JSON", got)
	}
	if len(logged) != 0 {
		t.Errorf("tool answered: why-log = %q, want no line", logged)
	}
}

// On a Mac with the tool and no ledger, a tool that fails is no report, as before:
// there is no ledger to fall back to.
func TestResolveSpendOnAMacWithoutTheLedgerKeepsAFailedToolUnavailable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeSpendTool(t, home)
	homeFn := func() (string, error) { return home, nil }

	failing := func(context.Context, []string) ([]byte, error) { return nil, errors.New("exit status 1") }
	spend, reason := resolveSpendIn(false, "", homeFn, os.Stat, failing, nil)
	if spend == nil || reason != "" {
		t.Fatalf("tool only: spend nil %v, reason %q; want the tool and no reason", spend == nil, reason)
	}
	if out := spend(); out != nil {
		t.Errorf("tool fails with no ledger: served %s, want no report", out)
	}
}

// The Cost panel's ledger read carries the malformed lines it skipped, and a
// ledger that cannot be read is an empty read rather than a day of zeros
// (gt-59v3x).
func TestDashboardSpendHoursFromCarriesTheSkippedLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "balance.log")
	ledger := "2026-10-25T16:05:00 balance=80.00\nnot a row\n"
	if err := os.WriteFile(path, []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	read := dashboardSpendHoursFrom(path)()
	if read.Skipped != 1 {
		t.Errorf("skipped = %d, want the one malformed line", read.Skipped)
	}
	if len(read.Hours) != 24 {
		t.Errorf("hours = %d, want the day's 24", len(read.Hours))
	}
}

func TestDashboardSpendHoursFromReadsAnUnreadableLedgerAsEmpty(t *testing.T) {
	t.Parallel()
	read := dashboardSpendHoursFrom(t.TempDir())() // a directory is not a ledger
	if read.Hours != nil || read.Skipped != 0 {
		t.Errorf("read = %+v, want an empty read", read)
	}
}

// The hub writes the DeepSeek reading where the snapshot, gt tail and gt
// report read it: the town's runtime watch directory, the same one
// internal/metrics and internal/report build from TownRuntimePath (gt-jqtw9).
func TestDashboardWatchDirIsTheTownRuntimeWatchDir(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	want := filepath.Join(constants.TownRuntimePath(town), "watch")
	if got := dashboardWatchDir(town); got != want {
		t.Errorf("the hub writes the spend reading to %q, want %q", got, want)
	}
}
