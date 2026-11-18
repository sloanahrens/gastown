package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
)

// preflightCall records how a fake check was called.
type preflightCall struct {
	townConfig, handshake int
}

// fakeTownConfigCheck is the town config check as a fake: it records its call
// and answers with err, so a test runs no town config load.
func fakeTownConfigCheck(calls *preflightCall, err error) func(string) error {
	return func(string) error {
		calls.townConfig++
		return err
	}
}

// fakeBDHandshake is the bd handshake as a fake: it records its call and
// answers with err, so a test runs no bd.
func fakeBDHandshake(calls *preflightCall, err error) func(context.Context, string) error {
	return func(context.Context, string) error {
		calls.handshake++
		return err
	}
}

// fakePreflightChecks are both checks as fakes.
func fakePreflightChecks(calls *preflightCall, townConfigErr, handshakeErr error) preflightChecks {
	return preflightChecks{
		townConfig:  fakeTownConfigCheck(calls, townConfigErr),
		bdHandshake: fakeBDHandshake(calls, handshakeErr),
	}
}

// writeUnparseableTownConfig writes a mayor/town.json under townRoot that does
// not parse, so a test runs the real town config check against a broken town.
func writeUnparseableTownConfig(t *testing.T, townRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// failedPreflightResults lists the names of the results that failed.
func failedPreflightResults(results []PreflightResult) []string {
	var failed []string
	for _, result := range results {
		if result.Err != nil {
			failed = append(failed, result.Name)
		}
	}
	return failed
}

// TestPreRollGuardResults drives every combination of the two checks' outcomes
// and pins that both always run and every result is reported in run order, so
// one check's failure cannot hide another's.
func TestPreRollGuardResults(t *testing.T) {
	t.Parallel()
	errConfig := errors.New("mayor/town.json: invalid JSON")
	errHandshake := errors.New("the bd on PATH is not a fork build")
	tests := []struct {
		name         string
		configErr    error
		handshakeErr error
		wantFailed   []string
	}{
		{name: "both pass"},
		{name: "town config fails", configErr: errConfig, wantFailed: []string{preflightNameTownConfig}},
		{name: "handshake fails", handshakeErr: errHandshake, wantFailed: []string{preflightNameBDHandshake}},
		{name: "both fail", configErr: errConfig, handshakeErr: errHandshake,
			wantFailed: []string{preflightNameTownConfig, preflightNameBDHandshake}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls preflightCall
			checks := fakePreflightChecks(&calls, tt.configErr, tt.handshakeErr)

			results := runPreflightChecksWith(context.Background(), "/town", checks)

			if len(results) != 2 {
				t.Fatalf("results = %d, want one per check (2): %+v", len(results), results)
			}
			if results[0].Name != preflightNameTownConfig || results[1].Name != preflightNameBDHandshake {
				t.Errorf("result names = %q, %q, want %q, %q",
					results[0].Name, results[1].Name, preflightNameTownConfig, preflightNameBDHandshake)
			}
			if got := failedPreflightResults(results); !reflect.DeepEqual(got, tt.wantFailed) {
				t.Errorf("failed results = %v, want %v", got, tt.wantFailed)
			}
			if !errors.Is(results[0].Err, tt.configErr) || !errors.Is(results[1].Err, tt.handshakeErr) {
				t.Errorf("result errors = %v, %v, want %v, %v",
					results[0].Err, results[1].Err, tt.configErr, tt.handshakeErr)
			}
			if calls.townConfig != 1 || calls.handshake != 1 {
				t.Errorf("checks ran town-config %d, bd-handshake %d times, want each once even after a failure",
					calls.townConfig, calls.handshake)
			}
		})
	}
}

// TestPreRollGuardBrokenTownConfigStillRunsHandshake runs the production town
// config check against a town whose mayor/town.json does not parse, and pins
// that the handshake still runs and its result is reported (gt-0mwjns).
func TestPreRollGuardBrokenTownConfigStillRunsHandshake(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeUnparseableTownConfig(t, townRoot)
	var calls preflightCall
	checks := preflightChecks{townConfig: daemon.CheckTownConfig, bdHandshake: fakeBDHandshake(&calls, nil)}

	results := runPreflightChecksWith(context.Background(), townRoot, checks)

	if len(results) != 2 {
		t.Fatalf("results = %d, want one per check (2): %+v", len(results), results)
	}
	if results[0].Name != preflightNameTownConfig || results[0].Err == nil {
		t.Fatalf("town-config result = %+v, want a failure for the unparseable mayor/town.json", results[0])
	}
	if !strings.Contains(results[0].Err.Error(), "mayor/town.json") {
		t.Errorf("town-config error = %v, want it to name mayor/town.json", results[0].Err)
	}
	if calls.handshake != 1 {
		t.Errorf("bd handshake ran %d times, want once: a broken town config must not skip it", calls.handshake)
	}
	if results[1].Name != preflightNameBDHandshake || results[1].Err != nil {
		t.Errorf("bd-handshake result = %+v, want the passing handshake still reported", results[1])
	}
}

// TestPreRollGuardUnwiredCheckFailsClosed pins that a check the preflight has
// no function for is reported as a failure, so an unwired check never reads as
// a pass.
func TestPreRollGuardUnwiredCheckFailsClosed(t *testing.T) {
	t.Parallel()
	results := runPreflightChecksWith(context.Background(), "/town", preflightChecks{})

	if len(results) != 2 {
		t.Fatalf("results = %d, want one per check (2): %+v", len(results), results)
	}
	if got, want := failedPreflightResults(results), []string{preflightNameTownConfig, preflightNameBDHandshake}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failed results = %v, want %v", got, want)
	}
	for _, result := range results {
		if !strings.Contains(result.Err.Error(), result.Name) {
			t.Errorf("%s error = %v, want it to name the unwired check", result.Name, result.Err)
		}
	}
}

// TestPreRollGuardProductionChecksAreWired pins that the production struct
// wires both checks, so neither is reported as unwired, and that its
// town-config check runs the real town config check: against a town whose
// mayor/town.json does not parse it must fail naming the file.
func TestPreRollGuardProductionChecksAreWired(t *testing.T) {
	t.Parallel()
	if townPreflightChecks.townConfig == nil {
		t.Fatal("the preflight wires no town-config check, so it reports the check as unwired and fails")
	}
	if townPreflightChecks.bdHandshake == nil {
		t.Fatal("the preflight wires no bd-handshake check, so it reports the check as unwired and fails")
	}

	townRoot := t.TempDir()
	writeUnparseableTownConfig(t, townRoot)

	if err := townPreflightChecks.townConfig(townRoot); err == nil || !strings.Contains(err.Error(), "mayor/town.json") {
		t.Errorf("the production town-config check = %v, want the real check's failure naming mayor/town.json", err)
	}
}

// preflightCommand runs the built `gt daemon preflight` over the checks and
// town-root lookup passed in and returns its output, the command (for its
// silence flags) and the error, so a test drives the command the binary runs.
func preflightCommand(checks preflightChecks, findTownRoot func() (string, error)) (string, *cobra.Command, error) {
	var out bytes.Buffer
	cmd := newDaemonPreflightCmd(checks, findTownRoot)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	return out.String(), cmd, err
}

// TestPreRollGuardFailedResultPrintsFAILAndExitsOne pins that a failed check
// prints its FAIL line with the check's name and error and that the command's
// exit status is 1.
func TestPreRollGuardFailedResultPrintsFAILAndExitsOne(t *testing.T) {
	t.Parallel()
	errHandshake := errors.New("the bd on PATH is not a fork build")
	var calls preflightCall
	checks := fakePreflightChecks(&calls, nil, errHandshake)

	out, cmd, err := preflightCommand(checks, func() (string, error) { return "/town", nil })

	want := "ok  " + preflightNameTownConfig + "\n" +
		"FAIL " + preflightNameBDHandshake + ": the bd on PATH is not a fork build\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	code, silent := IsSilentExit(err)
	if !silent || code != 1 {
		t.Fatalf("error = %v, want a silent exit 1", err)
	}
	if !cmd.SilenceErrors || !cmd.SilenceUsage {
		t.Error("the silent exit would be printed as a cobra error over the per-check lines")
	}
	if calls.townConfig != 1 || calls.handshake != 1 {
		t.Errorf("checks ran town-config %d, bd-handshake %d times, want each once",
			calls.townConfig, calls.handshake)
	}
}

// TestPreRollGuardAllPassingPrintsOkAndExitsZero pins that passing checks each
// print an ok line and that the command's exit status is 0.
func TestPreRollGuardAllPassingPrintsOkAndExitsZero(t *testing.T) {
	t.Parallel()
	var calls preflightCall
	checks := fakePreflightChecks(&calls, nil, nil)

	out, _, err := preflightCommand(checks, func() (string, error) { return "/town", nil })

	want := "ok  " + preflightNameTownConfig + "\n" + "ok  " + preflightNameBDHandshake + "\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if err != nil {
		t.Errorf("error = %v, want nil for a passing preflight", err)
	}
}

// TestPreRollGuardTownLookupFailureIsNotACheckFailure pins that a town the
// command cannot find prints no check line and fails with the lookup's own
// error, not a silent exit 1 that reads like a failed check.
func TestPreRollGuardTownLookupFailureIsNotACheckFailure(t *testing.T) {
	t.Parallel()
	findErr := errors.New("no town found from /nowhere")
	var calls preflightCall

	out, cmd, err := preflightCommand(fakePreflightChecks(&calls, nil, nil), func() (string, error) { return "", findErr })

	if out != "" {
		t.Errorf("output = %q, want no check lines when the town root is unknown", out)
	}
	if !errors.Is(err, findErr) {
		t.Fatalf("error = %v, want the town lookup's error %v", err, findErr)
	}
	if _, silent := IsSilentExit(err); silent {
		t.Error("a town lookup failure exited silently as if the exit status were the answer")
	}
	if cmd.SilenceErrors {
		t.Error("the town lookup failure was silenced, so nothing would name it")
	}
	if calls.townConfig != 0 || calls.handshake != 0 {
		t.Errorf("checks ran town-config %d, bd-handshake %d times after the lookup failed, want none",
			calls.townConfig, calls.handshake)
	}
}

// TestPreRollGuardFailedWriteIsNotASilentExit pins that a line the writer
// refused fails the command with that error, so a lost FAIL line is never
// reported as the preflight's own exit status.
func TestPreRollGuardFailedWriteIsNotASilentExit(t *testing.T) {
	t.Parallel()
	var calls preflightCall
	cmd := newDaemonPreflightCmd(fakePreflightChecks(&calls, nil, nil), func() (string, error) { return "/town", nil })
	cmd.SetOut(failingWriter{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	err := cmd.Execute()

	if err == nil {
		t.Fatal("a failed write did not fail the command")
	}
	if _, silent := IsSilentExit(err); silent {
		t.Errorf("a failed write exited silently as %v", err)
	}
	if cmd.SilenceErrors {
		t.Error("a failed write was silenced as if the exit status were the answer")
	}
}
