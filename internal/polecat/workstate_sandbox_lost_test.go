package polecat

import (
	"slices"
	"testing"
)

// A sandbox seat whose sandbox the gateway affirmed gone, with its bead
// unfinished and no pushed or resume branch to continue from, has lost its
// work: the verdict names that, the seat is neither reusable nor safe to nuke,
// and it counts toward capacity until the bead is sent back for rework. A
// finished or submitted bead, a surviving branch, a sandbox the gateway did not
// affirm gone, and a seat that names no bead are not lost work (gt-qq6rg.3).
func TestDecideWorkstateOfASandboxSeatWithLostWork(t *testing.T) {
	t.Parallel()
	// A seat that ended idle and clean in a gone sandbox: nothing else to refuse.
	gone := func(edit func(*WorkstateFacts)) WorkstateFacts {
		f := WorkstateFacts{
			State: StateIdle, CleanupStatus: CleanupClean, HookBeadSafe: true,
			GitStateSource: GitStateSourceLive, AgentBeadRead: true,
			SandboxGone: true, SandboxWorkBead: "gt-work",
		}
		edit(&f)
		return f
	}
	tests := []struct {
		name       string
		facts      WorkstateFacts
		wantLost   bool
		wantSafe   bool
		wantReason string // checked when set
	}{
		{"hook_cleared_bead_open_nothing_pushed", gone(func(*WorkstateFacts) {}), true, false, "sandbox-work-lost"},
		{"hook_still_set_bead_open_nothing_pushed", gone(func(f *WorkstateFacts) { f.HookBead, f.HookBeadSafe = "gt-work", false }), true, false, "sandbox-work-lost"},
		{"branch_on_origin_or_resume_branch", gone(func(f *WorkstateFacts) { f.SandboxBranchPreserved = true }), false, true, ""},
		{"hook_still_set_but_branch_preserved", gone(func(f *WorkstateFacts) {
			f.SandboxBranchPreserved, f.HookBead, f.HookBeadSafe = true, "gt-work", false
		}), false, false, "hook-still-set"},
		{"hook_bead_terminal", gone(func(f *WorkstateFacts) { f.HookBeadTerminal = true }), false, true, ""},
		{"assigned_bead_terminal", gone(func(f *WorkstateFacts) { f.AssignedBeadTerminal = true }), false, true, ""},
		{"active_mr_source_terminal", gone(func(f *WorkstateFacts) { f.ActiveMRSourceTerminal = true }), false, true, ""},
		{"bead_submitted_for_landing", gone(func(f *WorkstateFacts) { f.HookBeadSubmitted = true }), false, false, "submitted-for-landing"},
		{"sandbox_not_affirmed_gone", gone(func(f *WorkstateFacts) { f.SandboxGone = false }), false, true, ""},
		{"seat_names_no_bead", gone(func(f *WorkstateFacts) { f.SandboxWorkBead = "" }), false, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := NewWorkstateInput(tt.facts)
			if in.SandboxWorkLost != tt.wantLost {
				t.Errorf("SandboxWorkLost = %v, want %v", in.SandboxWorkLost, tt.wantLost)
			}

			d := DecideWorkstate(in)

			if tt.wantLost {
				if d.Verdict != WorkstateVerdictNeedsRecovery || !d.NeedsRecovery || d.SafeToNuke || d.Reusable || !d.CountsTowardCapacity {
					t.Errorf("lost work = %+v, want NEEDS_RECOVERY that is neither reusable nor safe to nuke and counts toward capacity", d)
				}
				if !slices.Contains(d.Blockers, "sandbox gone with gt-work unfinished and no pushed or resume branch: the work is lost and the bead needs rework") {
					t.Errorf("blockers %v do not say the work is lost", d.Blockers)
				}
			}
			if d.SafeToNuke != tt.wantSafe {
				t.Errorf("SafeToNuke = %v, want %v (reason %s blockers %v)", d.SafeToNuke, tt.wantSafe, d.Reason, d.Blockers)
			}
			if tt.wantReason != "" && d.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", d.Reason, tt.wantReason)
			}
		})
	}
}
