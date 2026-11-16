package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
)

// preflightCall records how a fake check was called.
type preflightCall struct {
	townConfig, handshake int
}

// fakePreflightChecks are the pre-roll checks as fakes: each records its call
// and answers with its error, so a test runs neither a town config load nor bd.
func fakePreflightChecks(calls *preflightCall, ctxErr, handshakeErr error) preflightChecks {
	return preflightChecks{
		townConfig: func(string) error {
			calls.townConfig++
			return ctxErr
		},
		bdHandshake: func(context.Context, string) error {
			calls.handshake++
			return handshakeErr
		},
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
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls preflightCall
	checks := preflightChecks{townConfig: daemon.CheckTownConfig, bdHandshake: fakePreflightChecks(&calls, nil, nil).bdHandshake}

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

// TestPreRollGuardProductionChecksAreWired pins that runPreflightChecks runs
// the real town config check and the real bd handshake, so a check dropped
// from the production struct cannot silently leave the report.
func TestPreRollGuardProductionChecksAreWired(t *testing.T) {
	t.Parallel()
	if townPreflightChecks.townConfig == nil ||
		reflect.ValueOf(townPreflightChecks.townConfig).Pointer() != reflect.ValueOf(daemon.CheckTownConfig).Pointer() {
		t.Error("the preflight's town-config check is not daemon.CheckTownConfig")
	}
	if townPreflightChecks.bdHandshake == nil ||
		reflect.ValueOf(townPreflightChecks.bdHandshake).Pointer() != reflect.ValueOf(bdTownHandshake).Pointer() {
		t.Error("the preflight's bd-handshake check is not bdTownHandshake")
	}
}
