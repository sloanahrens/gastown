package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

// preflightPassing is a check seam whose every check passed, for a fixture that
// installs a pair without testing the gate.
func preflightPassing(context.Context, string) []PreflightResult {
	return []PreflightResult{
		{Name: preflightNameTownConfig},
		{Name: preflightNameBDHandshake},
	}
}

// preflightDigestInstaller builds the installer digestInstaller wires for a
// digest install over one town root, so a test drives its pre-swap gate. run is
// the check seam; nil builds the installer with no check seam wired.
func preflightDigestInstaller(t *testing.T, townRoot string, run func(context.Context, string) []PreflightResult) *update.DigestInstaller {
	t.Helper()
	path, _ := podPairValuesFile(t)
	seams := updateSeams{
		// The installer is built for a pod target, the update Job's own; the gate
		// is the only thing this test drives, so the kubectl runner is never
		// called.
		PodRun: func(context.Context, string, []string, string, ...string) (string, int, error) {
			return "", 0, nil
		},
		Notifier:  func(string) notify.Notifier { return notifyfake.New() },
		Preflight: run,
	}
	target := podPairTarget()
	installer, err := digestInstaller(seams, townRoot, update.Deps{}, path, &target, io.Discard)
	if err != nil {
		t.Fatalf("digestInstaller: %v", err)
	}
	install, ok := installer.(*update.DigestInstaller)
	if !ok {
		t.Fatalf("installer = %T, want the digest installer", installer)
	}
	if install.PreSwap == nil {
		t.Fatal("the installer has no pre-swap gate: a digest install would swap without the preflight")
	}
	return install
}

// TestPreRollGuardPreSwapNamesEveryFailedCheck holds the gate to what the
// installer records: a passing preflight accepts the pair, and a failing one
// refuses it as the preflight check, naming every failed check and its message.
func TestPreRollGuardPreSwapNamesEveryFailedCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		results []PreflightResult
		// want is empty for a passing preflight; otherwise every string must
		// appear in the refusal.
		want []string
	}{
		{
			name: "a passing preflight accepts the pair",
			results: []PreflightResult{
				{Name: "town-config"},
				{Name: "bd-handshake"},
			},
		},
		{
			name: "a failed check is named with its message",
			results: []PreflightResult{
				{Name: "town-config", Err: errors.New("mayor/town.json does not parse")},
				{Name: "bd-handshake"},
			},
			want: []string{"town-config", "mayor/town.json does not parse"},
		},
		{
			name: "every failed check is named",
			results: []PreflightResult{
				{Name: "town-config", Err: errors.New("mayor/rigs.json does not parse")},
				{Name: "bd-handshake", Err: errors.New("the store answered schema 66, want 67")},
			},
			want: []string{
				"town-config", "mayor/rigs.json does not parse",
				"bd-handshake", "the store answered schema 66, want 67",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			var checkedRoot string
			run := func(_ context.Context, townRoot string) []PreflightResult {
				checkedRoot = townRoot
				return tt.results
			}
			install := preflightDigestInstaller(t, townRoot, run)

			err := install.PreSwap(context.Background(), podPairPair)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("PreSwap = %v, want the pair accepted", err)
				}
			} else {
				if err == nil {
					t.Fatal("PreSwap accepted a pair a check refused")
				}
				var pc update.PairCheckError
				if !errors.As(err, &pc) || pc.Check != update.CheckPreflight {
					t.Errorf("PreSwap error = %v, want a PairCheckError naming %q", err, update.CheckPreflight)
				}
				for _, want := range tt.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("PreSwap error = %q, want it to name %q", err, want)
					}
				}
			}
			if checkedRoot != townRoot {
				t.Errorf("the checks ran over %q, want the installer's town root %q", checkedRoot, townRoot)
			}
		})
	}
}

// TestPreRollGuardAPreflightThatCannotRunIsRefused holds the gate to refusing a
// preflight it cannot run: no town root, no check seam, or a seam that reports
// no checks at all is a failure, never a pass.
func TestPreRollGuardAPreflightThatCannotRunIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		townRoot func(t *testing.T) string
		// run is the check seam; nil leaves it unwired.
		run  func(context.Context, string) []PreflightResult
		want string
	}{
		{
			name:     "no town root",
			townRoot: func(*testing.T) string { return "" },
			run: func(context.Context, string) []PreflightResult {
				t.Error("the checks ran with no town root to run them over")
				return nil
			},
			want: "no town root",
		},
		{
			name:     "no check seam wired",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			want:     "no checks wired",
		},
		{
			name:     "a seam that ran no checks",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			run:      func(context.Context, string) []PreflightResult { return nil },
			want:     "ran no checks",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			install := preflightDigestInstaller(t, tt.townRoot(t), tt.run)

			err := install.PreSwap(context.Background(), podPairPair)
			if err == nil {
				t.Fatal("PreSwap accepted the pair: a preflight that cannot run is a failure, not a pass")
			}
			var pc update.PairCheckError
			if !errors.As(err, &pc) || pc.Check != update.CheckPreflight {
				t.Errorf("PreSwap error = %v, want a PairCheckError naming %q", err, update.CheckPreflight)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("PreSwap error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

// TestPreRollGuardProductionWiresThePreflight pins that the production command
// has the checks seam: a digest install built from the production seams would be
// refused with no checks wired otherwise.
func TestPreRollGuardProductionWiresThePreflight(t *testing.T) {
	t.Parallel()
	if productionUpdateSeams().Preflight == nil {
		t.Fatal("productionUpdateSeams wires no preflight: every digest install would be refused")
	}
}
