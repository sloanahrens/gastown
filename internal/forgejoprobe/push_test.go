package forgejoprobe

import (
	"errors"
	"io/fs"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// rigCall is one Run the code under test made: the directory, the variables
// added to the child's environment and the argv.
type rigCall struct {
	dir  string
	env  []string
	args []string
}

// rigGit models the git side of a push. The rig's pre-push hook runs only for a
// push from the worktree, where core.hooksPath resolves to the rig's hooks:
// refused is the hook's answer there, and nil lets the push through. Every other
// call is answered from answers, keyed by argv joined with spaces, and an argv
// it does not name fails.
type rigGit struct {
	answers  map[string]gitAnswer
	wt       string
	refused  error
	calls    []rigCall
	hookRuns []string
}

func (g *rigGit) Run(dir string, env []string, args ...string) (string, error) {
	g.calls = append(g.calls, rigCall{dir: dir, env: slices.Clone(env), args: slices.Clone(args)})
	if args[0] == "push" && dir == g.wt {
		g.hookRuns = append(g.hookRuns, dir)
		if g.refused != nil {
			return "", g.refused
		}
	}
	a, ok := g.answers[strings.Join(args, " ")]
	if !ok {
		return "", errors.New("rigGit: no answer for " + strings.Join(args, " "))
	}
	return a.out, a.err
}

// pushes returns the directory of every push the code under test made, in order.
func (g *rigGit) pushes() []string {
	var dirs []string
	for _, c := range g.calls {
		if c.args[0] == "push" {
			dirs = append(dirs, c.dir)
		}
	}
	return dirs
}

// exitError is a git failure that carries its exit status, as the error execGit
// returns for a git that exited does.
type exitError struct{ code int }

func (e exitError) Error() string { return "exit status " + strconv.Itoa(e.code) }
func (e exitError) ExitCode() int { return e.code }

const (
	testBare    = "rig/.repo.git"
	testWork    = "work"
	testWT      = "work/wt"
	testCopy    = "work/copy"
	pushSHA     = "0123abcd"
	testMain    = "main"
	testBranch  = "land/probe-rig"
	testPushURL = "https://forgejo.example/acme/rig.git"
)

// worktreeAnswers are the git answers of a push that cuts a worktree and is
// accepted by the hook.
func worktreeAnswers() map[string]gitAnswer {
	return map[string]gitAnswer{
		"rev-parse --verify --quiet " + pushSHA + "^{commit}":                                  {out: pushSHA + "\n"},
		"worktree add --detach " + testWT + " " + pushSHA:                                      {},
		"push --quiet --force -- " + testPushURL + " " + pushSHA + ":refs/heads/" + testBranch: {},
		"worktree remove --force " + testWT:                                                    {},
	}
}

// pushPusher is a Pusher over the test paths, with the rig's repository present
// or absent as present says, and its log lines collected into logs. The Stat
// seam returns no FileInfo: only its error is read.
func pushPusher(g Git, present bool, logs *[]string) *Pusher {
	return &Pusher{
		Git:        g,
		Bare:       testBare,
		Copy:       testCopy,
		Work:       testWork,
		GitURL:     testPushURL,
		MainBranch: testMain,
		Branch:     testBranch,
		Stat: func(name string) (fs.FileInfo, error) {
			if present {
				return nil, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
		},
		Log: func(msg string) { *logs = append(*logs, msg) },
	}
}

// TestPushFromWorktreeRunsTheHook pins the push that makes the rig's hook run: a
// push from the worktree of the rig's repository. A push from anywhere else
// runs no hook, so a refusal that does not come from the worktree is no proof.
func TestPushFromWorktreeRunsTheHook(t *testing.T) {
	t.Parallel()
	t.Run("a refusing hook fails the push with its text and nothing else is pushed", func(t *testing.T) {
		t.Parallel()
		const hookText = "pre-push: refusing refs/heads/land/probe-rig"
		g := &rigGit{answers: worktreeAnswers(), wt: testWT, refused: errors.New("exit status 1: " + hookText)}
		var logs []string
		p := pushPusher(g, true, &logs)
		_, err := p.Push(pushSHA)
		if err == nil || !strings.Contains(err.Error(), hookText) {
			t.Fatalf("Push = %v, want the hook's refusal text", err)
		}
		if got, want := g.pushes(), []string{testWT}; !reflect.DeepEqual(got, want) {
			t.Fatalf("pushes ran in %q, want only %q", got, want)
		}
		if err := p.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		last := g.calls[len(g.calls)-1]
		if want := []string{"worktree", "remove", "--force", testWT}; last.dir != testBare || !slices.Equal(last.args, want) {
			t.Errorf("last call = dir %q args %q, want dir %q args %q", last.dir, last.args, testBare, want)
		}
	})
	t.Run("an allowing hook records that it ran in the worktree", func(t *testing.T) {
		t.Parallel()
		g := &rigGit{answers: worktreeAnswers(), wt: testWT}
		var logs []string
		p := pushPusher(g, true, &logs)
		if _, err := p.Push(pushSHA); err != nil {
			t.Fatalf("Push: %v", err)
		}
		if want := []string{testWT}; !reflect.DeepEqual(g.hookRuns, want) {
			t.Errorf("hook ran in %q, want %q", g.hookRuns, want)
		}
		if want := []string{testWT}; !reflect.DeepEqual(g.pushes(), want) {
			t.Errorf("pushes ran in %q, want %q", g.pushes(), want)
		}
		add := g.calls[1]
		if !slices.Contains(add.env, "GIT_LFS_SKIP_SMUDGE=1") {
			t.Errorf("worktree add env = %q, want GIT_LFS_SKIP_SMUDGE=1", add.env)
		}
	})
}

// TestPushReportsItsMode pins the mode a push returns: the rig's hooks were
// exercised only by a push from the worktree, and a refused push reports the
// zero value, which is not exercised.
func TestPushReportsItsMode(t *testing.T) {
	t.Parallel()
	t.Run("a push from the worktree exercised the hooks", func(t *testing.T) {
		t.Parallel()
		g := &rigGit{answers: worktreeAnswers(), wt: testWT}
		var logs []string
		mode, err := pushPusher(g, true, &logs).Push(pushSHA)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if mode != PushedFromWorktree || !mode.HooksExercised() || mode.String() != "rig hooks exercised" {
			t.Errorf("mode = %v (%q, exercised %t), want PushedFromWorktree, exercised", mode, mode, mode.HooksExercised())
		}
	})
	t.Run("a push from the clone did not exercise the hooks", func(t *testing.T) {
		t.Parallel()
		g := &rigGit{answers: map[string]gitAnswer{
			"push --quiet --force -- " + testPushURL + " refs/remotes/origin/main:refs/heads/" + testBranch: {},
		}}
		var logs []string
		mode, err := pushPusher(g, false, &logs).Push(pushSHA)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if mode != PushedFromClone || mode.HooksExercised() || mode.String() != "rig hooks NOT exercised: no rig repository" {
			t.Errorf("mode = %v (%q, exercised %t), want PushedFromClone, not exercised", mode, mode, mode.HooksExercised())
		}
	})
	t.Run("a refused push reports the zero value, not exercised", func(t *testing.T) {
		t.Parallel()
		g := &rigGit{answers: worktreeAnswers(), wt: testWT, refused: errors.New("exit status 1")}
		var logs []string
		mode, err := pushPusher(g, true, &logs).Push(pushSHA)
		if err == nil {
			t.Fatal("Push succeeded, want the refusal")
		}
		if mode != PushMode(0) || mode.HooksExercised() {
			t.Errorf("mode = %v on a refused push, want the zero value, not exercised", mode)
		}
	})
}

// TestPushFallsBackToTheCloneLoudly pins the push with no rig repository: the
// exact warning, the copy's refspec, no worktree and no hook.
func TestPushFallsBackToTheCloneLoudly(t *testing.T) {
	t.Parallel()
	g := &rigGit{answers: map[string]gitAnswer{
		"push --quiet --force -- " + testPushURL + " refs/remotes/origin/main:refs/heads/" + testBranch: {},
	}}
	var logs []string
	p := pushPusher(g, false, &logs)
	if _, err := p.Push(pushSHA); err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := "warning: no rig repository at rig/.repo.git, so the probe pushes from the clone and the rig's hooks were not exercised"
	if !reflect.DeepEqual(logs, []string{want}) {
		t.Errorf("log = %q, want %q", logs, []string{want})
	}
	if len(g.calls) != 1 || g.calls[0].dir != testCopy {
		t.Fatalf("calls = %+v, want one push from %q", g.calls, testCopy)
	}
	if len(g.hookRuns) != 0 {
		t.Errorf("the hook ran for a push from the clone")
	}
	if err := p.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(g.calls) != 1 {
		t.Errorf("Cleanup made calls %+v with no worktree to remove", g.calls[1:])
	}
}

// TestPushRefusesAnUnheldCommit pins the two refusals of a rig repository that
// cannot give the commit: rev-parse exiting 1 says it does not hold it, and any
// other failure says the repository could not be read. Either way nothing is
// pushed and no worktree is cut.
func TestPushRefusesAnUnheldCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		wantErr string
	}{
		{
			name:    "the repository does not hold the commit",
			err:     exitError{code: 1},
			wantErr: "the rig repository rig/.repo.git has no 0123abcd, the main the probe would push; fetch main in rig/.repo.git and run the probe again, since the hooks it must exercise live there",
		},
		{
			name:    "the repository could not be read",
			err:     exitError{code: 128},
			wantErr: "could not check the rig repository rig/.repo.git for 0123abcd: exit status 128",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &rigGit{answers: map[string]gitAnswer{
				"rev-parse --verify --quiet " + pushSHA + "^{commit}": {err: tc.err},
			}}
			var logs []string
			_, err := pushPusher(g, true, &logs).Push(pushSHA)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Push = %v, want %q", err, tc.wantErr)
			}
			if len(g.calls) != 1 || g.calls[0].dir != testBare {
				t.Errorf("calls = %+v, want only the rev-parse in %q", g.calls, testBare)
			}
		})
	}
}

// TestPushRefusesADashValue pins the refusal of a value git would read as an
// option, before any git runs.
func TestPushRefusesADashValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		sha   string
		apply func(p *Pusher)
	}{
		{name: "a commit", sha: "-x", apply: func(p *Pusher) {}},
		{name: "a git URL", sha: pushSHA, apply: func(p *Pusher) { p.GitURL = "--upload-pack=sh" }},
		{name: "a main branch", sha: pushSHA, apply: func(p *Pusher) { p.MainBranch = "-main" }},
		{name: "a branch", sha: pushSHA, apply: func(p *Pusher) { p.Branch = "-land" }},
		{name: "a worktree path", sha: pushSHA, apply: func(p *Pusher) { p.Work = "-work" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &rigGit{answers: worktreeAnswers(), wt: testWT}
			var logs []string
			p := pushPusher(g, true, &logs)
			tc.apply(p)
			if _, err := p.Push(tc.sha); err == nil || !strings.Contains(err.Error(), "starts with -") {
				t.Fatalf("Push = %v, want a refusal of the dash value", err)
			}
			if len(g.calls) != 0 {
				t.Errorf("git ran %+v for a refused value", g.calls)
			}
		})
	}
}

// TestPushFailsClosedOnStatError pins that a stat failure other than a missing
// path stops the probe rather than reading as an absent rig repository.
func TestPushFailsClosedOnStatError(t *testing.T) {
	t.Parallel()
	g := &rigGit{answers: map[string]gitAnswer{}}
	var logs []string
	p := pushPusher(g, true, &logs)
	p.Stat = func(string) (fs.FileInfo, error) { return nil, errors.New("permission denied") }
	if _, err := p.Push(pushSHA); err == nil || !strings.Contains(err.Error(), "could not stat the rig repository rig/.repo.git") {
		t.Fatalf("Push = %v, want a stat refusal", err)
	}
	if len(g.calls) != 0 || len(logs) != 0 {
		t.Errorf("calls %+v and logs %q, want neither after a stat failure", g.calls, logs)
	}
}

// TestPushReportsItsFailures pins the two failures of the worktree path: a
// worktree that cannot be cut, and a push that is refused. A refused push
// carries git's output with the credentials of the URL removed.
func TestPushReportsItsFailures(t *testing.T) {
	t.Parallel()
	t.Run("a worktree that cannot be cut is named and nothing is pushed", func(t *testing.T) {
		t.Parallel()
		answers := worktreeAnswers()
		answers["worktree add --detach "+testWT+" "+pushSHA] = gitAnswer{err: errors.New("fatal: already exists")}
		g := &rigGit{answers: answers, wt: testWT}
		var logs []string
		p := pushPusher(g, true, &logs)
		_, err := p.Push(pushSHA)
		if want := "could not add a worktree of rig/.repo.git at 0123abcd under work/wt"; err == nil || err.Error() != want {
			t.Fatalf("Push = %v, want %q", err, want)
		}
		if len(g.pushes()) != 0 {
			t.Errorf("pushes ran %q after the worktree failed", g.pushes())
		}
		if err := p.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		for _, c := range g.calls {
			if slices.Contains(c.args, "remove") {
				t.Errorf("Cleanup removed a worktree the failed add never made: %+v", c)
			}
		}
	})
	t.Run("a refused push names the redacted URL and not its credentials", func(t *testing.T) {
		t.Parallel()
		const credURL = "https://ci:s3cret@forgejo.example/acme/rig.git"
		answers := worktreeAnswers()
		delete(answers, "push --quiet --force -- "+testPushURL+" "+pushSHA+":refs/heads/"+testBranch)
		answers["push --quiet --force -- "+credURL+" "+pushSHA+":refs/heads/"+testBranch] = gitAnswer{}
		g := &rigGit{answers: answers, wt: testWT, refused: errors.New(
			"exit status 128: fatal: unable to access '" + credURL + "/': refused")}
		var logs []string
		p := pushPusher(g, true, &logs)
		p.GitURL = credURL
		_, err := p.Push(pushSHA)
		if err == nil {
			t.Fatal("Push succeeded, want the refusal")
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("error %q carries the URL's password", err.Error())
		}
		want := "git push of main (0123abcd) to https://ci:xxxxx@forgejo.example/acme/rig.git as land/probe-rig failed from work/wt: exit status 128: fatal: unable to access '<repository URL>/': refused"
		if err.Error() != want {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	})
}

// TestPushRedactsEveryForm pins the refused push's error for each URL in
// redactRows: the URL's header and git's echo are both redacted, and no secret
// the URL carried reaches the error.
func TestPushRedactsEveryForm(t *testing.T) {
	t.Parallel()
	for _, row := range redactRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			g := &rigGit{answers: worktreeAnswers(), wt: testWT, refused: errors.New(row.echo)}
			var logs []string
			p := pushPusher(g, true, &logs)
			p.GitURL = row.rawURL
			_, err := p.Push(pushSHA)
			want := "git push of main (0123abcd) to " + row.header + " as land/probe-rig failed from work/wt: " + row.wantEcho
			if err == nil || err.Error() != want {
				t.Fatalf("Push = %v, want %q", err, want)
			}
			if row.secret != "" && strings.Contains(err.Error(), row.secret) {
				t.Fatalf("error carries a secret the URL held")
			}
		})
	}
}

// TestPushFallsBackToTheCloneRefusesLoudly pins the fallback push failing: the
// clone's push error names the copy as the place it ran, and the mode is not
// exercised. No rig repository, no hook, and a refused push all at once.
func TestPushFallsBackToTheCloneRefusesLoudly(t *testing.T) {
	t.Parallel()
	g := &rigGit{answers: map[string]gitAnswer{}}
	var logs []string
	mode, err := pushPusher(g, false, &logs).Push(testSHA)
	if err == nil || !strings.Contains(err.Error(), "failed from "+testCopy+": ") {
		t.Fatalf("Push = %v, want the clone's push refusal naming %q", err, testCopy)
	}
	if mode != PushMode(0) || mode.HooksExercised() {
		t.Errorf("mode = %v on a refused clone push, want the zero value, not exercised", mode)
	}
	if want := []string{testCopy}; !reflect.DeepEqual(g.pushes(), want) {
		t.Errorf("pushes ran in %q, want only %q", g.pushes(), want)
	}
}

// TestPushChildrenNeverInheritTheTokens pins the environment of every git the
// Pusher starts: none carries the probe's credentials or a repository pin, even
// when the probe's own environment holds them. The environment is a fake, so
// the test reads no real one. The four git calls are the rev-parse, the
// worktree add, the push and the worktree remove.
func TestPushChildrenNeverInheritTheTokens(t *testing.T) {
	t.Parallel()
	inherited := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/furiosa",
		"GT_AUTH_FORGEJO_TOKEN=broker-token",
		"FORGEJO_ADMIN_TOKEN=admin-token",
		"FOO_TOKEN=foo-token",
		"GIT_DIR=/elsewhere/.git",
		"GIT_INDEX_FILE=/elsewhere/index",
		"GIT_WORK_TREE=/elsewhere/tree",
	}
	scrubbed := []string{"GT_AUTH_FORGEJO_TOKEN", "FORGEJO_ADMIN_TOKEN", "FOO_TOKEN", "GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE"}

	var argvs [][]string
	var envs [][]string
	g := execGit{
		run: func(cmd *exec.Cmd) error {
			argvs = append(argvs, cmd.Args[1:])
			envs = append(envs, cmd.Env)
			return nil
		},
		environ: func() []string { return inherited },
	}
	var logs []string
	p := pushPusher(g, true, &logs)
	if _, err := p.Push(pushSHA); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := p.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(argvs) != 4 {
		t.Fatalf("git ran %d times, want the four a worktree push starts: %q", len(argvs), argvs)
	}
	for i, env := range envs {
		for _, kv := range env {
			key, _, _ := strings.Cut(kv, "=")
			if slices.Contains(scrubbed, key) {
				// Name the key only: a value here may be a credential.
				t.Errorf("git %q inherited %s", argvs[i], key)
			}
		}
	}
}

// TestCleanupRemovesNeverPrunes pins Cleanup: it removes the worktree and never
// prunes, a failed remove names the prune remedy in its warning, and a second
// call does nothing.
func TestCleanupRemovesNeverPrunes(t *testing.T) {
	t.Parallel()
	t.Run("the worktree is removed and nothing is pruned", func(t *testing.T) {
		t.Parallel()
		g := &rigGit{answers: worktreeAnswers(), wt: testWT}
		var logs []string
		p := pushPusher(g, true, &logs)
		if _, err := p.Push(pushSHA); err != nil {
			t.Fatalf("Push: %v", err)
		}
		if err := p.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		for _, c := range g.calls {
			if slices.Contains(c.args, "prune") {
				t.Errorf("a call pruned the rig repository: %+v", c)
			}
		}
		last := g.calls[len(g.calls)-1]
		if want := []string{"worktree", "remove", "--force", testWT}; last.dir != testBare || !slices.Equal(last.args, want) {
			t.Errorf("Cleanup call = dir %q args %q, want dir %q args %q", last.dir, last.args, testBare, want)
		}
	})
	t.Run("a failing remove names the prune remedy and a second call does nothing", func(t *testing.T) {
		t.Parallel()
		answers := worktreeAnswers()
		answers["worktree remove --force "+testWT] = gitAnswer{err: errors.New("fatal: locked")}
		g := &rigGit{answers: answers, wt: testWT}
		var logs []string
		p := pushPusher(g, true, &logs)
		if _, err := p.Push(pushSHA); err != nil {
			t.Fatalf("Push: %v", err)
		}
		if err := p.Cleanup(); err == nil {
			t.Fatal("Cleanup succeeded, want the remove's failure")
		}
		if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "run 'git -C rig/.repo.git worktree prune' to clear the registration") {
			t.Fatalf("log = %q, want the prune remedy", logs)
		}
		before := len(g.calls)
		if err := p.Cleanup(); err != nil {
			t.Fatalf("second Cleanup: %v", err)
		}
		if len(g.calls) != before {
			t.Errorf("second Cleanup made calls %+v", g.calls[before:])
		}
	})
}
