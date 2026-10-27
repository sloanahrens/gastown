package polecatimage

import (
	"os"
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

func TestMainDefersToAnotherRunsLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	reply := newRunReply("abc1234")
	reply.label = "9999999"
	tr.reply = reply.answer(t, labelFormat(tr))
	makeLock(t, tr.Cfg.LockDir, "9999", testNow)
	t.Cleanup(tr.Cleanup)
	if err := tr.Main(); err != nil {
		t.Fatalf("Main() failed: %v", err)
	}
	tr.wantLogged(t, "already running (lock "+tr.Cfg.LockDir+")")
	if got := len(tr.callsTo("make")); got != 0 {
		t.Errorf("make was called %d times, want none", got)
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
		t.Errorf("the other run's lock now records pid %q, want 9999", got)
	}
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
