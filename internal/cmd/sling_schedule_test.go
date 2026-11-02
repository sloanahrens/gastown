package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/wisp"
)

// TestAreScheduledFailClosed: when no town can be found from the cwd, every
// requested bead reads as scheduled (fail closed), and no sling context is
// read at all. This prevents false stranded detection and duplicate
// scheduling on transient errors.
func TestAreScheduledFailClosed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		root string
		err  error
	}{
		{"no town", "", nil},
		{"unreadable cwd", "", errors.New("getwd: permission denied")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requestedIDs := []string{"bead-1", "bead-2", "bead-3"}
			result := areScheduledWith("", requestedIDs,
				func() (string, error) { return tt.root, tt.err },
				func(string, []string) map[string]bool {
					t.Fatal("areScheduled read sling contexts without a town")
					return nil
				})

			for _, id := range requestedIDs {
				if !result[id] {
					t.Errorf("areScheduled fail-closed: expected %q to be marked as scheduled, but it was not", id)
				}
			}
		})
	}
}

// TestShouldDeferDispatchNoTownIsDirect pins the one case where an absent
// answer legitimately means "direct dispatch": the cwd is readable and simply
// not inside a town. It must not fall through to LoadOrCreateTownSettings
// with an empty root, which would write settings/config.json into the cwd
// (gt-udrrw, gt-bfale).
func TestShouldDeferDispatchNoTownIsDirect(t *testing.T) {
	t.Parallel()
	deferred, err := shouldDeferDispatchWith(
		func() (string, error) { return "", nil },
		func(path string) (*config.TownSettings, error) {
			t.Fatalf("shouldDeferDispatch loaded (and could create) settings at %q outside a town", path)
			return nil, nil
		})
	if err != nil {
		t.Fatalf("shouldDeferDispatch outside a town: unexpected error %v", err)
	}
	if deferred {
		t.Error("shouldDeferDispatch outside a town = true, want false (direct dispatch)")
	}
}

// TestAreScheduledEmptyInput verifies areScheduled returns empty map for no input.
func TestAreScheduledEmptyInput(t *testing.T) {
	t.Parallel()
	result := areScheduled(nil)
	if len(result) != 0 {
		t.Errorf("areScheduled(nil) should return empty map, got %d entries", len(result))
	}
	result = areScheduled([]string{})
	if len(result) != 0 {
		t.Errorf("areScheduled([]) should return empty map, got %d entries", len(result))
	}
}

// TestResolveFormula verifies formula resolution precedence:
// explicit flag > wisp layer > bead layer > system default > settings file > hardcoded fallback.
func TestResolveFormula(t *testing.T) {
	t.Parallel()

	t.Run("explicit flag wins", func(t *testing.T) {
		t.Parallel()
		got := resolveFormula("mol-evolve", false, "/tmp/nonexistent", "myrig")
		if got != "mol-evolve" {
			t.Errorf("got %q, want %q", got, "mol-evolve")
		}
	})

	t.Run("hookRawBead returns empty", func(t *testing.T) {
		t.Parallel()
		got := resolveFormula("mol-evolve", true, "/tmp/nonexistent", "myrig")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("system default mol-polecat-work", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		useFailingTownBD(t, tmpDir)
		rigName := "testrig"
		_ = os.MkdirAll(filepath.Join(tmpDir, rigName), 0o755)
		got := resolveFormula("", false, tmpDir, rigName)
		if got != "mol-polecat-work" {
			t.Errorf("got %q, want %q", got, "mol-polecat-work")
		}
	})

	t.Run("wisp layer overrides system default", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		useFailingTownBD(t, tmpDir)
		rigName := "testrig"
		_ = os.MkdirAll(filepath.Join(tmpDir, rigName), 0o755)

		wispCfg := wisp.NewConfig(tmpDir, rigName)
		if err := wispCfg.Set("default_formula", "mol-evolve"); err != nil {
			t.Fatalf("wisp set: %v", err)
		}

		got := resolveFormula("", false, tmpDir, rigName)
		if got != "mol-evolve" {
			t.Errorf("got %q, want %q", got, "mol-evolve")
		}
	})

	t.Run("explicit flag overrides wisp layer", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		useFailingTownBD(t, tmpDir)
		rigName := "testrig"
		_ = os.MkdirAll(filepath.Join(tmpDir, rigName), 0o755)

		wispCfg := wisp.NewConfig(tmpDir, rigName)
		if err := wispCfg.Set("default_formula", "mol-evolve"); err != nil {
			t.Fatalf("wisp set: %v", err)
		}

		got := resolveFormula("mol-custom", false, tmpDir, rigName)
		if got != "mol-custom" {
			t.Errorf("got %q, want %q", got, "mol-custom")
		}
	})

	t.Run("empty rigName falls back to hardcoded default", func(t *testing.T) {
		t.Parallel()
		got := resolveFormula("", false, "/tmp/nonexistent", "")
		if got != "mol-polecat-work" {
			t.Errorf("got %q, want %q", got, "mol-polecat-work")
		}
	})
}

// TestValidateNoTaskOnlySchedulerFlagsRejectsBranchAndPR: --branch and --pr
// resume an existing branch, which only a task bead has; epic and convoy mode
// must refuse them rather than drop them silently.
//
// The flags come from slingCmd itself. A stand-in here would pass even if gt
// sling stopped registering one of them, so the mode check would have nothing
// to reject and the refusal would never happen in practice.
//
//testpolicy:allow parallel — sets Changed on slingCmd's process-global flags, which other tests read
func TestValidateNoTaskOnlySchedulerFlagsRejectsBranchAndPR(t *testing.T) {
	// slingCmd's flags are process-global, so this test does not run parallel
	// with the ones that read them.
	for _, name := range []string{"branch", "pr"} {
		t.Run(name, func(t *testing.T) {
			f := slingCmd.Flags().Lookup(name)
			if f == nil {
				t.Fatalf("gt sling has no --%s flag, so epic and convoy mode cannot refuse it", name)
			}
			// Mark it changed as a parse would; setting the value would write
			// the shared sling-time variables other tests read. Restore the
			// prior state rather than false: another test may have left the
			// flag set.
			prior := f.Changed
			f.Changed = true
			defer func() { f.Changed = prior }()

			for _, mode := range []string{"epic", "convoy"} {
				err := validateNoTaskOnlySchedulerFlags(slingCmd, mode)
				if err == nil {
					t.Fatalf("validateNoTaskOnlySchedulerFlags(%s) accepted --%s", mode, name)
				}
				if !strings.Contains(err.Error(), "--"+name) {
					t.Fatalf("validateNoTaskOnlySchedulerFlags(%s) error = %v, want --%s", mode, err, name)
				}
			}
		})
	}
}

// failingCreateContexts is a rig's sling contexts whose creation fails. The
// lookup and every other write still go to the harness fake, so only the one
// path under test changes.
type failingCreateContexts struct {
	fakeSlingContexts
	err error
}

func (f failingCreateContexts) Create(beads.CreateOptions) (*beads.Issue, error) {
	return nil, f.err
}

// TestScheduleSlingTouchesContextStamp: the dispatch heartbeat answers "nothing
// is scheduled" without reading a database while no context has been created
// since its last empty scan, so a schedule that creates one has to stamp it
// (gt-66e5d). A creation that fails writes no context and touches nothing.
func TestScheduleSlingTouchesContextStamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		createErr error
		wantStamp bool
	}{
		{name: "created context", wantStamp: true},
		{name: "failed creation", createErr: errors.New("dolt write failed")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{Title: "scheduled work"})
			h.run.townOrEnv = func() (string, error) { return townRoot, nil }
			if tc.createErr != nil {
				h.run.slingContexts = func(string) slingContextStore {
					return failingCreateContexts{fakeSlingContexts{h}, tc.createErr}
				}
			}

			err := h.run.scheduleSlingBead(slingBead, "gastown", ScheduleOptions{})
			if tc.createErr != nil {
				wantSlingErr(t, err, "creating sling context")
			} else if err != nil {
				t.Fatalf("schedule: %v", err)
			}

			touches := h.matching("touch sling context stamp")
			if tc.wantStamp && len(touches) != 1 {
				t.Errorf("a schedule that created a sling context touched the stamp %d time(s), want 1: the heartbeat would keep skipping its scan and never dispatch it\nfull log:\n  %s",
					len(touches), strings.Join(h.log(), "\n  "))
			}
			if !tc.wantStamp && len(touches) != 0 {
				t.Errorf("a schedule whose context creation failed touched the stamp %d time(s); only a created context may announce itself", len(touches))
			}
			if tc.wantStamp && len(touches) == 1 && touches[0] != "touch sling context stamp "+townRoot {
				t.Errorf("touched the stamp for %q, want the town root %q", touches[0], townRoot)
			}
		})
	}
}

// TestScheduleStampFailureWarningIsAccurateAndDriven drives a failing stamp
// write through the schedule path through its injected seam, not a real
// filesystem fault, and pins what the operator is told. A touch that dropped the
// empty-scan marker leaves the next scan to see the context; a touch that could
// not drop it means the heartbeat may keep answering from the marker until the
// scan ceiling passes, and the warning must say so rather than promise a scan
// the marker does not allow.
func TestScheduleStampFailureWarningIsAccurateAndDriven(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk full")
	tests := []struct {
		name string
		// touchErr is what the injected stamp write returns.
		touchErr error
		// want is the claim the warning must make.
		want string
		// wantAbsent is the claim it must not make.
		wantAbsent string
	}{
		{
			name:       "the marker was dropped",
			touchErr:   &beads.SlingContextStampError{Err: boom, MarkerDropped: true},
			want:       "the next dispatch scan will read every database and see it",
			wantAbsent: "may be up to",
		},
		{
			name:       "the marker could not be dropped",
			touchErr:   &beads.SlingContextStampError{Err: boom},
			want:       "the next full scan may be up to " + beads.SlingContextScanMaxAge.String() + " away",
			wantAbsent: "will read every database and see it",
		},
	}
	var texts []string
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{Title: "scheduled work"})
			townRoot := t.TempDir()
			h.run.townOrEnv = func() (string, error) { return townRoot, nil }
			h.run.touchSlingContextStamp = func(string, time.Time) error { return tc.touchErr }

			// The context exists and the schedule stands: a stamp failure is a
			// warning, not a failed sling.
			if err := h.run.scheduleSlingBead(slingBead, "gastown", ScheduleOptions{}); err != nil {
				t.Fatalf("schedule = %v, want it to stand", err)
			}
			out := h.out.String()
			if !strings.Contains(out, "Warning") {
				t.Fatalf("output = %q, want a warning naming the failed stamp", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("warning = %q, want it to say %q", out, tc.want)
			}
			if strings.Contains(out, tc.wantAbsent) {
				t.Errorf("warning = %q, must not say %q: the cost depends on whether the marker was dropped", out, tc.wantAbsent)
			}
			texts = append(texts, out)
		})
	}
	if len(texts) == 2 && texts[0] == texts[1] {
		t.Errorf("both stamp-failure cases printed the same warning %q; the operator cannot tell an ordered next scan from one up to %s away",
			texts[0], beads.SlingContextScanMaxAge)
	}
}
