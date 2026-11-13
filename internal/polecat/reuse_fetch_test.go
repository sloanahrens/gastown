package polecat

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fetchLog records the fetches a manager makes, in order, across every
// repository it opens: "fetch origin" for a full fetch, "fetch origin <branch>"
// for a branch fetch.
type fetchLog struct {
	calls []string
	// failFetch makes a full fetch of origin fail, as an offline remote does.
	failFetch bool
	// failBranch makes a branch fetch fail, as a branch origin lacks does.
	failBranch bool
}

// fetchCountingRepo is a gitRepo whose fetches are recorded on log.
type fetchCountingRepo struct {
	gitRepo
	log *fetchLog
}

func (r fetchCountingRepo) Fetch(remote string) error {
	r.log.calls = append(r.log.calls, "fetch "+remote)
	if r.log.failFetch {
		return errors.New("fetch refused: remote unreachable")
	}
	return r.gitRepo.Fetch(remote)
}

func (r fetchCountingRepo) FetchBranch(remote, branch string) error {
	r.log.calls = append(r.log.calls, "fetch "+remote+" "+branch)
	if r.log.failBranch {
		return errors.New("fetch refused: no such branch")
	}
	return r.gitRepo.FetchBranch(remote, branch)
}

// countFetchesOn makes every repository mgr opens record its fetches on log.
func countFetchesOn(mgr *Manager, w *world, log *fetchLog) {
	base := w.opener()
	mgr.gits = gitOpener{
		open: func(dir string) gitRepo {
			return fetchCountingRepo{base.open(dir), log}
		},
		openDir: func(gitDir, workDir string) gitRepo {
			return fetchCountingRepo{base.openDir(gitDir, workDir), log}
		},
		repoFile: base.repoFile,
	}
}

// TestReuseFetchesOnce: the reuse path fetches origin once, on the bare
// repository, and fetches the resume branch only when the full fetch left its
// remote-tracking ref missing, or when the full fetch failed.
func TestReuseFetchesOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// resume is a branch origin does not have, "" for no resume.
		resume string
		// onOrigin resumes the seat's own branch, which origin has.
		onOrigin bool
		// staleOnOrigin has the remote-tracking ref already fetched before the
		// reuse, so a failed full fetch leaves it stale rather than missing.
		staleOnOrigin bool
		// failFetch makes the full fetch fail.
		failFetch bool
		// want is the fetches the reuse path makes, in order. "<branch>" is
		// the branch the case resumes.
		want []string
		// wantWarn is a substring of the one fetch warning the case emits;
		// "" for a case whose fetches all succeed. "<branch>" is replaced
		// as in want.
		wantWarn string
	}{
		{
			name: "no resume branch fetches origin once",
			want: []string{"fetch origin"},
		},
		{
			name:     "resume branch present after the full fetch is not fetched again",
			onOrigin: true,
			want:     []string{"fetch origin"},
		},
		{
			name:     "resume branch missing after the full fetch is fetched",
			resume:   "polecat/toast/resume-missing",
			want:     []string{"fetch origin", "fetch origin <branch>"},
			wantWarn: "could not fetch resume branch <branch> on the bare repo",
		},
		{
			name:          "failed full fetch leaves a stale ref, which is still fetched",
			onOrigin:      true,
			staleOnOrigin: true,
			failFetch:     true,
			want:          []string{"fetch origin", "fetch origin <branch>"},
			wantWarn:      "could not fetch origin for polecat toast",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, _, added, w := canonicalWithPolecats(t, true, "toast")
			resume := tt.resume
			if tt.onOrigin {
				resume = added["toast"].Branch
			}
			if tt.staleOnOrigin {
				// Fetch the branch through the bare repository the manager
				// uses, before the counting wrapper is installed, so the
				// remote-tracking ref exists when the full fetch fails.
				repo, err := mgr.repoBase()
				if err != nil {
					t.Fatalf("repoBase: %v", err)
				}
				if err := repo.FetchBranch("origin", resume); err != nil {
					t.Fatalf("warm the remote-tracking ref: %v", err)
				}
			}
			log := &fetchLog{failFetch: tt.failFetch}
			countFetchesOn(mgr, w, log)
			var warns []string
			mgr.warn = func(format string, args ...any) {
				warns = append(warns, fmt.Sprintf(format, args...))
			}

			// A resume branch missing on origin fails the start-point check
			// after the fetches; this test pins the fetches, not that check.
			_, _ = mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next", ResumeBranch: resume})

			want := make([]string, len(tt.want))
			for i, c := range tt.want {
				want[i] = replaceBranchPlaceholder(c, resume)
			}
			if !reflect.DeepEqual(log.calls, want) {
				t.Errorf("reuse fetches = %q, want %q", log.calls, want)
			}

			// A failed fetch is warned, and the warning is the case's only
			// one: the branch retry happens either because the branch is
			// missing or because the full fetch failed, and each says which
			// fetch failed.
			var fetchWarns []string
			for _, warning := range warns {
				if strings.Contains(warning, "fetch") {
					fetchWarns = append(fetchWarns, warning)
				}
			}
			wantWarn := replaceBranchPlaceholder(tt.wantWarn, resume)
			if wantWarn == "" {
				if len(fetchWarns) != 0 {
					t.Errorf("reuse warned %q, want no fetch warning", fetchWarns)
				}
			} else if len(fetchWarns) != 1 || !strings.Contains(fetchWarns[0], wantWarn) {
				t.Errorf("reuse fetch warnings = %q, want one carrying %q", fetchWarns, wantWarn)
			}
		})
	}
}

// replaceBranchPlaceholder names the resumed branch in a wanted fetch or
// warning, which the table cannot know before the seat exists.
func replaceBranchPlaceholder(text, branch string) string {
	return strings.ReplaceAll(text, "<branch>", branch)
}

// TestReuseFetchFallsBackToWorktree: with no bare repository under the rig,
// the reuse path fetches from the worktree, and its warning names the repo it
// really used: the worktree, not the bare repository that is missing.
func TestReuseFetchFallsBackToWorktree(t *testing.T) {
	t.Parallel()
	mgr, bare, _, added, w := canonicalWithPolecats(t, true, "toast")
	if err := os.RemoveAll(bare); err != nil {
		t.Fatalf("remove the bare repo: %v", err)
	}
	// Both fetches fail: the full fetch, and the branch retry it triggers.
	log := &fetchLog{failFetch: true, failBranch: true}
	countFetchesOn(mgr, w, log)
	var warns []string
	mgr.warn = func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf(format, args...))
	}

	// With no bare repo the seat's git state cannot be judged, so the reuse
	// gate refuses; AllowRecovery on the seat's own branch is what lets the
	// reuse run on, to the fetch it pins.
	resume := added["toast"].Branch
	// The reuse stops at the start-point check after the fetches; this test
	// pins the fetches and their warnings, not that check.
	_, _ = mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next", ResumeBranch: resume, AllowRecovery: true})

	want := []string{"fetch origin", "fetch origin " + resume}
	if !reflect.DeepEqual(log.calls, want) {
		t.Errorf("reuse fetches = %q, want %q", log.calls, want)
	}
	var fetchWarns []string
	for _, warning := range warns {
		if strings.Contains(warning, "fetch") {
			fetchWarns = append(fetchWarns, warning)
		}
	}
	wantFull := "could not fetch origin for polecat toast"
	wantBranch := "could not fetch resume branch " + resume + " on the worktree"
	if len(fetchWarns) != 2 || !strings.Contains(fetchWarns[0], wantFull) || !strings.Contains(fetchWarns[1], wantBranch) {
		t.Errorf("reuse fetch warnings = %q, want %q then %q", fetchWarns, wantFull, wantBranch)
	}
}
