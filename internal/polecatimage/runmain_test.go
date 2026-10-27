package polecatimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runReply answers a whole Main run: the installed commits gt reports in order,
// the label the image carries (following the last build), and the publish calls
// through publishReply.
type runReply struct {
	installs []string // what `gt version` says, in order; the last repeats
	label    string   // the label the image carries before a build
	info     result   // docker info
	build    result   // make polecat-image
	pgrep    result   // pgrep for a running sling
	list     result   // openshell sandbox list
	fail     map[string]result

	n        int    // how many times gt version was asked
	resolved string // the commit the last build resolved
	built    string // the commit the last build left in the label
}

// newRunReply is a run whose docker is up, no sling is running and no sandbox is
// listed.
func newRunReply(installs ...string) *runReply {
	return &runReply{
		installs: installs,
		pgrep:    result{exit: 1},
		list:     result{stdout: "No sandboxes found.\n"},
	}
}

// labelFormat is the template ImageCommit reads the label with.
func labelFormat(tr *testRun) string {
	return `{{index .Config.Labels "` + tr.Cfg.Label + `"}}`
}

func (rr *runReply) answer(t *testing.T, label string) func(command) result {
	base := publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:dev\n"}}.answer(t)
	return func(c command) result {
		if res, ok := rr.fail[callKey(c.name, c.args...)]; ok {
			return res
		}
		switch {
		case c.name == "gt" && hasArg(c.args, "version"):
			say := ""
			if len(rr.installs) > 0 {
				say = rr.installs[len(rr.installs)-1]
				if rr.n < len(rr.installs) {
					say = rr.installs[rr.n]
				}
			}
			rr.n++
			if say == "" {
				return result{stdout: "gt version (dev: unknown)\n"}
			}
			return result{stdout: "gt version (dev: " + say + ")\n"}
		case c.name == "docker" && hasArg(c.args, "info"):
			return rr.info
		case c.name == "docker" && hasArg(c.args, label):
			if rr.built != "" {
				return result{stdout: rr.built + "\n"}
			}
			return result{stdout: rr.label}
		case c.name == "git" && hasArg(c.args, "rev-parse"):
			want := strings.TrimSuffix(c.args[len(c.args)-1], "^{commit}")
			rr.resolved = want + strings.Repeat("0", 40-len(want))
			return result{stdout: rr.resolved + "\n"}
		case c.name == "git" && hasArg(c.args, "add"):
			if err := os.MkdirAll(c.args[len(c.args)-2], 0o755); err != nil {
				t.Fatal(err)
			}
			return result{}
		case c.name == "git" && hasArg(c.args, "remove"):
			if err := os.RemoveAll(c.args[len(c.args)-1]); err != nil {
				t.Fatal(err)
			}
			return result{}
		case c.name == "make":
			rr.built = rr.resolved
			return rr.build
		case c.name == "pgrep":
			return rr.pgrep
		case c.name == "openshell":
			return rr.list
		}
		return base(c)
	}
}

// TestMainRepublishesACurrentImage is the main path through the publish step
// when no build is needed: the image is current and its push did not finish, so
// the run retries it before it reports current.
func TestMainRepublishesACurrentImage(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	reply := newRunReply("abc1234")
	reply.label = "abc1234"
	tr.reply = reply.answer(t, labelFormat(tr))
	t.Cleanup(tr.Cleanup)
	if err := tr.Main(); err != nil {
		t.Fatalf("Main() failed: %v", err)
	}
	if got := len(tr.callsTo("make")); got != 0 {
		t.Errorf("make was called %d times, want none: the image is current", got)
	}
	if !anyCallNamed(tr.callsTo("docker"), "save") || !anyCallNamed(tr.callsTo("crane"), "push") {
		t.Errorf("the pending push was not retried: %v %v", callLines(tr, "docker"), callLines(tr, "crane"))
	}
	tr.wantLogged(t, "current at abc1234")
}

// TestMainPublishesTheImageItBuilds is the main path through the publish step
// after a build: the image is behind the install, so the run builds it and
// publishes what it built.
func TestMainPublishesTheImageItBuilds(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	reply := newRunReply("abc1234")
	reply.label = "9999999"
	tr.reply = reply.answer(t, labelFormat(tr))
	t.Cleanup(tr.Cleanup)
	if err := tr.Main(); err != nil {
		t.Fatalf("Main() failed: %v", err)
	}
	if got := len(tr.callsTo("make")); got != 1 {
		t.Errorf("make was called %d times, want one build", got)
	}
	tr.wantLogged(t, "building gastown-polecat:dev at abc123400000")
	tr.wantLogged(t, "registry: pushed "+testRegistry+"/gastown-polecat:dev")
	tr.wantLogged(t, "current at abc1234")
	if _, err := os.Stat(tr.Cfg.Published); err != nil {
		t.Errorf("the push was not recorded: %v", err)
	}
}

func TestMainDefersWhenDockerIsUnavailable(t *testing.T) {
	t.Parallel()
	for _, failClosed := range []bool{false, true} {
		name := "best effort"
		if failClosed {
			name = "fail closed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Cfg.FailClosed = failClosed
			reply := newRunReply("abc1234")
			reply.label = "9999999"
			reply.info = result{exit: 1, stderr: "cannot connect"}
			tr.reply = reply.answer(t, labelFormat(tr))
			err := tr.Main()
			tr.wantLogged(t, "deferred: docker unavailable")
			if got := len(tr.callsTo("make")); got != 0 {
				t.Errorf("make was called %d times, want none", got)
			}
			if failClosed && (err == nil || !strings.Contains(err.Error(), "POLECAT_IMAGE_FAIL_CLOSED=1")) {
				t.Errorf("Main() = %v, want the run's failure under the switch", err)
			}
			if !failClosed && err != nil {
				t.Errorf("Main() = %v, want a deferred run to carry on", err)
			}
		})
	}
}

func TestMainDefersWhenASlingIsRunning(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	reply := newRunReply("abc1234")
	reply.label = "9999999"
	reply.pgrep = result{exit: 0}
	tr.reply = reply.answer(t, labelFormat(tr))
	if err := tr.Main(); err != nil {
		t.Fatalf("Main() failed: %v", err)
	}
	tr.wantLogged(t, "deferred: sandbox sling in progress")
	if got := len(tr.callsTo("make")); got != 0 {
		t.Errorf("make was called %d times, want none", got)
	}
}

// TestMainDefersWhenASlingStartsBetweenRounds drives the check each build round
// makes: a sling that started after the run took the lock stops the next build,
// and under POLECAT_IMAGE_FAIL_CLOSED=1 it is the run's failure (gt-i893i).
func TestMainDefersWhenASlingStartsBetweenRounds(t *testing.T) {
	t.Parallel()
	for _, failClosed := range []bool{false, true} {
		name := "best effort"
		if failClosed {
			name = "fail closed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Cfg.FailClosed = failClosed
			// A reinstall between the first build and the round after it, which
			// is where the run asks again whether a sling is running.
			reply := newRunReply("aaaaaaa", "aaaaaaa", "bbbbbbb")
			reply.label = "9999999"
			base := reply.answer(t, labelFormat(tr))
			// No sling runs until the run's first build is done, so the
			// pre-lock check and the first round see none, and the round after
			// the build sees one start.
			built := false
			tr.reply = func(c command) result {
				if c.name == "pgrep" && built {
					return result{exit: 0}
				}
				if c.name == "make" {
					built = true
				}
				return base(c)
			}
			t.Cleanup(tr.Cleanup)
			err := tr.Main()
			tr.wantLogged(t, "deferred: sandbox sling in progress")
			if got := len(tr.callsTo("make")); got != 1 {
				t.Errorf("make was called %d times, want one build: the sling stopped the round after it", got)
			}
			if failClosed {
				// The wording is Defer's, pinned by TestFailClosedAndDefer;
				// here the branch under the switch must fail, not carry on.
				if err == nil {
					t.Error("Main() = nil, want the run's failure under the switch")
				}
				return
			}
			if err != nil {
				t.Errorf("Main() = %v, want a deferred run to carry on", err)
			}
		})
	}
}

func TestMainDefersToAnotherRunsLock(t *testing.T) {
	t.Parallel()
	for _, failClosed := range []bool{false, true} {
		name := "best effort"
		if failClosed {
			name = "fail closed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Cfg.FailClosed = failClosed
			reply := newRunReply("abc1234")
			reply.label = "9999999"
			tr.reply = reply.answer(t, labelFormat(tr))
			makeLock(t, tr.Cfg.LockDir, "9999", testNow)
			t.Cleanup(tr.Cleanup)
			err := tr.Main()
			tr.wantLogged(t, "already running (lock "+tr.Cfg.LockDir+")")
			if failClosed {
				// The image a lock wait leaves unbuilt is not the cause; the
				// run was never asked to build one this run may not touch.
				if err == nil || !strings.Contains(err.Error(), "another run holds the lock "+tr.Cfg.LockDir) {
					t.Errorf("Main() = %v, want the lock the run waited on", err)
				}
				if strings.Contains(err.Error(), "was not built") {
					t.Errorf("Main() = %v, want the lock named rather than a missing image", err)
				}
			} else if err != nil {
				t.Errorf("Main() = %v, want a deferred run to carry on", err)
			}
			if got := len(tr.callsTo("make")); got != 0 {
				t.Errorf("make was called %d times, want none", got)
			}
			if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
				t.Errorf("the other run's lock now records pid %q, want 9999", got)
			}
		})
	}
}

// TestMainReturnsARepublishFailure drives the current image's retry: a lock it
// cannot make is the run's failure, not a push left for the next run.
func TestMainReturnsARepublishFailure(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	reply := newRunReply("abc1234")
	reply.label = "abc1234"
	tr.reply = reply.answer(t, labelFormat(tr))
	tr.Cfg.LockDir = blockedLockDir(t, tr)
	err := tr.Main()
	if err == nil || !strings.Contains(err.Error(), "cannot take the lock") {
		t.Errorf("Main() = %v, want the retry's lock failure", err)
	}
}

// TestMainReturnsALockFailure drives the lock a build runs under: one it cannot
// make fails the run instead of deferring.
func TestMainReturnsALockFailure(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	reply := newRunReply("abc1234")
	reply.label = "9999999"
	tr.reply = reply.answer(t, labelFormat(tr))
	tr.Cfg.LockDir = blockedLockDir(t, tr)
	err := tr.Main()
	if err == nil || !strings.Contains(err.Error(), "cannot take the lock") {
		t.Errorf("Main() = %v, want the lock failure", err)
	}
	if got := len(tr.callsTo("make")); got != 0 {
		t.Errorf("make was called %d times, want none", got)
	}
}

// TestMainReturnsABuildFailure drives a build that fails: the run answers it
// rather than going on to publish an image it does not have.
func TestMainReturnsABuildFailure(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	reply := newRunReply("abc1234")
	reply.label = "9999999"
	reply.build = result{exit: 1, stderr: "compile error\n"}
	tr.reply = reply.answer(t, labelFormat(tr))
	t.Cleanup(tr.Cleanup)
	err := tr.Main()
	if err == nil || !strings.Contains(err.Error(), "make polecat-image failed at abc123400000") {
		t.Errorf("Main() = %v, want the failed build", err)
	}
	if anyCallNamed(tr.callsTo("docker"), "save") {
		t.Error("the run published an image the build did not produce")
	}
}

// TestMainReturnsAPublishFailure drives a build whose publish fails under
// POLECAT_IMAGE_FAIL_CLOSED=1: the run fails rather than reporting the image
// current.
func TestMainReturnsAPublishFailure(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.FailClosed = true
	reply := newRunReply("abc1234")
	reply.label = "9999999"
	base := reply.answer(t, labelFormat(tr))
	tr.reply = func(c command) result {
		if c.name == "crane" && hasArg(c.args, "push") {
			return result{exit: 1, stderr: "unauthorized"}
		}
		return base(c)
	}
	t.Cleanup(tr.Cleanup)
	err := tr.Main()
	if err == nil || !strings.Contains(err.Error(), "so this run fails (POLECAT_IMAGE_FAIL_CLOSED=1)") {
		t.Errorf("Main() = %v, want the failed publish under the switch", err)
	}
	if strings.Contains(tr.output(), "current at abc1234") {
		t.Error("the run reported current after a publish that failed closed")
	}
}

// blockedLockDir is a lock path under a file, which no run can make.
func blockedLockDir(t *testing.T, tr *testRun) string {
	t.Helper()
	blocked := filepath.Join(tr.root, "blocked")
	if err := os.WriteFile(blocked, []byte("a file where a directory belongs"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocked, "polecat-image.lock")
}

func TestMainFailsWhenTheInstalledCommitIsUnreadable(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = newRunReply("").answer(t, labelFormat(tr))
	err := tr.Main()
	if err == nil || !strings.Contains(err.Error(), "cannot read the installed gt's commit from 'gt version'") {
		t.Errorf("Main() = %v, want the unreadable install", err)
	}
	if exists(tr.Cfg.LockDir) {
		t.Error("the run took the lock before it could learn which commit to build")
	}
}

// TestMainFailsWhenALaterInstallReadFails drives the check inside each round:
// the first read answered, and the one after a reinstall did not, which is the
// run's failure rather than a build of nothing.
func TestMainFailsWhenALaterInstallReadFails(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	reply := newRunReply("abc1234", "")
	reply.label = "9999999"
	tr.reply = reply.answer(t, labelFormat(tr))
	t.Cleanup(tr.Cleanup)
	err := tr.Main()
	if err == nil || !strings.Contains(err.Error(), "cannot read the installed gt's commit from 'gt version'") {
		t.Errorf("Main() = %v, want the unreadable install", err)
	}
	if got := len(tr.callsTo("make")); got != 0 {
		t.Errorf("make was called %d times, want none: there was no commit to build", got)
	}
}

func TestMainFailsWhenTheInstallKeepsMoving(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	reply := newRunReply("aaaaaaa", "aaaaaaa", "bbbbbbb", "ccccccc", "ddddddd")
	reply.label = "9999999"
	tr.reply = reply.answer(t, labelFormat(tr))
	t.Cleanup(tr.Cleanup)
	err := tr.Main()
	if err == nil || !strings.Contains(err.Error(), "the installed gt kept moving") || !strings.Contains(err.Error(), "after 3 builds") {
		t.Errorf("Main() = %v, want the install that kept moving", err)
	}
	if got := len(tr.callsTo("make")); got != 3 {
		t.Errorf("make was called %d times, want %d builds", got, 3)
	}
}

// TestMainPublishesThroughTheTownConfigsRegistryAndCrane is the hand install's
// publish: the environment names neither, so the run takes both from the town's
// settings/config.json, and a key it did not name never reaches its log
// (gt-du3fi).
func TestMainPublishesThroughTheTownConfigsRegistryAndCrane(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{"daemon":{"env":{
		"POLECAT_IMAGE_REGISTRY":"registry.example:5000",
		"POLECAT_IMAGE_CRANE":"/config/bin/crane",
		"ANTHROPIC_API_KEY":"sk-town-sentinel"}}}`)
	cfg, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": town}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	tr := newTestRun(t)
	tr.Cfg.Registry, tr.Cfg.Crane = cfg.Registry, cfg.Crane
	reply := newRunReply("abc1234")
	reply.label = "abc1234"
	tr.reply = reply.answer(t, labelFormat(tr))
	t.Cleanup(tr.Cleanup)
	if err := tr.Main(); err != nil {
		t.Fatalf("Main() failed: %v", err)
	}
	tr.wantLogged(t, "registry: pushed registry.example:5000/gastown-polecat:dev")
	pushed := false
	for _, c := range tr.callsTo("/config/bin/crane") {
		if hasArg(c.args, "push") && c.args[len(c.args)-1] == "registry.example:5000/gastown-polecat:dev" {
			pushed = true
		}
	}
	if !pushed {
		t.Errorf("the town's crane did not push: %v", callLines(tr, "/config/bin/crane"))
	}
	if strings.Contains(tr.output(), "sk-town-sentinel") {
		t.Errorf("the run logged a daemon.env key it did not name: %s", tr.output())
	}
}

// TestMainPublishesNothingWhenNothingNamesARegistry is the other side: the
// environment and the town both name no registry, so the run neither publishes
// nor reports a fault (gt-du3fi).
func TestMainPublishesNothingWhenNothingNamesARegistry(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownImageConfig(t, town, `{"daemon":{"env":{"POLECAT_IMAGE_CRANE":"/config/bin/crane"}}}`)
	cfg, err := ConfigFromEnv(envMap(map[string]string{"POLECAT_IMAGE_TOWN_ROOT": town}))
	if err != nil {
		t.Fatalf("ConfigFromEnv() failed: %v", err)
	}
	tr := newTestRun(t)
	tr.Cfg.Registry = cfg.Registry
	reply := newRunReply("abc1234")
	reply.label = "abc1234"
	tr.reply = reply.answer(t, labelFormat(tr))
	t.Cleanup(tr.Cleanup)
	if err := tr.Main(); err != nil {
		t.Fatalf("Main() failed: %v", err)
	}
	tr.wantLogged(t, "current at abc1234")
	if anyCallNamed(tr.callsTo("crane"), "push") || anyCallNamed(tr.callsTo("docker"), "save") {
		t.Errorf("a run with no registry published: %v %v", callLines(tr, "docker"), callLines(tr, "crane"))
	}
}
