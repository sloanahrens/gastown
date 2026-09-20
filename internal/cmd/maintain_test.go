package cmd

import (
	"errors"
	"testing"
)

func TestMaintainCommand_Registered(t *testing.T) {
	t.Parallel()
	var found bool
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "maintain" {
			found = true

			if f := cmd.Flags().Lookup("force"); f == nil {
				t.Error("expected --force flag")
			}
			if f := cmd.Flags().Lookup("dry-run"); f == nil {
				t.Error("expected --dry-run flag")
			}
			if f := cmd.Flags().Lookup("threshold"); f == nil {
				t.Error("expected --threshold flag")
			} else if f.DefValue != "100" {
				t.Errorf("expected threshold default 100, got %s", f.DefValue)
			}
			// The divergence pre-flight is opt-OUT: flatten refuses by default
			// and --force-diverged turns the guard off.
			if f := cmd.Flags().Lookup("force-diverged"); f == nil {
				t.Error("expected --force-diverged flag")
			} else if f.DefValue != "false" {
				t.Errorf("expected --force-diverged default false, got %s", f.DefValue)
			}

			if cmd.GroupID != GroupServices {
				t.Errorf("expected GroupServices, got %s", cmd.GroupID)
			}
			break
		}
	}
	if !found {
		t.Fatal("maintain command not registered on rootCmd")
	}
}

func TestMaintainNeedsFlatten(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		commitCount int
		countKnown  bool
		threshold   int
		flatten     bool
	}{
		{"quiet below threshold", 0, true, 100, false},
		{"near threshold", 99, true, 100, false},
		{"at threshold", 100, true, 100, true},
		{"over threshold", 200, true, 100, true},
		{"well over threshold", 1000, true, 100, true},
		{"at custom threshold", 5, true, 5, true},
		{"below custom threshold", 4, true, 5, false},
		// gt-racu: an unknown count is not a zero. A failed measurement must
		// not become "below threshold" — that is the silent skip.
		{"unknown count flattens rather than skips", 0, false, 100, true},
		{"unknown count ignores a stale value", 7, false, 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := maintainDBInfo{name: "om", commitCount: tt.commitCount, countKnown: tt.countKnown}
			if got := db.needsFlatten(tt.threshold); got != tt.flatten {
				t.Errorf("needsFlatten(%d) with countKnown=%v commitCount=%d: got %v, want %v",
					tt.threshold, tt.countKnown, tt.commitCount, got, tt.flatten)
			}
		})
	}
}

func TestMaintainCountRendering(t *testing.T) {
	t.Parallel()
	known := maintainDBInfo{name: "om", commitCount: 608, countKnown: true}
	zero := maintainDBInfo{name: "om", commitCount: 0, countKnown: true}
	unknown := maintainDBInfo{name: "om", countErr: errors.New("connect: connection refused")}

	if got, want := known.countText(), "608 commits"; got != want {
		t.Errorf("known countText: got %q, want %q", got, want)
	}
	if got, want := known.countLabel(), "608"; got != want {
		t.Errorf("known countLabel: got %q, want %q", got, want)
	}
	if got, want := zero.countText(), "0 commits"; got != want {
		t.Errorf("zero countText: got %q, want %q", got, want)
	}

	wantUnknown := "commits unknown (connect: connection refused)"
	if got := unknown.countText(); got != wantUnknown {
		t.Errorf("unknown countText: got %q, want %q", got, wantUnknown)
	}
	if got, want := unknown.countLabel(), "unknown"; got != want {
		t.Errorf("unknown countLabel: got %q, want %q", got, want)
	}

	// The defect: a failed count and a genuinely quiet database were
	// indistinguishable in the plan. Neither the error text nor the bare
	// label may collapse the two.
	if zero.countText() == unknown.countText() {
		t.Errorf("a failed count and a genuine zero render identically: %q", zero.countText())
	}
	if zero.countLabel() == unknown.countLabel() {
		t.Errorf("a failed count and a genuine zero label identically: %q", zero.countLabel())
	}
}

func TestMaintainDBInfo(t *testing.T) {
	t.Parallel()
	// Verify the struct can hold expected values.
	info := maintainDBInfo{
		name:        "gastown",
		commitCount: 500,
		countKnown:  true,
		hasBackup:   true,
	}
	if info.name != "gastown" {
		t.Errorf("expected name gastown, got %s", info.name)
	}
	if info.commitCount != 500 {
		t.Errorf("expected 500 commits, got %d", info.commitCount)
	}
	if !info.hasBackup {
		t.Error("expected hasBackup true")
	}
}

func TestMaintainConstants(t *testing.T) {
	t.Parallel()
	if defaultMaintainThreshold != 100 {
		t.Errorf("expected default threshold 100, got %d", defaultMaintainThreshold)
	}
}

// TestMaintainPreflightRefusal drives every branch of the flatten guard. The
// guard exists because flatten rewrites the commit graph: squashing a database
// whose remote has moved on makes the two histories disagree, and the
// force-push that follows a flatten then deletes the remote-only commits.
func TestMaintainPreflightRefusal(t *testing.T) {
	t.Parallel()
	fetchErr := errors.New("DOLT_FETCH origin: dial tcp 127.0.0.1:443: connect: connection refused")

	tests := []struct {
		name          string
		preflight     maintainPreflight
		forceDiverged bool
		wantRefusal   string
	}{
		{
			name:      "no remote clears the guard",
			preflight: maintainPreflight{},
		},
		{
			name:      "remote verified as not diverged clears the guard",
			preflight: maintainPreflight{Remote: "origin"},
		},
		{
			name:        "diverged refuses with the required message",
			preflight:   maintainPreflight{Remote: "origin", Diverged: true},
			wantRefusal: "diverged from origin; pass --force-diverged",
		},
		{
			// The load-bearing case: a pre-flight that could not run is not a
			// pass. Only a completed check licenses the destructive write.
			name:        "failed check refuses instead of passing",
			preflight:   maintainPreflight{Remote: "origin", Err: fetchErr},
			wantRefusal: "cannot verify remote (DOLT_FETCH origin: dial tcp 127.0.0.1:443: connect: connection refused) — pass --force-diverged to flatten anyway",
		},
		{
			name:        "unreachable remote with no name known still refuses",
			preflight:   maintainPreflight{Err: errors.New("list remotes: connection refused")},
			wantRefusal: "cannot verify remote (list remotes: connection refused) — pass --force-diverged to flatten anyway",
		},
		{
			// A failed check on a database that also looked diverged reports
			// the uncertainty, not the divergence: the divergence was read from
			// a check that did not finish.
			name:        "failure outranks a partial divergence reading",
			preflight:   maintainPreflight{Remote: "origin", Diverged: true, Err: fetchErr},
			wantRefusal: "cannot verify remote (DOLT_FETCH origin: dial tcp 127.0.0.1:443: connect: connection refused) — pass --force-diverged to flatten anyway",
		},
		{
			name:          "--force-diverged overrides a detected divergence",
			preflight:     maintainPreflight{Remote: "origin", Diverged: true},
			forceDiverged: true,
		},
		{
			name:          "--force-diverged overrides a failed check",
			preflight:     maintainPreflight{Remote: "origin", Err: fetchErr},
			forceDiverged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.preflight.refusal(tt.forceDiverged); got != tt.wantRefusal {
				t.Errorf("refusal(forceDiverged=%v) = %q, want %q", tt.forceDiverged, got, tt.wantRefusal)
			}
		})
	}
}

// TestMaintainPreflightRefusalIsNotSilent pins the property the guard would
// lose if someone "simplified" the failed-check branch to a pass: a pre-flight
// error must never be reported as cleared.
func TestMaintainPreflightRefusalIsNotSilent(t *testing.T) {
	t.Parallel()
	failed := maintainPreflight{Remote: "origin", Err: errors.New("boom")}
	if failed.refusal(false) == "" {
		t.Fatal("a failed pre-flight cleared the guard — flatten would proceed unverified")
	}
	unchecked := maintainPreflight{Remote: "origin"}
	if unchecked.refusal(false) != "" {
		t.Fatal("a verified, undiverged remote was refused — the guard would never clear")
	}
}
