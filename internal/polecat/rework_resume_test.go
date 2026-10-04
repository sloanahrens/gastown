package polecat

import "testing"

// TestReworkResumeAllowed pins the fence around the workstate exemption a
// rework resume takes (gt-lid6d): only a named resume branch, checked out on
// the seat, which is not working and holds no other bead.
func TestReworkResumeAllowed(t *testing.T) {
	t.Parallel()
	const bead, branch = "gt-rw1", "polecat/garnet/gt-rw1+muu1"
	onBranch := func(state State, issue string) *Polecat {
		return &Polecat{State: state, Issue: issue, Branch: branch}
	}
	for _, tt := range []struct {
		name    string
		opts    AddOptions
		current *Polecat
		want    bool
	}{
		{
			name:    "idle author, its own bead on the hook",
			opts:    AddOptions{HookBead: bead, ResumeBranch: branch, AllowRecovery: true},
			current: onBranch(StateIdle, bead),
			want:    true,
		},
		{
			name:    "idle author, no hook",
			opts:    AddOptions{HookBead: bead, ResumeBranch: branch, AllowRecovery: true},
			current: onBranch(StateIdle, ""),
			want:    true,
		},
		{
			name:    "done author mid-recovery",
			opts:    AddOptions{HookBead: bead, ResumeBranch: branch, AllowRecovery: true},
			current: &Polecat{State: StateDone, Issue: bead, Branch: branch},
			want:    true,
		},
		{
			name:    "not allowed",
			opts:    AddOptions{HookBead: bead, ResumeBranch: branch},
			current: onBranch(StateIdle, bead),
		},
		{
			name:    "no resume branch",
			opts:    AddOptions{HookBead: bead, AllowRecovery: true},
			current: onBranch(StateIdle, bead),
		},
		{
			name:    "author standing on another branch",
			opts:    AddOptions{HookBead: bead, ResumeBranch: branch, AllowRecovery: true},
			current: &Polecat{State: StateIdle, Branch: "polecat/garnet/gt-other+muu9"},
		},
		{
			name:    "author working",
			opts:    AddOptions{HookBead: bead, ResumeBranch: branch, AllowRecovery: true},
			current: onBranch(StateWorking, bead),
		},
		{
			name:    "author holds another bead",
			opts:    AddOptions{HookBead: bead, ResumeBranch: branch, AllowRecovery: true},
			current: onBranch(StateIdle, "gt-other"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reworkResumeAllowed(tt.opts, tt.current); got != tt.want {
				t.Errorf("reworkResumeAllowed(%+v, %+v) = %v; want %v", tt.opts, tt.current, got, tt.want)
			}
		})
	}
}
