package polecat

import (
	"errors"
	"reflect"
	"testing"
)

// fetchLog records the fetches a manager makes, in order, across every
// repository it opens: "fetch origin" for a full fetch, "fetch origin <branch>"
// for a branch fetch.
type fetchLog struct {
	calls []string
	// failFetch makes a full fetch of origin fail, as an offline remote does.
	failFetch bool
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
			name:   "resume branch missing after the full fetch is fetched",
			resume: "polecat/toast/resume-missing",
			want:   []string{"fetch origin", "fetch origin <branch>"},
		},
		{
			name:          "failed full fetch leaves a stale ref, which is still fetched",
			onOrigin:      true,
			staleOnOrigin: true,
			failFetch:     true,
			want:          []string{"fetch origin", "fetch origin <branch>"},
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
		})
	}
}

// replaceBranchPlaceholder names the resumed branch in a wanted fetch, which
// the table cannot know before the seat exists.
func replaceBranchPlaceholder(call, branch string) string {
	if call == "fetch origin <branch>" {
		return "fetch origin " + branch
	}
	return call
}
