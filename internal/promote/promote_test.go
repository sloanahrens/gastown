package promote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/lock"
)

// recordingRepo is a Repo a test scripts and reads back: what the target's
// main holds, whether it is an ancestor of the green commit, whether the push
// fails, and every push's refspec, environment and force flag.
type recordingRepo struct {
	tip         string
	tipErr      error
	fetchErr    error
	ancestor    bool
	ancestorErr error
	pushErr     error
	fetches     []fetchCall
	pushes      []pushCall
}

type fetchCall struct {
	remote, refspec string
	env             []string
}

type pushCall struct {
	remote, refspec string
	force           bool
	env             []string
}

func (r *recordingRepo) ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	if r.tipErr != nil {
		return nil, r.tipErr
	}
	if r.tip == "" {
		return nil, nil
	}
	return []git.RemoteRef{{Hash: r.tip, Name: MainRef}}, nil
}

func (r *recordingRepo) FetchRefspecWithEnv(remote, refspec string, env []string) error {
	r.fetches = append(r.fetches, fetchCall{remote: remote, refspec: refspec, env: env})
	return r.fetchErr
}

func (r *recordingRepo) IsAncestor(ancestor, descendant string) (bool, error) {
	if r.ancestorErr != nil {
		return false, r.ancestorErr
	}
	return r.ancestor, nil
}

func (r *recordingRepo) PushWithEnv(remote, refspec string, force bool, env []string) error {
	r.pushes = append(r.pushes, pushCall{remote: remote, refspec: refspec, force: force, env: env})
	return r.pushErr
}

// promoterFixture is a stub-backed Promoter with its log lines and
// escalations collected.
type promoterFixture struct {
	repo     *recordingRepo
	p        *Promoter
	keyFile  string
	logs     []string
	alerts   []string
	pinnedAt time.Time
}

func newPromoterFixture(t *testing.T) *promoterFixture {
	t.Helper()
	fx := &promoterFixture{
		repo:     &recordingRepo{ancestor: true},
		keyFile:  filepath.Join(t.TempDir(), "promote-gastown.key"),
		pinnedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
	}
	fx.p = &Promoter{
		Rig:     "gastown",
		Target:  "git@github.com:example/gastown.git",
		KeyFile: fx.keyFile,
		Repo:    fx.repo,
		Logf:    func(format string, args ...any) { fx.logs = append(fx.logs, fmt.Sprintf(format, args...)) },
		Now:     func() time.Time { return fx.pinnedAt },
		Escalate: func(message string) {
			fx.alerts = append(fx.alerts, message)
		},
	}
	return fx
}

func TestPromoteFastForwardsTheTargetsMain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := gitfake.New()
	rig := filepath.Join(root, "rig.git")
	gh := filepath.Join(root, "github.git")
	f.InitBare(t, rig)
	f.InitBare(t, gh)
	base := f.Commit(t, rig, "main", "main: seed", map[string]string{"a.txt": "one\n"})
	// GitHub's main is where the last promotion left it: behind.
	f.SetRef(t, gh, "refs/heads/main", base)
	green := f.Commit(t, rig, "main", "green", map[string]string{"b.txt": "ok\n"})

	fx := newPromoterFixture(t)
	fx.p.Repo = f.Open(rig)
	fx.p.Target = gh

	st := fx.p.Promote(State{}, green)
	if st.LastPromoted != green {
		t.Errorf("LastPromoted = %q, want %s", st.LastPromoted, green)
	}
	if !st.LastPromotedAt.Equal(fx.pinnedAt) {
		t.Errorf("LastPromotedAt = %v, want %v", st.LastPromotedAt, fx.pinnedAt)
	}
	if st.LastError != "" || st.GitHubDiverged != nil {
		t.Errorf("state = %+v, want no error and no divergence", st)
	}
	if got := f.Ref(gh, MainRef); got != green {
		t.Errorf("target main = %q, want the green commit %s", got, green)
	}
	if len(fx.alerts) != 0 {
		t.Errorf("alerts = %v, want none", fx.alerts)
	}
}

func TestPromoteRecordsTheCommitWhenTheTargetAlreadyHoldsIt(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "green"
	fx.repo.pushErr = errors.New("the push must not run")

	was := State{LastError: "a stale push failure", GitHubDiverged: &Divergence{RemoteMain: "aaaa1111", Commit: "bbbb2222"}}
	st := fx.p.Promote(was, "green")
	if len(fx.repo.pushes) != 0 {
		t.Fatalf("pushes = %+v, want none when the target is already at the commit", fx.repo.pushes)
	}
	if st.LastPromoted != "green" {
		t.Errorf("LastPromoted = %q, want the commit the target already holds", st.LastPromoted)
	}
	if !st.LastPromotedAt.Equal(fx.pinnedAt) {
		t.Errorf("LastPromotedAt = %v, want %v: a first record carries a time", st.LastPromotedAt, fx.pinnedAt)
	}
	if st.LastError != "" || st.GitHubDiverged != nil {
		t.Errorf("state = %+v, want the stale error and divergence cleared: the target is at the commit", st)
	}
}

func TestPromoteAdvancesLastPromotedWithoutRewritingThePushTime(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "green"
	fx.repo.pushErr = errors.New("the push must not run")
	// The last push, which is what LastPromotedAt dates; this promotion
	// pushes nothing, so the time must survive untouched.
	pushedAt := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)

	was := State{LastPromoted: "older", LastPromotedAt: pushedAt}
	st := fx.p.Promote(was, "green")
	if len(fx.repo.pushes) != 0 {
		t.Fatalf("pushes = %+v, want none when the target is already at the commit", fx.repo.pushes)
	}
	if st.LastPromoted != "green" {
		t.Errorf("LastPromoted = %q, want it advanced to the target's commit", st.LastPromoted)
	}
	if !st.LastPromotedAt.Equal(pushedAt) {
		t.Errorf("LastPromotedAt = %v, want %v untouched: nothing was pushed", st.LastPromotedAt, pushedAt)
	}
}

func TestPromoteRecordsAndEscalatesADivergedTargetOnce(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "aaaa1111"
	fx.repo.ancestor = false

	st := fx.p.Promote(State{}, "bbbb2222")
	if len(fx.repo.pushes) != 0 {
		t.Fatalf("pushes = %+v, want none for a diverged target", fx.repo.pushes)
	}
	if st.GitHubDiverged == nil || st.GitHubDiverged.RemoteMain != "aaaa1111" || st.GitHubDiverged.Commit != "bbbb2222" {
		t.Fatalf("GitHubDiverged = %+v, want both shas", st.GitHubDiverged)
	}
	if st.LastError != "" {
		t.Errorf("LastError = %q, want it cleared: a divergence is its own condition", st.LastError)
	}
	if len(fx.alerts) != 1 {
		t.Fatalf("alerts = %v, want one for the divergence", fx.alerts)
	}
	// The same divergence on the next green verdict pages nobody again.
	if again := fx.p.Promote(st, "bbbb2222"); len(fx.alerts) != 1 {
		t.Errorf("alerts = %v, want the divergence escalated once", fx.alerts)
	} else if again.GitHubDiverged == nil {
		t.Errorf("a repeat promotion dropped the divergence record")
	}
}

func TestPromoteRecordsAnUnknownTargetTipAsDivergedAndPushesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := gitfake.New()
	rig := filepath.Join(root, "rig.git")
	gh := filepath.Join(root, "github.git")
	f.InitBare(t, rig)
	f.InitBare(t, gh)
	base := f.Commit(t, rig, "main", "main: seed", map[string]string{"a.txt": "one\n"})
	f.SetRef(t, gh, "refs/heads/main", base)
	// Someone pushed to GitHub main directly: this commit exists nowhere in
	// the rig's repository, so merge-base cannot name it until it is fetched.
	ghTip := f.Commit(t, gh, "main", "github-only", map[string]string{"github.txt": "direct\n"})
	green := f.Commit(t, rig, "main", "green", map[string]string{"b.txt": "ok\n"})

	fx := newPromoterFixture(t)
	fx.p.Repo = f.Open(rig)
	fx.p.Target = gh

	st := fx.p.Promote(State{}, green)
	if st.GitHubDiverged == nil {
		t.Fatalf("GitHubDiverged = nil, want the target's unknown tip recorded as diverged; LastError = %q", st.LastError)
	}
	if st.GitHubDiverged.RemoteMain != ghTip || st.GitHubDiverged.Commit != green {
		t.Errorf("GitHubDiverged = %+v, want remote %s commit %s", st.GitHubDiverged, ghTip, green)
	}
	if st.LastError != "" {
		t.Errorf("LastError = %q, want it cleared: a divergence is its own condition", st.LastError)
	}
	if len(fx.alerts) != 1 {
		t.Fatalf("alerts = %v, want one for the divergence", fx.alerts)
	}
	if got := f.Ref(gh, MainRef); got != ghTip {
		t.Errorf("target main = %q, want %q untouched: a divergence is never pushed", got, ghTip)
	}
	// The same divergence on the next green verdict pages nobody again.
	if again := fx.p.Promote(st, green); len(fx.alerts) != 1 {
		t.Errorf("alerts = %v, want the divergence escalated once", fx.alerts)
	} else if again.GitHubDiverged == nil {
		t.Error("a repeat promotion dropped the divergence record")
	}
}

func TestPromoteRecordsAFailedFetchAsAnErrorAndNotADivergence(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "aaaa1111"
	fx.repo.ancestor = false
	fx.repo.fetchErr = errors.New("fatal: Could not read from remote repository.")

	st := fx.p.Promote(State{}, "bbbb2222")
	if st.LastError == "" {
		t.Fatal("LastError is empty after a failed fetch")
	}
	if st.GitHubDiverged != nil {
		t.Errorf("GitHubDiverged = %+v, want none: an unreachable target is not a divergence", st.GitHubDiverged)
	}
	if len(fx.alerts) != 0 {
		t.Errorf("alerts = %v, want none for an unreachable target", fx.alerts)
	}
	if len(fx.repo.pushes) != 0 {
		t.Errorf("pushes = %+v, want none after a failed fetch", fx.repo.pushes)
	}
}

func TestPromoteRecordsAFailedPushAndRetriesAtTheNextVerdict(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "aaaa1111"
	fx.repo.pushErr = errors.New("fatal: Could not read from remote repository.")

	st := fx.p.Promote(State{}, "bbbb2222")
	if st.LastError == "" {
		t.Fatal("LastError is empty after a failed push")
	}
	if st.LastPromoted != "" {
		t.Errorf("LastPromoted = %q, want it unchanged after a failed push", st.LastPromoted)
	}
	if len(fx.alerts) != 0 {
		t.Errorf("alerts = %v, want none: a failed push is not a divergence", fx.alerts)
	}

	fx.repo.pushErr = nil
	st = fx.p.Promote(st, "bbbb2222")
	if st.LastError != "" || st.LastPromoted != "bbbb2222" {
		t.Errorf("state after the retry = %+v, want the push recorded", st)
	}
	if len(fx.repo.pushes) != 2 {
		t.Errorf("pushes = %+v, want the failed one retried", fx.repo.pushes)
	}
}

func TestPromotePushesExactlyTheGreenCommitToMainWithTheDeployKey(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "aaaa1111"

	fx.p.Promote(State{}, "bbbb2222")
	if len(fx.repo.pushes) != 1 {
		t.Fatalf("pushes = %+v, want one", fx.repo.pushes)
	}
	got := fx.repo.pushes[0]
	if got.refspec != "bbbb2222:"+MainRef {
		t.Errorf("refspec = %q, want %q", got.refspec, "bbbb2222:"+MainRef)
	}
	if got.force {
		t.Error("push was forced; a promotion is fast-forward only")
	}
	if got.remote != fx.p.Target {
		t.Errorf("remote = %q, want the promote target", got.remote)
	}
	env := strings.Join(got.env, " ")
	if !strings.Contains(env, "GIT_SSH_COMMAND=") || !strings.Contains(env, fx.keyFile) || !strings.Contains(env, "IdentitiesOnly=yes") {
		t.Errorf("env = %v, want GIT_SSH_COMMAND with the key file and IdentitiesOnly=yes", got.env)
	}
	if !strings.Contains(env, "BatchMode=yes") {
		t.Errorf("env = %v, want BatchMode=yes so a headless ssh never blocks on a prompt", got.env)
	}

	// The ancestry check fetched the target's tip into the throwaway ref, with
	// the same key and no other ref: a diverged tip is only comparable once
	// its object is local.
	if len(fx.repo.fetches) != 1 {
		t.Fatalf("fetches = %+v, want one before the ancestry check", fx.repo.fetches)
	}
	fetch := fx.repo.fetches[0]
	if want := "+" + MainRef + ":" + TargetMainRef; fetch.refspec != want {
		t.Errorf("fetch refspec = %q, want %q", fetch.refspec, want)
	}
	if fetch.remote != fx.p.Target {
		t.Errorf("fetch remote = %q, want the promote target", fetch.remote)
	}
	if strings.Join(fetch.env, " ") != env {
		t.Errorf("fetch env = %v, want the push's env %v: one identity for the fetch and the push", fetch.env, got.env)
	}
}

func TestPromoteKeepsTheKeyFilePathOutOfLogsAndState(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "aaaa1111"
	// The key file holds a secret this package never reads, let alone stores.
	const keyContents = "-----BEGIN OPENSSH PRIVATE KEY-----never-in-a-log-----END OPENSSH PRIVATE KEY-----"
	if err := os.WriteFile(fx.keyFile, []byte(keyContents), 0o600); err != nil {
		t.Fatal(err)
	}
	// An ssh failure quotes the key it could not load.
	fx.repo.pushErr = errors.New(`Load key "` + fx.keyFile + `": Permission denied`)

	st := fx.p.Promote(State{}, "bbbb2222")
	if strings.Contains(st.LastError, fx.keyFile) {
		t.Errorf("LastError = %q, want the key path scrubbed", st.LastError)
	}
	if !strings.Contains(st.LastError, KeyPlaceholder) {
		t.Errorf("LastError = %q, want the placeholder in the key path's place", st.LastError)
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range append(append([]string{}, fx.logs...), string(data), st.LastError) {
		if strings.Contains(line, fx.keyFile) || strings.Contains(line, keyContents) {
			t.Errorf("recorded %q, want neither the key's path nor its contents", line)
		}
	}

	// A fetch that fails before any push quotes the same key, and is scrubbed
	// the same way.
	fx.repo.pushErr = nil
	fx.repo.fetchErr = errors.New(`Load key "` + fx.keyFile + `": Permission denied`)
	fx.logs = nil
	st = fx.p.Promote(State{}, "cccc3333")
	if strings.Contains(st.LastError, fx.keyFile) || !strings.Contains(st.LastError, KeyPlaceholder) {
		t.Errorf("LastError = %q, want the key path scrubbed on the fetch error path", st.LastError)
	}
	for _, line := range append(append([]string{}, fx.logs...), st.LastError) {
		if strings.Contains(line, fx.keyFile) {
			t.Errorf("recorded %q, want the key's path scrubbed on the fetch error path", line)
		}
	}
}

func TestPromoteSkipsWhileAnotherPromotionHoldsTheLock(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.repo.tip = "aaaa1111"
	fx.p.LockPath = filepath.Join(t.TempDir(), "red-main", "gastown.promote.flock")
	if err := os.MkdirAll(filepath.Dir(fx.p.LockPath), 0o755); err != nil {
		t.Fatal(err)
	}

	unlock, err := lock.FlockAcquire(fx.p.LockPath)
	if err != nil {
		t.Fatalf("FlockAcquire: %v", err)
	}
	defer unlock()

	st := fx.p.Promote(State{}, "bbbb2222")
	if len(fx.repo.pushes) != 0 {
		t.Errorf("pushes = %+v, want none while the lock is held", fx.repo.pushes)
	}
	if st.LastPromoted != "" || st.LastError != "" {
		t.Errorf("state = %+v, want it untouched by a skipped attempt", st)
	}
}

func TestPromoteSkipsARigWithNoTarget(t *testing.T) {
	t.Parallel()
	fx := newPromoterFixture(t)
	fx.p.Target = ""

	st := fx.p.Promote(State{}, "bbbb2222")
	if len(fx.repo.pushes) != 0 || st.LastPromoted != "" || st.LastError != "" {
		t.Errorf("state = %+v, pushes = %+v; want a rig with no target untouched", st, fx.repo.pushes)
	}
}
