package cmd

import (
	"errors"
	"testing"
)

func TestMaintainCommand_Registered(t *testing.T) {
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
	if defaultMaintainThreshold != 100 {
		t.Errorf("expected default threshold 100, got %d", defaultMaintainThreshold)
	}
}
