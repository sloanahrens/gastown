package landworker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/promote"
)

// stubPromoteRepo is the git surface a RedMain test gives its Promoter: a
// target main tip, whether it is an ancestor of the green commit, and the
// pushes it saw.
type stubPromoteRepo struct {
	tip      string
	ancestor bool
	pushes   []string
}

func (s *stubPromoteRepo) ListRemoteRefsWithHashes(_, _ string) ([]git.RemoteRef, error) {
	if s.tip == "" {
		return nil, nil
	}
	return []git.RemoteRef{{Hash: s.tip, Name: promote.MainRef}}, nil
}

// FetchRefspecWithEnv is a no-op success: this stub models no objects, so the
// fetch that makes the target's tip comparable changes nothing.
func (s *stubPromoteRepo) FetchRefspecWithEnv(_, _ string, _ []string) error { return nil }

func (s *stubPromoteRepo) IsAncestor(_, _ string) (bool, error) { return s.ancestor, nil }

func (s *stubPromoteRepo) PushWithEnv(_, refspec string, _ bool, _ []string) error {
	s.pushes = append(s.pushes, refspec)
	return nil
}

// promotedGreenFile is a green run at commit, as the post-land runner hands it
// to RedMain.Green.
func promotedGreenFile(commit string) (PostLand, PostLandResult) {
	return PostLand{BeadID: "gt-land1", Commit: commit, Target: "main"}, PostLandResult{}
}

func TestRedMainGreenPromotesTheGreenCommitAndRecordsIt(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	repo := &stubPromoteRepo{tip: "aaaa1111", ancestor: true}
	state := &MemoryMainState{}
	h.r.State = state
	h.r.Promote = &promote.Promoter{
		Rig:     "gastown",
		Target:  "git@github.com:example/gastown.git",
		KeyFile: "/tmp/promote-gastown.key",
		Repo:    repo,
		Logf:    t.Logf,
	}

	pl, res := promotedGreenFile("bbbb2222")
	h.r.Green(context.Background(), "make test-slow", pl, res)

	if len(repo.pushes) != 1 || repo.pushes[0] != "bbbb2222:"+promote.MainRef {
		t.Fatalf("pushes = %v, want the green commit pushed to main", repo.pushes)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastPromoted != "bbbb2222" {
		t.Errorf("LastPromoted = %q, want the green commit in the rig's main state", st.LastPromoted)
	}
	if st.LastRun != "bbbb2222" || st.LastGreen != "bbbb2222" {
		t.Errorf("state = %+v, want the verdict recorded beside the promotion", st)
	}
}

// The state stamps when a green verdict first named its commit, which is the
// clock the townhealth promote field judges a waiting candidate by
// (gt-fn9e6.39). A later verdict at the same commit is the same candidate and
// leaves the stamp alone, so a restart that reruns the tip cannot make an
// unpromoted candidate look fresh.
func TestRedMainGreenStampsTheCandidateOnce(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	state := &MemoryMainState{st: MainState{LastGreen: "bbbb2222", LastRun: "bbbb2222", LastGreenAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}}
	h.r.State = state

	pl, res := promotedGreenFile("bbbb2222")
	h.r.Green(context.Background(), "make test-slow", pl, res)

	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st.LastGreenAt, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("LastGreenAt = %v after a repeat verdict at the same commit, want the first verdict's %v", got, want)
	}

	pl, res = promotedGreenFile("cccc3333")
	h.r.Green(context.Background(), "make test-slow", pl, res)
	if st, _ = state.Load(); st.LastGreenAt.IsZero() || !st.LastGreenAt.After(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("LastGreenAt = %v, want a fresh stamp when the green commit moves", st.LastGreenAt)
	}
}

func TestRedMainRedNeverPromotes(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.rerunExit[pkgA] = 1
	repo := &stubPromoteRepo{tip: "aaaa1111", ancestor: true}
	h.r.State = &MemoryMainState{}
	h.r.Promote = &promote.Promoter{Rig: "gastown", Target: "git@github.com:example/gastown.git", Repo: repo, Logf: t.Logf}

	h.r.Red(context.Background(), "make test-slow",
		PostLand{BeadID: "gt-land1", Commit: "bbbb2222"},
		PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})

	if len(repo.pushes) != 0 {
		t.Errorf("pushes = %v, want none on a red verdict", repo.pushes)
	}
	if open := h.open(t); open[RedMainTitle("gastown", pkgA)] == nil {
		t.Error("the red-main bead was not filed: red handling must be unchanged")
	}
}

func TestMainStateCarriesThePromotionRecordInTheSameFile(t *testing.T) {
	t.Parallel()
	st := MainState{
		LastGreen: "aaaa1111",
		State: promote.State{
			LastPromoted: "bbbb2222",
			LastError:    "",
			GitHubDiverged: &promote.Divergence{
				RemoteMain: "cccc3333",
				Commit:     "bbbb2222",
			},
		},
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"last_promoted"`, `"github_diverged"`, `"remote_main"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("state JSON %s has no %s key: the promotion record must stay in the red-main file's own shape", data, key)
		}
	}
	// Older state, written before promotion existed, still loads.
	var old MainState
	if err := json.Unmarshal([]byte(`{"last_green":"aaaa1111","last_run":"aaaa1111"}`), &old); err != nil {
		t.Fatalf("decoding state written before promotion: %v", err)
	}
	if old.LastGreen != "aaaa1111" || old.LastPromoted != "" {
		t.Errorf("old state = %+v, want the old fields and a zero promotion record", old)
	}
}
