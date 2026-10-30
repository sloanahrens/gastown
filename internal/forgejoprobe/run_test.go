package forgejoprobe

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The run's world: a town holding the rig's repository, a config dir, the
// probe's temporary directory, and the two tokens and the decoy file the cases
// use. The probe's own temporary directory is runWork, so the worktree it cuts is
// runWT inside it.
const (
	runAPI      = "http://forgejo.test/api/v1"
	runWeb      = "http://forgejo.test"
	runPushURL  = "http://forgejo.test/acme/rig.git"
	runTown     = "/town"
	runBare     = "/town/rig/.repo.git"
	runWork     = "/work/forgejo-probe.1"
	runCopy     = "/work/forgejo-probe.1/copy"
	runWT       = "/work/forgejo-probe.1/wt"
	runHome     = "/home/furiosa"
	runRecord   = "/cfg/gt/probe-rig.record"
	runBranch   = "land/probe-rig"
	runSHA      = "cafebabecafebabecafebabecafebabecafebabe"
	runContext  = "ci / gate (push)"
	runBrokerTK = "brokertoken-zqxvjkcd"
	runStandIn  = "stub-admin-token"
	runDecoyTK  = "decoytoken-zqxvjkcd"
	runPushRef  = runSHA + ":refs/heads/" + runBranch
	runFetchRef = "refs/remotes/origin/main:refs/heads/" + runBranch
)

// The gate workflows a case can hand the copy in place of the real one.
const (
	runGateDoc  = "name: ci\non: push\njobs:\n  gate:\n    runs-on: ubuntu-latest\n"
	runTwoJobs  = "name: ci\njobs:\n  lint:\n    runs-on: ci-base\n  gate:\n    runs-on: ci-base\n"
	runRenamedA = "name: build\njobs:\n  check:\n    runs-on: ci-base\n"
)

// runTailLog is the failing job's log, whose last lines name the failure.
const runTailLog = "the failing package is named here\nFAIL\tTestGate\t0.01s\n"

// fakeRepoGit models the git side of a run: the clone, the copy's gate file, the
// rig's repository (its commit, its worktrees and its pre-push hook) and the
// push. A push from a worktree of the rig's repository runs hook, when the rig has
// one, with the remote ref it pushes to; a push from the copy never does. An argv
// the fake does not model fails, so a test sees every call the code makes.
type fakeRepoGit struct {
	gateDoc     string
	gateMissing bool
	sha         string
	bareHasSHA  bool
	hook        func(remoteRef string) error
	calls       []rigCall
	worktrees   map[string]bool
	added       []string
	removed     []string
	hookRuns    []string
	pushes      []string
	// cloneErr fails the clone when set, as an unreachable GitHub does.
	cloneErr error
}

func (g *fakeRepoGit) Run(dir string, env []string, args ...string) (string, error) {
	g.calls = append(g.calls, rigCall{dir: dir, env: slices.Clone(env), args: slices.Clone(args)})
	switch args[0] {
	case "clone":
		return "", g.cloneErr
	case "show":
		if g.gateMissing {
			return "", errorString("fatal: path '" + args[1] + "' does not exist")
		}
		return g.gateDoc, nil
	case "config":
		return runPushURL + "\n", nil
	case "rev-parse":
		if dir == runBare && !g.bareHasSHA {
			return "", exitError{code: 1}
		}
		return g.sha + "\n", nil
	case "worktree":
		return g.worktree(args)
	case "push":
		return "", g.push(dir, args[len(args)-1])
	}
	return "", errorString("fakeRepoGit: no answer for " + strings.Join(args, " "))
}

// worktree models a worktree add at args[3] and a remove of args[3]: the add is
// the one the pusher cuts, and a remove of a worktree never added is an error.
func (g *fakeRepoGit) worktree(args []string) (string, error) {
	if len(args) < 4 {
		return "", errorString("fakeRepoGit: no answer for " + strings.Join(args, " "))
	}
	wt := args[3]
	switch args[1] {
	case "add":
		g.worktrees[wt] = true
		g.added = append(g.added, wt)
		return "", nil
	case "remove":
		if !g.worktrees[wt] {
			return "", errorString("fatal: '" + wt + "' is not a working tree")
		}
		delete(g.worktrees, wt)
		g.removed = append(g.removed, wt)
		return "", nil
	}
	return "", errorString("fakeRepoGit: no answer for " + strings.Join(args, " "))
}

// push sends a refspec. From a worktree of the rig's repository the rig's hook
// answers first; a refusal is the push's error, with the hook's own words.
func (g *fakeRepoGit) push(dir, refspec string) error {
	if _, remoteRef, ok := strings.Cut(refspec, ":"); ok && g.worktrees[dir] && g.hook != nil {
		g.hookRuns = append(g.hookRuns, dir)
		if err := g.hook(remoteRef); err != nil {
			return err
		}
	}
	g.pushes = append(g.pushes, refspec)
	return nil
}

// errorString is an error whose text is the string it was made from.
type errorString string

func (e errorString) Error() string { return string(e) }

// refuseLandBranches is a rig's pre-push that refuses the probe's branch.
func refuseLandBranches(remoteRef string) error {
	if strings.HasPrefix(remoteRef, "refs/heads/land/") {
		return errorString("rig pre-push: refusing " + remoteRef + "; land branches are pushed by the lander, not from here")
	}
	return nil
}

// allowAll is a rig's pre-push that lets every push through.
func allowAll(string) error { return nil }

// runFixture is the world one run sees: its git and Forgejo, the record file
// system, the clock, the environment, the files it can read, and its streams.
type runFixture struct {
	git     *fakeRepoGit
	forge   *fakeForgejo
	fsys    *recordFS
	clock   *fakeClock
	files   map[string]string
	env     map[string]string
	rigRepo bool
	noGit   bool
	// mkdirErr and statErr fail the work directory and the rig repository's stat when set.
	mkdirErr error
	statErr  error
	removed  []string
	stdout   strings.Builder
	stderr   strings.Builder
}

// newRunFixture is the default case: the rig's repository present, the copy's gate
// the standard one, a green gate on the first status read, and the broker's token
// in the environment.
func newRunFixture() *runFixture {
	return &runFixture{
		git: &fakeRepoGit{
			gateDoc:    runGateDoc,
			sha:        runSHA,
			bareHasSHA: true,
			worktrees:  map[string]bool{},
		},
		forge: &fakeForgejo{
			sha:     runSHA,
			context: runContext,
			plan:    []string{"success"},
			jobName: "gate",
			jobLog:  runTailLog,
		},
		fsys:    newRecordFS(),
		clock:   &fakeClock{now: recordWhen},
		files:   map[string]string{},
		env:     map[string]string{"GT_AUTH_FORGEJO_TOKEN": runBrokerTK, "HOME": runHome, "XDG_CONFIG_HOME": "/cfg"},
		rigRepo: true,
	}
}

// deps is the run's seams, each one a fake of this fixture.
func (fx *runFixture) deps() Deps {
	return Deps{
		Git: fx.git,
		LookPath: func(file string) (string, error) {
			if fx.noGit {
				return "", exec.ErrNotFound
			}
			return "/usr/bin/" + file, nil
		},
		ReadFile: func(name string) ([]byte, error) {
			data, ok := fx.files[name]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(data), nil
		},
		HTTP:  &http.Client{Transport: fx.forge},
		Clock: fx.clock,
		FS:    fx.fsys,
		Stat: func(name string) (fs.FileInfo, error) {
			if fx.statErr != nil {
				return nil, fx.statErr
			}
			if name == runBare && fx.rigRepo {
				return nil, nil
			}
			return nil, fs.ErrNotExist
		},
		MkdirTemp: func(string, string) (string, error) {
			if fx.mkdirErr != nil {
				return "", fx.mkdirErr
			}
			return runWork, nil
		},
		RemoveAll: func(path string) error {
			fx.removed = append(fx.removed, path)
			return nil
		},
	}
}

// run runs the probe with args against the fixture and returns its exit code.
func (fx *runFixture) run(args []string) int {
	getenv := func(key string) string { return fx.env[key] }
	return Run(context.Background(), args, getenv, Streams{Stdout: &fx.stdout, Stderr: &fx.stderr}, fx.deps())
}

// output is both streams, as a test reads them for what a run printed.
func (fx *runFixture) output() string { return fx.stdout.String() + fx.stderr.String() }

// gitArgvs is the argv of every git call, joined with spaces.
func (fx *runFixture) gitArgvs() []string {
	out := make([]string, 0, len(fx.git.calls))
	for _, c := range fx.git.calls {
		out = append(out, strings.Join(c.args, " "))
	}
	return out
}

// runArgs is the command line the cases share: the rig, its repository, the
// instance and town, and a short wait. Extra arguments follow and win.
func runArgs(extra ...string) []string {
	base := []string{
		"rig", "--repo", "acme/rig",
		"--api-url", runAPI, "--web-url", runWeb, "--town-root", runTown,
		"--timeout", "20", "--poll-interval", "1",
	}
	return append(base, extra...)
}

// expect fails the test with desc when ok is false. Each desc names the behaviour
// it checks, so a failure reads as the broken behaviour.
func expect(t *testing.T, ok bool, desc string) {
	t.Helper()
	if !ok {
		t.Errorf("%s", desc)
	}
}

// anyContains reports whether one of the strings holds sub.
func anyContains(list []string, sub string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, sub) })
}

// recordFields reads the key=value lines of a record into a map.
func recordFields(data string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		k, v, _ := strings.Cut(line, "=")
		fields[k] = v
	}
	return fields
}

// TestRunHelp holds the help request: the flag list prints, exits 0, touches
// nothing and writes nothing on stderr.
func TestRunHelp(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			fx := newRunFixture()
			if rc := fx.run([]string{flag}); rc != ExitOK {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, rc, fx.stderr.String())
			}
			// Each default is read from the constant that holds it, so a default
			// the text no longer names fails here.
			for _, want := range []string{
				"Usage: forgejo-probe <rig> --repo OWNER/NAME",
				"else " + DefaultAPIURL,
				"--town-root",
				"$GT_TOWN_ROOT",
				"--dry-run",
				"(default: " + DefaultMainBranch + ")",
				"(default: 1200)",
				"(default: 15)",
				"Exit codes:",
				"3    green, but the rig's hooks were not exercised, so no record was written",
			} {
				if !strings.Contains(fx.stdout.String(), want) {
					t.Errorf("stdout = %q, want it to hold %q", fx.stdout.String(), want)
				}
			}
			if strings.Contains(fx.stdout.String(), "not wired") {
				t.Errorf("stdout still names the run as not wired")
			}
			if fx.stderr.Len() != 0 || len(fx.git.calls) != 0 || len(fx.forge.calls) != 0 {
				t.Errorf("stderr = %q, git calls = %d, forge calls = %d; want a help request to touch nothing",
					fx.stderr.String(), len(fx.git.calls), len(fx.forge.calls))
			}
		})
	}
}

// TestRunNoGit holds the PATH check: a host without git exits 1 before the run.
func TestRunNoGit(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	fx.noGit = true
	if rc := fx.run(runArgs()); rc != ExitFailure {
		t.Fatalf("run with no git = %d, want 1 (stderr: %s)", rc, fx.stderr.String())
	}
	if !strings.Contains(fx.stderr.String(), "forgejo-probe: git is not on PATH") {
		t.Errorf("stderr = %q, want the missing git named", fx.stderr.String())
	}
	if fx.stdout.Len() != 0 || len(fx.git.calls) != 0 || len(fx.forge.calls) != 0 {
		t.Errorf("a failed start touched something: stdout %q, git %d, forge %d",
			fx.stdout.String(), len(fx.git.calls), len(fx.forge.calls))
	}
}

// TestRunUsageRefusals holds the refused command lines: each exits 2 with its
// message and the usage hint, and touches nothing.
func TestRunUsageRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		msg  string
	}{
		{name: "a missing rig", args: []string{"--repo", "acme/rig"}, msg: "<rig> is required"},
		{name: "a missing --repo", args: []string{"rig"}, msg: "--repo is required"},
		{name: "--repo without OWNER/NAME", args: []string{"rig", "--repo", "not-a-repo"}, msg: "--repo 'not-a-repo' must be OWNER/NAME"},
		{name: "a rig name with a slash", args: []string{"bad/rig", "--repo", "acme/rig"}, msg: "<rig> 'bad/rig' must match [A-Za-z0-9._-]+"},
		{name: "a zero timeout", args: runArgs("--timeout", "0"), msg: "--timeout '0' must be a positive integer"},
		{name: "--town-root without a value", args: []string{"rig", "--repo", "acme/rig", "--town-root"}, msg: "--town-root needs a value"},
		{name: "a non-numeric timeout", args: runArgs("--timeout", "soon"), msg: "--timeout 'soon' must be a positive integer"},
		{name: "an unknown option", args: runArgs("--nope"), msg: "unknown option: --nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newRunFixture()
			if rc := fx.run(tc.args); rc != ExitUsage {
				t.Fatalf("run(%v) = %d, want 2 (stderr: %s)", tc.args, rc, fx.stderr.String())
			}
			expect(t, strings.Contains(fx.stderr.String(), tc.msg), "the refusal names the flag: "+tc.msg)
			expect(t, strings.Contains(fx.stderr.String(), "usage: forgejo-probe --help"), "the usage hint follows the refusal")
			expect(t, fx.stdout.Len() == 0, "nothing reaches stdout on a refusal")
			expect(t, len(fx.git.calls) == 0 && len(fx.forge.calls) == 0, "a refused command line touches no git and no Forgejo")
		})
	}
}

// TestRunGreen holds the green path: the run clones the copy in full, pushes main
// from the rig's repository as the probe branch, deletes the branch under its
// encoded name, unregisters the worktree with remove, and writes the record of the
// green verdict. The token reaches no argv, output or file.
func TestRunGreen(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	rc := fx.run(runArgs())
	if rc != ExitOK {
		t.Fatalf("rc = %d, want 0; output:\n%s", rc, fx.output())
	}
	out := fx.output()
	argvs := fx.gitArgvs()
	rec, recorded := fx.fsys.files[runRecord]
	fields := recordFields("")
	if recorded {
		fields = recordFields(rec.data)
	}
	expect(t, anyContains(argvs, "clone --quiet"), "the copy is cloned in full")
	expect(t, !anyContains(argvs, "--depth"), "no shallow flag is used")
	expect(t, slices.Contains(fx.git.pushes, runPushRef), "main is pushed as the probe branch")
	expect(t, slices.ContainsFunc(fx.git.calls, func(c rigCall) bool { return c.dir == runBare }),
		"the candidate is pushed from the rig's own repository")
	expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the probe branch is deleted")
	expect(t, fx.forge.count(http.MethodDelete, "/repos/acme/rig/branches/land%2Fprobe-rig") == 1,
		"the deletion names the encoded branch")
	expect(t, strings.Contains(fx.stderr.String(), "green"), "the run reports the gate green")
	expect(t, strings.Contains(fx.stderr.String(), runContext), "the run names the context it waited on")
	expect(t, len(fx.git.added) == 1 && slices.Equal(fx.git.added, fx.git.removed),
		"the worktree is unregistered after the push")
	expect(t, slices.Equal(fx.removed, []string{runWork}), "the worktree directory is gone with its temp dir")
	expect(t, !anyContains(argvs, "prune"), "the unregistration is worktree remove, not a prune of the shared repo")
	expect(t, len(fx.git.added) == 1 && strings.HasPrefix(fx.git.added[0], runWork+"/"),
		"the throwaway worktree sits in the probe's temp dir")
	expect(t, !anyContains(argvs, runBrokerTK), "the admin token never reaches the git argv")
	expect(t, !strings.Contains(out, runBrokerTK), "the admin token never reaches the output")
	expect(t, recorded, "a green run writes the record")
	expect(t, fields["rig"] == "rig", "the record names the rig")
	expect(t, fields["repo"] == "acme/rig", "the record names the repository")
	expect(t, fields["commit"] == runSHA, "the record names the probed commit")
	expect(t, fields["context"] == runContext, "the record names the gate context")
	expect(t, fields["verdict"] == "green", "the record names the green verdict")
	expect(t, fields["time"] == "2026-10-30T02:51:25Z", "the record names the time")
	expect(t, regexp.MustCompile(`^\d+$`).MatchString(fields["epoch"]), "the record carries an epoch to age it with")
	expect(t, recorded && rec.mode == recordMode, "the record is mode 600")
	expect(t, recorded && !strings.Contains(rec.data, runBrokerTK), "the record holds no token")
}

// TestRunHookAllows is the green run behind a rig hook that lets the candidate
// through: the hook runs in the worktree the push rides, and the branch is still
// deleted and the worktree still unregistered.
func TestRunHookAllows(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	fx.rigRepo = true
	fx.git.hook = allowAll
	if rc := fx.run(runArgs()); rc != ExitOK {
		t.Fatalf("rc = %d, want 0; output:\n%s", rc, fx.output())
	}
	expect(t, slices.Equal(fx.git.hookRuns, []string{runWT}), "the rig's hook ran in the push's worktree")
	expect(t, slices.Contains(fx.git.pushes, runPushRef), "the hook ran where the push did")
	expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the allowed push deletes the branch")
	expect(t, len(fx.git.added) == 1, "the allowed push cut a worktree")
	expect(t, slices.Equal(fx.git.added, fx.git.removed), "the allowed worktree is unregistered")
}

// TestRunNoRigRepository is the run whose rig has no repository: the push falls
// back to the copy, the warning names the missing repository, and the run is
// green but proves nothing about the hooks, so it exits 3 and writes no record
// (the overseer's ruling on gt-tpg0p.92).
func TestRunNoRigRepository(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	fx.rigRepo = false
	rc := fx.run(runArgs())
	if rc != ExitHooksNotExercised {
		t.Fatalf("rc = %d, want 3; output:\n%s", rc, fx.output())
	}
	expect(t, strings.Contains(fx.stderr.String(), "no rig repository at "+runBare),
		"the warning names the missing repository")
	expect(t, strings.Contains(fx.stderr.String(), "hooks were not exercised"),
		"the warning says the hooks were not exercised")
	expect(t, slices.Contains(fx.git.pushes, runFetchRef), "the push falls back to the clone")
	expect(t, len(fx.git.added) == 0, "no worktree is cut without a repository")
	expect(t, len(fx.git.hookRuns) == 0, "no hook runs without a repository")
	expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the fallback push still deletes the branch")
	expect(t, strings.Contains(fx.stderr.String(), "NOT exercised"), "the green run names that its hooks were NOT exercised")
	_, recorded := fx.fsys.files[runRecord]
	expect(t, !recorded, "a green run whose hooks did not run writes no record")
}

// TestRunRenamedWorkflow is the context derived from the workflow's own name and
// job, not from a fixed one.
func TestRunRenamedWorkflow(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	fx.git.gateDoc = runRenamedA
	fx.forge.context = "build / check (push)"
	if rc := fx.run(runArgs()); rc != ExitOK {
		t.Fatalf("rc = %d, want 0; output:\n%s", rc, fx.output())
	}
	expect(t, strings.Contains(fx.stderr.String(), "build / check (push)"), "the derived context is reported")
}

// TestRunRefusalsAndFailures holds each refusal and failure: the exit code, the
// message it names, no push or no branch deletion where the case says so, and no
// record. A stale record from an earlier run does not survive a refusal either.
func TestRunRefusalsAndFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// rc is the exit the case expects; zero means ExitFailure, the refusal's code.
		rc     int
		setup  func(fx *runFixture)
		args   []string
		msg    string
		assert func(t *testing.T, fx *runFixture)
	}{
		{
			name:  "a rig with no repository is green but exits 3 and writes no record",
			rc:    ExitHooksNotExercised,
			setup: func(fx *runFixture) { fx.rigRepo = false },
			msg:   "no rig repository at " + runBare,
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, strings.Contains(fx.stderr.String(), "NOT exercised"), "the run names that its hooks were NOT exercised")
				expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the fallback push still deletes the branch")
			},
		},
		{
			name:  "a hook refusing the candidate fails the probe",
			setup: func(fx *runFixture) { fx.git.hook = refuseLandBranches },
			msg:   "rig pre-push: refusing refs/heads/land/probe-rig",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.pushes) == 0, "the refused push never happens")
				expect(t, len(fx.forge.deleted) == 0, "no branch is deleted for a refused push")
				expect(t, fx.forge.reads == 0, "the status is never read")
				expect(t, len(fx.git.added) == 1 && slices.Equal(fx.git.added, fx.git.removed),
					"the refused push cut a worktree and it is unregistered")
			},
		},
		{
			name:  "a repository behind the probed commit names the remedy",
			setup: func(fx *runFixture) { fx.git.bareHasSHA = false },
			msg:   "fetch main in " + runBare,
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.pushes) == 0, "nothing is pushed for an unheld commit")
				expect(t, len(fx.git.added) == 0, "no worktree is cut for an unheld commit")
			},
		},
		{
			name:  "a copy without the gate workflow refuses before any push",
			setup: func(fx *runFixture) { fx.git.gateMissing = true },
			msg:   "no .forgejo/workflows/gate.yml",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.pushes) == 0, "the refusal happens before the push")
				expect(t, len(fx.forge.deleted) == 0, "no branch is deleted after a refusal")
				expect(t, fx.forge.reads == 0, "the status is never read")
			},
		},
		{
			name:  "a workflow with more than one job refuses",
			setup: func(fx *runFixture) { fx.git.gateDoc = runTwoJobs },
			msg:   "has 2 jobs, want exactly one",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.pushes) == 0, "nothing is pushed for an ambiguous context")
				expect(t, fx.forge.reads == 0, "no status is read for an ambiguous context")
			},
		},
		{
			name: "an unexpected status on the repository read fails loudly",
			setup: func(fx *runFixture) {
				fx.forge.repoStatus = http.StatusInternalServerError
				fx.forge.repoBody = "boom\nline two"
			},
			msg: "GET /repos/acme/rig: unexpected HTTP 500 — boom line two",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.calls) == 0, "nothing is cloned after an unexpected read")
			},
		},
		{
			name:  "a repository read that never reaches the instance fails loudly",
			setup: func(fx *runFixture) { fx.forge.transportErr = errorString("connection refused") },
			msg:   "connection refused",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.calls) == 0, "nothing is cloned when the repository cannot be read")
			},
		},
		{
			name:  "a work directory that cannot be made fails the run",
			setup: func(fx *runFixture) { fx.mkdirErr = errorString("no space left on device") },
			msg:   "could not create the probe's work directory: no space left on device",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.calls) == 0, "nothing is cloned without a work directory")
			},
		},
		{
			name:  "a copy that cannot be cloned fails the run",
			setup: func(fx *runFixture) { fx.git.cloneErr = errorString("network unreachable") },
			msg:   "git clone failed: network unreachable",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.pushes) == 0, "nothing is pushed from a copy that was not cloned")
			},
		},
		{
			name:  "a rig repository that cannot be statted fails the run",
			setup: func(fx *runFixture) { fx.statErr = errorString("permission denied") },
			msg:   "could not stat the rig repository " + runBare + ": permission denied",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.pushes) == 0, "nothing is pushed when the rig repository cannot be read")
			},
		},
		{
			name: "a repository the instance does not hold fails loudly",
			args: runArgs("--repo", "acme/missing"),
			msg:  "GET /repos/acme/missing returned 404",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, len(fx.git.calls) == 0, "nothing is cloned for a missing repository")
			},
		},
		{
			name: "a missing admin token names the broker, and a decoy in HOME is not read",
			setup: func(fx *runFixture) {
				delete(fx.env, "GT_AUTH_FORGEJO_TOKEN")
				fx.files[runHome+"/forgejo/.env"] = "FORGEJO_ADMIN_TOKEN=" + runDecoyTK + "\n"
			},
			msg: "sudo -u gt-forge gt-approve run forgejo-probe",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, strings.Contains(fx.stderr.String(), "no admin token"), "the missing token is named in the error")
				expect(t, len(fx.forge.calls) == 0, "no request is sent without a token")
				expect(t, len(fx.git.calls) == 0, "no git runs without a token")
				expect(t, !strings.Contains(fx.output(), runDecoyTK), "the file's token never reaches the output")
			},
		},
		{
			name:  "a named token file that is absent fails loudly",
			setup: func(fx *runFixture) { delete(fx.env, "GT_AUTH_FORGEJO_TOKEN") },
			args:  runArgs("--admin-token-file", "/absent.env"),
			msg:   "no admin token",
			assert: func(t *testing.T, fx *runFixture) {
				expect(t, strings.Contains(fx.stderr.String(), "sudo -u gt-forge gt-approve run forgejo-probe"),
					"the error names the broker command")
				expect(t, len(fx.forge.calls) == 0, "no request is sent without a token")
			},
		},
		{
			name: "a refusal removes the stale record an earlier run left",
			setup: func(fx *runFixture) {
				fx.fsys.files[runRecord] = &recordEntry{data: "rig=rig\nverdict=green\n", mode: recordMode}
				fx.git.gateMissing = true
			},
			msg: "no .forgejo/workflows/gate.yml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newRunFixture()
			if tc.setup != nil {
				tc.setup(fx)
			}
			args := tc.args
			if args == nil {
				args = runArgs()
			}
			want := tc.rc
			if want == 0 {
				want = ExitFailure
			}
			rc := fx.run(args)
			expect(t, rc == want, fmt.Sprintf("the run exits %d, got %d", want, rc))
			expect(t, strings.Contains(fx.stderr.String(), tc.msg), "the refusal names: "+tc.msg)
			expect(t, !strings.Contains(fx.stderr.String(), "usage: forgejo-probe --help"),
				"a failed run is not a refused command line: no usage hint")
			_, recorded := fx.fsys.files[runRecord]
			expect(t, !recorded, "a refused probe leaves no record")
			if tc.assert != nil {
				tc.assert(t, fx)
			}
		})
	}
}

// TestRunRedTimeoutAndPending holds the verdicts that are not green: a red gate
// fails with the failing job's tail on stdout, a silent or stuck gate times out,
// and a pending, absent or stale status is polled past until a verdict settles.
func TestRunRedTimeoutAndPending(t *testing.T) {
	t.Parallel()

	t.Run("a red gate fails and prints the job log tail", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"failure"}
		rc := fx.run(runArgs())
		expect(t, rc == ExitFailure, "a red probe exits 1")
		expect(t, strings.Contains(fx.stderr.String(), "ci / gate (push) is failure"), "the run reports the failing state")
		expect(t, strings.Contains(fx.stdout.String(), "the failing package is named here"), "the failing job's log tail is printed")
		expect(t, !strings.Contains(fx.stderr.String(), "FAIL\tTestGate"), "the progress carries no log lines: only the tail goes to stdout")
		expect(t, fx.forge.count(http.MethodGet, "/repos/acme/rig/actions/runs") == 1, "the run list is read once")
		expect(t, fx.forge.count(http.MethodGet, "/repos/acme/rig/actions/runs/12/jobs") == 1, "the jobs of the run are read")
		expect(t, fx.forge.count(http.MethodGet, "/repos/acme/rig/actions/jobs/311/logs") == 1, "the job's log is read")
		expect(t, fx.forge.reads == 1, "a red verdict stops the poll")
		expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the probe branch is deleted after a red")
		_, recorded := fx.fsys.files[runRecord]
		expect(t, !recorded, "a red probe writes no record")
	})

	t.Run("a long red log prints only its tail", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"failure"}
		fx.forge.jobLog = "HEAD-MARKER\n" + strings.Repeat("x", 40000) + "\nTAIL-MARKER\n"
		rc := fx.run(runArgs())
		expect(t, rc == ExitFailure, "a long red log still fails the probe")
		expect(t, strings.Contains(fx.stdout.String(), "TAIL-MARKER"), "the tail of a long log is printed")
		expect(t, !strings.Contains(fx.output(), "HEAD-MARKER"), "the head of a long log is dropped")
		expect(t, len(fx.output()) < 20000, "the printed tail is bounded")
	})

	t.Run("a renamed job reads the failed job", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"failure"}
		fx.forge.jobName = "renamed"
		rc := fx.run(runArgs())
		expect(t, rc == ExitFailure, "a renamed job still fails the probe")
		expect(t, fx.forge.count(http.MethodGet, "/repos/acme/rig/actions/jobs/311/logs") == 1,
			"the failed job is read when the named job is absent")
	})

	t.Run("a gate that never reports times out", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"pending"}
		fx.forge.noRun = true
		rc := fx.run(runArgs("--timeout", "2"))
		expect(t, rc == ExitFailure, "a silent gate exits 1")
		expect(t, strings.Contains(fx.stderr.String(), "reported nothing"), "the timeout is reported")
		expect(t, strings.Contains(fx.stderr.String(), "no workflow run tested"), "a silent gate reports there is no run to read")
		expect(t, fx.forge.reads <= 4, "the wait is bounded by the timeout")
		expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the probe branch is deleted after a timeout")
		_, recorded := fx.fsys.files[runRecord]
		expect(t, !recorded, "a timed-out probe writes no record")
		expect(t, fx.forge.countContaining(http.MethodGet, "/actions/jobs") == 0, "a silent gate reads no job log")
	})

	t.Run("a stuck run's log is printed when the gate goes silent", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"pending"}
		rc := fx.run(runArgs("--timeout", "2"))
		expect(t, rc == ExitFailure, "a stuck run exits 1")
		expect(t, strings.Contains(fx.stdout.String(), "the failing package is named here"), "the stuck run's job log is printed")
		expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the probe branch is deleted after a stuck run")
	})

	t.Run("a pending gate is polled until it reports", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"pending", "pending", "success"}
		rc := fx.run(runArgs())
		expect(t, rc == ExitOK, "a pending-then-green gate exits 0")
		expect(t, fx.forge.reads == 3, "the status was read three times")
		expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the probe branch is still deleted")
	})

	t.Run("an unreported context is polled through", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"absent", "success"}
		rc := fx.run(runArgs())
		expect(t, rc == ExitOK, "an unreported context then a green one exits 0")
		expect(t, fx.forge.reads == 2, "the absent status was polled through")
	})

	t.Run("a stale success does not hide the new red", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.forge.plan = []string{"success,failure"}
		rc := fx.run(runArgs())
		expect(t, rc == ExitFailure, "a stale success does not hide the new red")
		expect(t, strings.Contains(fx.stderr.String(), "ci / gate (push) is failure"), "the failing state is reported")
		expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the probe branch is deleted")
	})
}

// TestRunDryRun holds the dry run: it reads the repository and the copy, prints
// what it would do and sends nothing: no push, no deletion, no status read and no
// worktree, and it writes no record.
func TestRunDryRun(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	rc := fx.run(runArgs("--dry-run"))
	expect(t, rc == ExitOK, "a dry run exits 0")
	expect(t, len(fx.git.pushes) == 0, "dry run sends no push")
	expect(t, len(fx.forge.deleted) == 0, "dry run sends no delete")
	expect(t, fx.forge.reads == 0, "dry run sends no status read")
	expect(t, fx.forge.count(http.MethodGet, "/repos/acme/rig") == 1, "dry run reads the repository")
	expect(t, strings.Contains(fx.stderr.String(), "would push "+"main ("+runSHA+") to "+runPushURL+" as "+runBranch),
		"dry run says it would push")
	expect(t, strings.Contains(fx.stderr.String(), "would push from a throwaway worktree of "+runBare),
		"dry run names the worktree the push would ride")
	expect(t, len(fx.git.added) == 0, "dry run cuts no worktree")
	expect(t, strings.Contains(fx.stderr.String(), "would delete branch "+runBranch),
		"dry run says it would delete the branch")
	expect(t, strings.Contains(fx.stderr.String(), "dry run: no push and no deletion was sent"),
		"dry run reports no write was sent")
	_, recorded := fx.fsys.files[runRecord]
	expect(t, !recorded, "dry run writes no record")
	expect(t, strings.Count(fx.stderr.String(), "\n") == 5, "dry run prints the five lines the plan names")
}

// TestRunTokensNeverLeak holds the secrets rule for the two tokens a run can use
// and the decoy file it must never read: the token is in no git argv, no git
// environment, no output stream, no file written and no request URL. The broker's
// token is the one the auth broker hands the run; the stand-in is the override.
func TestRunTokensNeverLeak(t *testing.T) {
	t.Parallel()
	t.Run("the broker's token", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.files[runHome+"/forgejo/.env"] = "FORGEJO_ADMIN_TOKEN=" + runDecoyTK + "\n"
		rc := fx.run(runArgs())
		expect(t, rc == ExitOK, "a run under the broker's token exits 0")
		expect(t, len(fx.forge.calls) > 0 && allCallsPresent(fx.forge.calls, runBrokerTK),
			"every call presents the broker's token")
		assertNoToken(t, fx, runBrokerTK)
		assertNoToken(t, fx, runDecoyTK)
		expect(t, slices.Equal(fx.forge.deleted, []string{runBranch}), "the run still deletes the probe branch")
		expect(t, fx.forge.reads == 1, "the run still reads the gate status")
		_, recorded := fx.fsys.files[runRecord]
		expect(t, recorded, "the run still writes its green record")
	})
	t.Run("the environment override stands in for the broker", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		fx.env = map[string]string{"FORGEJO_ADMIN_TOKEN": runStandIn, "HOME": runHome, "XDG_CONFIG_HOME": "/cfg"}
		rc := fx.run(runArgs())
		expect(t, rc == ExitOK, "a run with the override in the environment exits 0")
		expect(t, len(fx.forge.calls) > 0 && fx.forge.calls[0].token == runStandIn,
			"the environment override is the one presented")
		assertNoToken(t, fx, runStandIn)
	})
}

// allCallsPresent reports whether every request presented token.
func allCallsPresent(calls []fakeCall, token string) bool {
	return slices.ContainsFunc(calls, func(c fakeCall) bool { return c.token == token }) &&
		!slices.ContainsFunc(calls, func(c fakeCall) bool { return c.token != token })
}

// assertNoToken checks that token reached no git argv, no git environment, no
// output stream, no file the run wrote and no request URL.
func assertNoToken(t *testing.T, fx *runFixture, token string) {
	t.Helper()
	for _, c := range fx.git.calls {
		if anyContains(c.args, token) || anyContains(c.env, token) {
			t.Errorf("token %q reached git %v (env %q)", token, c.args, c.env)
		}
	}
	if strings.Contains(fx.output(), token) {
		t.Errorf("token %q reached the output", token)
	}
	for name, e := range fx.fsys.files {
		if strings.Contains(e.data, token) {
			t.Errorf("token %q reached the file %s", token, name)
		}
	}
	for _, c := range fx.forge.calls {
		if strings.Contains(c.path+"?"+c.query, token) {
			t.Errorf("token %q reached the URL %s", token, c.path)
		}
	}
}
