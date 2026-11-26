package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
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

// preflightRunningCommit is the commit the running daemon reports to a gate
// fixture that does not test the read.
const preflightRunningCommit = "1111111111111111111111111111111111111111"

// preflightRunningProbes is a verify probe set for the pre-swap gate. It answers
// the running daemon's commit; a gate that asks another probe is asking the
// wrong question, so those fail the row.
type preflightRunningProbes struct {
	// commit is the build the daemon reports, and err the read's failure.
	commit string
	err    error
	// asked counts the DaemonVersion reads.
	asked int
}

func (p *preflightRunningProbes) DaemonVersion(context.Context) (string, error) {
	p.asked++
	return p.commit, p.err
}

func (p *preflightRunningProbes) GTVersion(context.Context) (string, error) {
	return "", errors.New("the gate read the running gt's commit, not the daemon's")
}

func (p *preflightRunningProbes) ImageLabel(context.Context) (string, error) {
	return "", errors.New("the gate read the image label")
}

func (p *preflightRunningProbes) Health(context.Context) (string, error) {
	return "", errors.New("the gate read the health line")
}

// descendingAncestry reports every pair commit as a descendant of the running
// one, so a gate fixture that does not test the downgrade check passes it.
func descendingAncestry(string, string, string) (bool, error) { return true, nil }

// preflightAncestry is the ancestry read a downgrade row steers: it answers the
// row's result and records the commits the gate asked about.
type preflightAncestry struct {
	ancestor bool
	err      error
	calls    int
	from, to string
}

func (a *preflightAncestry) read(_, from, to string) (bool, error) {
	a.calls++
	a.from, a.to = from, to
	return a.ancestor, a.err
}

// preflightRefusal asserts err is the preflight check's refusal of the pair,
// naming every string in want and wrapping wantErr when one is given.
func preflightRefusal(t *testing.T, err error, want []string, wantErr error) {
	t.Helper()
	if err == nil {
		t.Fatal("PreSwap accepted a pair the downgrade check refused")
	}
	var pc update.PairCheckError
	if !errors.As(err, &pc) || pc.Check != update.CheckPreflight {
		t.Errorf("PreSwap error = %v, want a PairCheckError naming %q", err, update.CheckPreflight)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("PreSwap error = %q, want it to name %q", err, w)
		}
	}
	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Errorf("PreSwap error = %v, want it to wrap %v", err, wantErr)
	}
}

// preflightDigestInstaller builds the installer digestInstaller wires for a
// digest install over one town root, so a test drives its pre-swap gate. run is
// the check seam (nil: none); verify is the daemon probe the running commit is
// read through (nil: unwired). The downgrade check is given a descendant, so a
// row that tests another guard is decided by that guard alone.
func preflightDigestInstaller(t *testing.T, townRoot string, run func(context.Context, string) []PreflightResult, verify update.VerifyProbes) *update.DigestInstaller {
	t.Helper()
	return preflightDigestInstallerOf(t, townRoot, run, verify, false, descendingAncestry)
}

// preflightDigestInstallerOf is preflightDigestInstaller with the downgrade
// check's inputs: allowRollback is the run's flag, and ancestry is the read that
// answers whether the pair descends from the running commit (nil: unwired).
func preflightDigestInstallerOf(t *testing.T, townRoot string, run func(context.Context, string) []PreflightResult, verify update.VerifyProbes, allowRollback bool, ancestry ancestryRead) *update.DigestInstaller {
	t.Helper()
	return preflightDigestInstallerTo(t, townRoot, run, verify, allowRollback, ancestry, pairHeldDerive(podPairPair), io.Discard)
}

// preflightDigestInstallerTo is preflightDigestInstallerOf with the registry derivation and the swap's writer.
func preflightDigestInstallerTo(t *testing.T, townRoot string, run func(context.Context, string) []PreflightResult, verify update.VerifyProbes, allowRollback bool, ancestry ancestryRead, derive deriveFunc, out io.Writer) *update.DigestInstaller {
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
		// The pair-level check reads agreeing levels, so a row that tests the
		// town checks is decided by them alone.
		PairLevel: agreeingPairLevels(),
		// The derivation is the row's: preflightDigestInstallerOf answers the
		// pair the fixture installs, so a row is decided by the guard it tests.
		PairDerive: derive,
		Ancestry:   ancestry,
	}
	target := podPairTarget()
	deps := update.Deps{Verify: verify}
	installer, err := digestInstaller(seams, townRoot, deps, path, &target, out, allowRollback)
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
			install := preflightDigestInstaller(t, townRoot, run, &preflightRunningProbes{commit: preflightRunningCommit})

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

// TestPreRollGuardPreSwapRunsTheRealChecks drives the production checks through
// the gate: the real town-config check and a bd handshake behind
// runPreflightChecksWith, so a town the checks refuse refuses the pair, naming
// the check and its message.
func TestPreRollGuardPreSwapRunsTheRealChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// brokenTown writes a mayor/town.json the real check refuses.
		brokenTown bool
		// want is empty when the pair is accepted; otherwise every string must
		// appear in the refusal.
		want []string
	}{
		{name: "a town the real checks pass accepts the pair"},
		{
			name:       "a town config the real check refuses refuses the pair",
			brokenTown: true,
			want:       []string{preflightNameTownConfig, "mayor/town.json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tt.brokenTown {
				writeUnparseableTownConfig(t, townRoot)
			}
			var calls preflightCall
			checks := preflightChecks{
				townConfig:  daemon.CheckTownConfig,
				bdHandshake: fakeBDHandshake(&calls, nil),
			}
			run := func(ctx context.Context, townRoot string) []PreflightResult {
				return runPreflightChecksWith(ctx, townRoot, checks)
			}
			install := preflightDigestInstaller(t, townRoot, run, &preflightRunningProbes{commit: preflightRunningCommit})

			err := install.PreSwap(context.Background(), podPairPair)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("PreSwap = %v, want the pair accepted", err)
				}
			} else {
				if err == nil {
					t.Fatal("PreSwap accepted a pair the real checks refused")
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
			if calls.handshake != 1 {
				t.Errorf("the bd handshake ran %d times, want once through the gate", calls.handshake)
			}
		})
	}
}

// TestPreRollGuardAPreflightThatCannotRunIsRefused holds the gate to refusing a
// preflight it cannot run: no town root, no check seam, or a seam that reports
// no checks at all is a failure, never a pass. The gate is built directly, so
// the no-town-root row drives the gate's own guard, not digestInstaller's
// tolerance of an empty town root.
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
			// These rows never reach the pair-level reads: each stops the gate at
			// the check seam. Zero reads would refuse if one were reached, and the
			// row's want names the guard it expects instead.
			gate := preSwapPreflight(tt.townRoot(t), tt.run, pairLevelReads{}, pairHeldDerive(podPairPair), &preflightRunningProbes{commit: preflightRunningCommit}, false, descendingAncestry, io.Discard)

			err := gate.check(context.Background(), podPairPair)
			if err == nil {
				t.Fatal("the pre-swap gate accepted the pair: a preflight that cannot run is a failure, not a pass")
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

// pairHeldDerive is a registry derivation that answers the pair a fixture holds:
// a row testing another guard passes the registry match and is decided by that
// guard alone.
func pairHeldDerive(pair update.ImagePair) deriveFunc {
	return func(context.Context, string) (imagevalues.Values, error) {
		return pairValues(pair), nil
	}
}

// pairOfValues is the pair a values file names, the pair a gate is given.
func pairOfValues(v imagevalues.Values) update.ImagePair {
	return update.ImagePair{Daemon: v.Daemon, Polecat: v.Polecat, Bd: v.Bd, Commit: v.Commit}
}

// TestPreSwapPairMatchesRegistry holds the pre-swap gate to the registry's
// re-derivation: the values file built from the pair the gate is given is
// compared with the pair the derivation answers for its commit, so an equal pair
// passes and a pair the derivation does not answer is refused as the preflight
// check, naming the field and both digests. The preflight passes and the levels
// agree, so no row is decided by another guard. The derivation is asked about
// the pair's commit and nothing else.
func TestPreSwapPairMatchesRegistry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// edit changes the pair the gate is given; nil leaves it equal to the
		// derived pair.
		edit func(p *update.ImagePair)
		// want is empty when the pair matches; otherwise every string must
		// appear in the refusal.
		want []string
	}{
		{name: "a file equal to the derived pair passes"},
		{
			name: "a differing daemon digest is refused as the preflight check",
			edit: func(p *update.ImagePair) { p.Daemon = pairMatchRef(imagevalues.DaemonImage, "111111111111") },
			want: []string{"daemon", "sha256:111111111111", "sha256:dddddddddddd"},
		},
		{
			name: "a differing polecat digest is refused as the preflight check",
			edit: func(p *update.ImagePair) { p.Polecat = pairMatchRef(imagevalues.PolecatImage, "222222222222") },
			want: []string{"polecat", "sha256:222222222222", "sha256:eeeeeeeeeeee"},
		},
		{
			name: "a differing bd digest is refused as the preflight check",
			edit: func(p *update.ImagePair) { p.Bd = pairMatchRef(imagevalues.BdImage, "333333333333") },
			want: []string{"bd", "sha256:333333333333", "sha256:bbbbbbbbbbbb"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			derived := pairMatchPair()
			pair := pairOfValues(derived)
			if tt.edit != nil {
				tt.edit(&pair)
			}
			var seen []string
			derive := func(_ context.Context, sha string) (imagevalues.Values, error) {
				seen = append(seen, sha)
				return derived, nil
			}
			gate := preSwapPreflight(t.TempDir(), preflightPassing, agreeingPairLevels(), derive, &preflightRunningProbes{commit: preflightRunningCommit}, false, descendingAncestry, io.Discard)

			err := gate.check(context.Background(), pair)
			if len(seen) != 1 || seen[0] != pair.Commit {
				t.Errorf("the derivation was asked about commits %q, want exactly [%q]", seen, pair.Commit)
			}
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("PreSwap refused a pair equal to the derived pair: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("PreSwap accepted a pair the registry derivation does not match")
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
		})
	}
}

// TestPreSwapPairMatchesRegistryUnwired holds the pre-swap gate to refusing an
// install with no registry derivation wired: the preflight passes and the pair
// levels agree, so only the registry match can refuse, and a nil derivation is
// refused there before any comparison, as the preflight check, naming the
// missing derivation and the commit it could not match. A gate that panicked on
// the nil derivation would fail the row.
func TestPreSwapPairMatchesRegistryUnwired(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// pair is the pair the gate is given: it passes the preflight and the
		// pair-level check, so the row is decided by the unwired derivation alone.
		pair update.ImagePair
	}{
		{name: "a pair the registry would match is refused with no derivation wired", pair: pairOfValues(pairMatchPair())},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := preSwapPreflight(t.TempDir(), preflightPassing, agreeingPairLevels(), nil, &preflightRunningProbes{commit: preflightRunningCommit}, false, descendingAncestry, io.Discard)

			err := gate.check(context.Background(), tt.pair)
			if err == nil {
				t.Fatal("PreSwap accepted a pair with no registry derivation wired")
			}
			var pc update.PairCheckError
			if !errors.As(err, &pc) || pc.Check != update.CheckPreflight {
				t.Errorf("PreSwap error = %v, want a PairCheckError naming %q", err, update.CheckPreflight)
			}
			for _, want := range []string{"no registry derivation wired", tt.pair.Commit} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("PreSwap error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// matchLine is the line a passed registry match writes, naming the pair
// pairMatchPair derives.
const matchLine = "pre-swap: the pair matches the registry: commit c0ffee0c0ffee0c0ffee0c0ffee0c0ffee0c0ffe, daemon registry.internal:5000/town/gastown-daemon@sha256:dddddddddddd0000000000000000000000000000000000000000000000000000, polecat registry.internal:5000/town/gastown-polecat@sha256:eeeeeeeeeeee0000000000000000000000000000000000000000000000000000, bd registry.internal:5000/town/gastown-bd@sha256:bbbbbbbbbbbb0000000000000000000000000000000000000000000000000000\n"

// TestPreSwapPairMatchesRegistryLogs holds the gate's registry match to the log
// it leaves in the swap: the writer the install was built with receives one
// literal line naming the commit and the three images the file names when the
// match passes, and nothing when the registry refuses the pair, when an earlier
// check refuses it, or when a later check does. The install is driven through
// digestInstaller, so a gate built without the swap's writer leaves the log
// empty.
func TestPreSwapPairMatchesRegistryLogs(t *testing.T) {
	t.Parallel()
	matched := pairOfValues(pairMatchPair())
	tests := []struct {
		name string
		// run is the check seam.
		run func(context.Context, string) []PreflightResult
		// ancestor is the downgrade check's answer.
		ancestor bool
		// edit changes the pair the install is given.
		edit func(p *update.ImagePair)
		// want is the line written; empty when the gate refuses and writes nothing.
		want string
	}{
		{
			name:     "an accepted pair writes the match line",
			run:      preflightPassing,
			ancestor: true,
			want:     matchLine,
		},
		{
			name:     "a pair the registry does not match writes nothing",
			run:      preflightPassing,
			ancestor: true,
			edit:     func(p *update.ImagePair) { p.Daemon = pairMatchRef(imagevalues.DaemonImage, "111111111111") },
		},
		{
			name: "an earlier check that refuses writes nothing",
			run: func(context.Context, string) []PreflightResult {
				return []PreflightResult{{Name: preflightNameTownConfig, Err: errors.New("the town config does not load")}}
			},
			ancestor: true,
		},
		{
			name: "a downgrade the gate refuses writes nothing",
			run:  preflightPassing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pair := matched
			if tt.edit != nil {
				tt.edit(&pair)
			}
			var out bytes.Buffer
			install := preflightDigestInstallerTo(t, t.TempDir(), tt.run, &preflightRunningProbes{commit: preflightRunningCommit}, false,
				(&preflightAncestry{ancestor: tt.ancestor}).read, pairHeldDerive(matched), &out)

			err := install.PreSwap(context.Background(), pair)
			if tt.want == "" {
				if err == nil {
					t.Fatal("PreSwap accepted a pair the gate refuses")
				}
				if out.Len() != 0 {
					t.Errorf("the pre-swap log = %q, want no line on a refusal", out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("PreSwap = %v, want the pair accepted", err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("the pre-swap log = %q, want the line %q", got, tt.want)
			}
		})
	}
}

// TestPreSwapRunningCommit holds the gate the installer wires to the running
// daemon's commit: the gate reads it through the install's verify probes, so a
// readable commit reaches the check and the pair passes, while a read that fails
// or reports no commit is refused as the preflight check. A gate with no daemon
// probe refuses too: a commit the gate did not read is not a commit it checked.
// The other checks pass, so every row is decided by the running-commit read.
func TestPreSwapRunningCommit(t *testing.T) {
	t.Parallel()
	unreadable := errors.New("the pod did not answer")
	tests := []struct {
		name string
		// verify is the installer's daemon probe; nil leaves it unwired.
		verify *preflightRunningProbes
		// want is empty when the pair is accepted; else every string must appear.
		want     []string
		wantRead int
		wantErr  error
	}{
		{
			name:     "a readable running commit reaches the check",
			verify:   &preflightRunningProbes{commit: preflightRunningCommit},
			wantRead: 1,
		},
		{
			name:     "a running commit that cannot be read is refused",
			verify:   &preflightRunningProbes{err: unreadable},
			want:     []string{"running daemon's commit", unreadable.Error()},
			wantRead: 1,
			wantErr:  unreadable,
		},
		{
			name:     "an empty running commit is refused",
			verify:   &preflightRunningProbes{},
			want:     []string{"running daemon", "no commit"},
			wantRead: 1,
		},
		{
			name: "a gate with no daemon probe is refused",
			want: []string{"no daemon probe"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A nil row hands the installer a nil interface, not a typed nil.
			var verify update.VerifyProbes
			if tt.verify != nil {
				verify = tt.verify
			}
			install := preflightDigestInstaller(t, t.TempDir(), preflightPassing, verify)

			err := install.PreSwap(context.Background(), podPairPair)
			if tt.verify != nil && tt.verify.asked != tt.wantRead {
				t.Errorf("the daemon was read %d times, want %d", tt.verify.asked, tt.wantRead)
			}
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("PreSwap refused a pair with a readable running commit: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("PreSwap accepted a pair whose running commit it could not read")
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
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("PreSwap error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// TestPreSwapRunningCommitRecords holds the gate to the running commit it
// records: an accepted pair leaves the commit the daemon reported on the gate,
// and a later run that cannot read the commit leaves none.
func TestPreSwapRunningCommitRecords(t *testing.T) {
	t.Parallel()
	probe := &preflightRunningProbes{commit: preflightRunningCommit}
	gate := preSwapPreflight(t.TempDir(), preflightPassing, agreeingPairLevels(), pairHeldDerive(podPairPair), probe, false, descendingAncestry, io.Discard)

	if err := gate.check(context.Background(), podPairPair); err != nil {
		t.Fatalf("PreSwap = %v, want the pair accepted", err)
	}
	if gate.running != preflightRunningCommit {
		t.Errorf("recorded running commit = %q, want %q", gate.running, preflightRunningCommit)
	}

	// A daemon that stops answering must not leave the previous run's commit.
	probe.commit = ""
	if err := gate.check(context.Background(), podPairPair); err == nil {
		t.Fatal("PreSwap accepted a pair whose running commit it could not read")
	}
	if gate.running != "" {
		t.Errorf("recorded running commit = %q after a refused read, want none", gate.running)
	}
}

// TestPreSwapNoDowngrade holds the pre-swap gate to the downgrade check: a pair
// whose commit descends from the running one passes, and an ancestor is refused
// as the preflight check, naming the running commit and the pair's. A run given
// --allow-rollback lets the ancestor through without asking the ancestry read.
// The read is asked whether the running commit is an ancestor of the pair's
// commit, in that order, so a swap of the arguments would invert the guard.
func TestPreSwapNoDowngrade(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		ancestor      bool
		allowRollback bool
		// want is empty when the pair is accepted; otherwise every string must
		// appear in the refusal.
		want []string
	}{
		{
			name:     "a descendant passes",
			ancestor: true,
		},
		{
			name: "an ancestor is refused naming both commits",
			want: []string{
				"refusing downgrade of " + preflightRunningCommit,
				"to " + podPairCommit,
			},
		},
		{
			name:          "an ancestor passes with allowRollback",
			allowRollback: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			read := &preflightAncestry{ancestor: tt.ancestor}
			install := preflightDigestInstallerOf(t, t.TempDir(), preflightPassing, &preflightRunningProbes{commit: preflightRunningCommit}, tt.allowRollback, read.read)

			err := install.PreSwap(context.Background(), podPairPair)
			wantRead := 1
			if tt.allowRollback {
				wantRead = 0
			}
			if read.calls != wantRead {
				t.Errorf("the ancestry read was asked %d times, want %d", read.calls, wantRead)
			}
			if read.calls > 0 && (read.from != preflightRunningCommit || read.to != podPairPair.Commit) {
				t.Errorf("the ancestry read was asked whether %q is an ancestor of %q, want (%q, %q)",
					read.from, read.to, preflightRunningCommit, podPairPair.Commit)
			}
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("PreSwap = %v, want the pair accepted", err)
				}
				return
			}
			preflightRefusal(t, err, tt.want, nil)
		})
	}
}

// TestPreSwapNoDowngradeAncestryFails holds the gate to refusing a downgrade it
// could not rule out: an ancestry read that fails is refused as the preflight
// check, naming both commits and wrapping the read's error, and an unwired
// ancestry read is refused rather than passing the pair.
func TestPreSwapNoDowngradeAncestryFails(t *testing.T) {
	t.Parallel()
	unreadable := errors.New("git merge-base --is-ancestor: exit status 128")
	tests := []struct {
		name    string
		unwired bool
		err     error
		// want is empty when the pair is accepted; otherwise every string must
		// appear in the refusal.
		want    []string
		wantErr error
	}{
		{
			name: "a failed ancestry read is refused",
			err:  unreadable,
			want: []string{
				"refusing downgrade of " + preflightRunningCommit,
				"to " + podPairCommit,
				unreadable.Error(),
			},
			wantErr: unreadable,
		},
		{
			name:    "an unwired ancestry read is refused",
			unwired: true,
			want:    []string{"no ancestry reader is wired"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			read := &preflightAncestry{err: tt.err}
			var ancestry ancestryRead = read.read
			if tt.unwired {
				ancestry = nil
			}
			install := preflightDigestInstallerOf(t, t.TempDir(), preflightPassing, &preflightRunningProbes{commit: preflightRunningCommit}, false, ancestry)

			err := install.PreSwap(context.Background(), podPairPair)
			if tt.unwired && read.calls != 0 {
				t.Errorf("the unwired ancestry read was asked %d times, want none", read.calls)
			}
			preflightRefusal(t, err, tt.want, tt.wantErr)
		})
	}
}

// TestPreRollGuardProductionWiresTheAncestry pins that the production seams
// carry the downgrade check's ancestry read, and that it is the town's reader:
// a nil or stub read would refuse every digest install of another commit.
func TestPreRollGuardProductionWiresTheAncestry(t *testing.T) {
	t.Parallel()
	read := productionUpdateSeams().Ancestry
	if read == nil {
		t.Fatal("productionUpdateSeams wires no ancestry read: every digest install of another commit would be refused")
	}
	if got, want := reflect.ValueOf(read).Pointer(), reflect.ValueOf(townAncestry).Pointer(); got != want {
		t.Error("productionUpdateSeams wires an ancestry read other than townAncestry")
	}
}
