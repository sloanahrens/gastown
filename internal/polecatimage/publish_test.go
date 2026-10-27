package polecatimage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishReply answers the calls a publish makes: docker's two image-inspect
// formats apart, and every other call through fail.
type publishReply struct {
	id, refs result            // docker image inspect: the image's id, its tags
	fail     map[string]result // other calls, keyed by name and arguments
	makeTar  bool              // docker save writes the tarball it is given
}

// callKey is a call as the publishReply table keys it.
func callKey(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func (p publishReply) answer(t *testing.T) func(command) result {
	return func(c command) result {
		switch {
		case c.name == "docker" && hasArg(c.args, imageIDFormat):
			return p.id
		case c.name == "docker" && hasArg(c.args, repoTagsFormat):
			return p.refs
		}
		if res, ok := p.fail[callKey(c.name, c.args...)]; ok {
			return res
		}
		if c.name == "docker" && hasArg(c.args, "save") && p.makeTar {
			for i, a := range c.args {
				if a == "-o" && i+1 < len(c.args) {
					if err := os.WriteFile(c.args[i+1], []byte("tar"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		return result{}
	}
}

// callLines renders the calls to a program as "prog arg arg" lines, in order.
func callLines(tr *testRun, name string) []string {
	var out []string
	for _, c := range tr.callsTo(name) {
		out = append(out, callKey(c.name, c.args...))
	}
	return out
}

// wantCallLines fails unless the program was called exactly with want, in
// order.
func wantCallLines(t *testing.T, tr *testRun, name string, want ...string) {
	t.Helper()
	got := callLines(tr, name)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s calls:\n%s\nwant:\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPublishStamp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   result
		want string
	}{
		{"the image's id and the registry", result{stdout: "sha256:abc\n"}, "sha256:abc " + testRegistry},
		{"a missing image", result{exit: 1, stderr: "no such image"}, " " + testRegistry},
		{"an id with trailing blank lines", result{stdout: "sha256:abc\n\n"}, "sha256:abc " + testRegistry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.reply = publishReply{id: tc.id}.answer(t)
			if got := tr.PublishStamp(); got != tc.want {
				t.Errorf("PublishStamp() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublishPending(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// noRegistry names none, so nothing can be pending.
		noRegistry bool
		// published is the file's contents; nil plants no file.
		published *string
		want      bool
		// wantChecked is whether the image's id was read: a run with no
		// registry asks docker nothing.
		wantChecked bool
	}{
		{name: "no registry", noRegistry: true, published: ptr("sha256:abc " + testRegistry), want: false},
		{name: "no record of a push", published: nil, want: true, wantChecked: true},
		{name: "the push this image needs", published: ptr("sha256:def " + testRegistry), want: true, wantChecked: true},
		{name: "another registry's push", published: ptr("sha256:abc other:5000"), want: true, wantChecked: true},
		{name: "the push that finished", published: ptr("sha256:abc " + testRegistry + "\n"), want: false, wantChecked: true},
		{name: "a record with a blank line after it", published: ptr("sha256:abc " + testRegistry + "\n\n"), want: false, wantChecked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			if tc.noRegistry {
				tr.Cfg.Registry = ""
			}
			if tc.published != nil {
				if err := os.WriteFile(tr.Cfg.Published, []byte(*tc.published), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}}.answer(t)
			if got := tr.PublishPending(); got != tc.want {
				t.Errorf("PublishPending() = %v, want %v", got, tc.want)
			}
			if checked := len(tr.callsTo("docker")) > 0; checked != tc.wantChecked {
				t.Errorf("the image's id was read: %v, want %v", checked, tc.wantChecked)
			}
		})
	}
}

// ptr is a pointer to s, for the table's optional file contents.
func ptr(s string) *string { return &s }

func TestRegistryProblem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		registry string
		noCrane  bool
		want     string
	}{
		{name: "a host and port", registry: testRegistry},
		{name: "an absolute path", registry: "/tmp/records", want: "POLECAT_IMAGE_REGISTRY=/tmp/records is a path, not a host:port"},
		{name: "a relative path", registry: "./records", want: "POLECAT_IMAGE_REGISTRY=./records is a path, not a host:port"},
		{name: "a parent path", registry: "../records", want: "POLECAT_IMAGE_REGISTRY=../records is a path, not a host:port"},
		{name: "a home path", registry: "~/.records", want: "POLECAT_IMAGE_REGISTRY=~/.records is a path, not a host:port"},
		{name: "no crane", registry: testRegistry, noCrane: true, want: "POLECAT_IMAGE_REGISTRY=" + testRegistry + " is set but crane is not installed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Cfg.Registry = tc.registry
			if tc.noCrane {
				tr.look = func(string) (string, error) { return "", errors.New("not found") }
			}
			if got := tr.RegistryProblem(); got != tc.want {
				t.Errorf("RegistryProblem() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublishState(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	if got := tr.PublishState(); got != "not published" {
		t.Errorf("PublishState() = %q before a push, want %q", got, "not published")
	}
	tr.FirstPushed = testRegistry + "/gastown-polecat:dev"
	if got := tr.PublishState(); got != "only partly published" {
		t.Errorf("PublishState() = %q after a push, want %q", got, "only partly published")
	}
}

// TestPublishTagsPushesTheFirstImageAndTagsTheRest is the registry's shape
// (gt-roa0c): one save and push under the first tag, every later tag made on the
// registry with crane tag, and an earlier push's alias left alone.
func TestPublishTagsPushesTheFirstImageAndTagsTheRest(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.TarDir = t.TempDir()
	first := testRegistry + "/gastown-polecat:dev"
	second := testRegistry + "/gastown-polecat:abc1234"
	tr.reply = publishReply{
		refs:    result{stdout: "gastown-polecat:dev\ngastown-polecat:abc1234\nlocalhost:5000/gastown-polecat:dev\n"},
		makeTar: true,
	}.answer(t)
	if err := tr.PublishTags(); err != nil {
		t.Fatalf("PublishTags() failed: %v", err)
	}
	tar := filepath.Join(tr.TarDir, "image.tar")
	wantCallLines(t, tr, "docker",
		"docker image inspect --format "+repoTagsFormat+" gastown-polecat:dev",
		"docker tag gastown-polecat:dev "+first,
		"docker save -o "+tar+" "+first,
		"docker tag gastown-polecat:abc1234 "+second,
	)
	wantCallLines(t, tr, "crane",
		"crane push --insecure "+tar+" "+first,
		"crane tag --insecure "+first+" abc1234",
	)
	if tr.FirstPushed != first {
		t.Errorf("FirstPushed = %q, want the first reference pushed %q", tr.FirstPushed, first)
	}
	if exists(tar) {
		t.Error("the tarball is still there after the push")
	}
	tr.wantLogged(t, "registry: pushed "+first)
	tr.wantLogged(t, "registry: tagged "+second)
}

func TestPublishTagsKeepsOnlyThisImageTags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{"the dev tag", "gastown-polecat:dev", true},
		{"a 7-character commit", "gastown-polecat:abc1234", true},
		{"a longer commit", "gastown-polecat:" + strings.Repeat("a", 40), true},
		{"a short commit", "gastown-polecat:abc123", false},
		{"an uppercase commit", "gastown-polecat:ABC1234", false},
		{"a moving tag", "gastown-polecat:latest", false},
		{"an empty tag", "gastown-polecat:", false},
		{"a reference with no tag", "gastown-polecat", false},
		{"an alias from an earlier push", testRegistry + "/gastown-polecat:abc1234", false},
		{"another image", "other-image:abc1234", false},
		{"a blank line", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.TarDir = t.TempDir()
			tr.reply = publishReply{refs: result{stdout: tc.ref + "\n"}}.answer(t)
			err := tr.PublishTags()
			tagged := len(tr.callsTo("docker")) > 1
			if tagged != tc.want {
				t.Errorf("%q was tagged and pushed: %v, want %v", tc.ref, tagged, tc.want)
			}
			// A tag list with nothing this run may publish is the step's
			// failure, not a quiet success.
			if tc.want && err != nil {
				t.Fatalf("PublishTags() failed: %v", err)
			}
			if !tc.want && !errors.Is(err, errPublish) {
				t.Errorf("PublishTags() error = %v, want the publish step's failure", err)
			}
		})
	}
}

func TestPublishTagsStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	first := testRegistry + "/gastown-polecat:dev"
	second := testRegistry + "/gastown-polecat:abc1234"
	cases := []struct {
		name string
		// refs is what docker lists for the image.
		refs result
		// fail answers the call that breaks, given the scratch directory's
		// tarball path.
		fail func(tar string) map[string]result
		// wantLog is the line the failure leaves.
		wantLog string
		// wantState is the publish state the line and the error carry.
		wantState string
		// notRun is a call that must not happen after the failure.
		notRun string
	}{
		{
			name:      "the tags cannot be read",
			refs:      result{exit: 1, stderr: "no such image\n"},
			wantLog:   "registry: cannot read the tags of gastown-polecat:dev (no such image); the image is not published",
			wantState: "not published",
			notRun:    "docker tag gastown-polecat:dev " + first,
		},
		{
			name: "docker cannot tag",
			refs: result{stdout: "gastown-polecat:dev\n"},
			fail: func(string) map[string]result {
				return map[string]result{callKey("docker", "tag", "gastown-polecat:dev", first): {exit: 1, stderr: "denied\n"}}
			},
			wantLog: "registry: cannot tag " + first + " (denied); the image is not published",
			notRun:  "docker save -o",
		},
		{
			name: "the image cannot be saved",
			refs: result{stdout: "gastown-polecat:dev\n"},
			fail: func(tar string) map[string]result {
				return map[string]result{callKey("docker", "save", "-o", tar, first): {exit: 1, stderr: "disk full\n"}}
			},
			wantLog: "registry: cannot save " + first + " to a tarball (disk full); the image is not published",
			notRun:  "crane push",
		},
		{
			name: "the image cannot be pushed",
			refs: result{stdout: "gastown-polecat:dev\n"},
			fail: func(tar string) map[string]result {
				return map[string]result{callKey("crane", "push", "--insecure", tar, first): {exit: 1, stderr: "unauthorized\n"}}
			},
			wantLog: "registry: pushing " + first + " failed (unauthorized); the image is not published",
		},
		{
			name: "a later tag cannot be made on the registry",
			refs: result{stdout: "gastown-polecat:dev\ngastown-polecat:abc1234\n"},
			fail: func(string) map[string]result {
				return map[string]result{callKey("crane", "tag", "--insecure", first, "abc1234"): {exit: 1, stderr: "manifest unknown\n"}}
			},
			wantLog:   "registry: tagging " + second + " failed (manifest unknown); the image is only partly published",
			wantState: "only partly published",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.TarDir = t.TempDir()
			base := publishReply{refs: tc.refs}.answer(t)
			tr.reply = func(c command) result {
				if tc.fail != nil {
					if res, ok := tc.fail(filepath.Join(tr.TarDir, "image.tar"))[callKey(c.name, c.args...)]; ok {
						return res
					}
				}
				return base(c)
			}
			err := tr.PublishTags()
			if !errors.Is(err, errPublish) {
				t.Fatalf("PublishTags() error = %v, want the publish step's failure", err)
			}
			tr.wantLogged(t, tc.wantLog)
			if tc.wantState != "" && tr.PublishState() != tc.wantState {
				t.Errorf("PublishState() = %q, want %q", tr.PublishState(), tc.wantState)
			}
			if tc.notRun != "" && strings.Contains(strings.Join(callLines(tr, "docker"), "\n")+strings.Join(callLines(tr, "crane"), "\n"), tc.notRun) {
				t.Errorf("%q ran after the failure: %v %v", tc.notRun, callLines(tr, "docker"), callLines(tr, "crane"))
			}
		})
	}
}

func TestPushToRegistryPublishesNothingWithoutARegistry(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	if err := tr.PushToRegistry(); err != nil {
		t.Fatalf("PushToRegistry() failed: %v", err)
	}
	if len(tr.calls) != 0 {
		t.Errorf("calls = %v, want none", tr.calls)
	}
}

func TestPushToRegistryRecordsThePush(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:dev\n"}}.answer(t)
	if err := tr.PushToRegistry(); err != nil {
		t.Fatalf("PushToRegistry() failed: %v", err)
	}
	want := "sha256:abc " + testRegistry + "\n"
	got, err := os.ReadFile(tr.Cfg.Published)
	if err != nil {
		t.Fatalf("reading the record of the push: %v", err)
	}
	if string(got) != want {
		t.Errorf("Published holds %q, want %q", got, want)
	}
	if tr.TarDir != "" {
		t.Errorf("TarDir = %q after the push, want empty", tr.TarDir)
	}
	left, err := filepath.Glob(filepath.Join(tr.Cfg.BuildRoot, "push.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the scratch directories %q are still there", left)
	}
}

// TestFailedPublishFailsClosed is the contract gt-efupr set: a publish that
// cannot finish is logged and the run carries on, unless
// POLECAT_IMAGE_FAIL_CLOSED=1 makes it the run's failure.
func TestFailedPublishFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, tr *testRun)
		wantLog string
	}{
		{
			name:    "the registry is a path",
			prepare: func(_ *testing.T, tr *testRun) { tr.Cfg.Registry = "/tmp/records" },
			wantLog: "registry: POLECAT_IMAGE_REGISTRY=/tmp/records is a path, not a host:port",
		},
		{
			name: "crane is missing",
			prepare: func(_ *testing.T, tr *testRun) {
				tr.look = func(string) (string, error) { return "", errors.New("not found") }
			},
			wantLog: "is set but crane is not installed",
		},
		{
			name: "the image's tags cannot be read",
			prepare: func(t *testing.T, tr *testRun) {
				tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{exit: 1, stderr: "no such image"}}.answer(t)
			},
			wantLog: "registry: cannot read the tags",
		},
		{
			name: "the image cannot be pushed",
			prepare: func(t *testing.T, tr *testRun) {
				base := publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:dev\n"}}.answer(t)
				tr.reply = func(c command) result {
					if c.name == "crane" && hasArg(c.args, "push") {
						return result{exit: 1, stderr: "unauthorized"}
					}
					return base(c)
				}
			},
			wantLog: "registry: pushing",
		},
		{
			name: "the image has no tag that can be published",
			prepare: func(t *testing.T, tr *testRun) {
				tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:latest\n"}}.answer(t)
			},
			wantLog: "registry: gastown-polecat:dev carries no tag to publish",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
					tc.prepare(t, tr)
					err := tr.PushToRegistry()
					tr.wantLogged(t, tc.wantLog)
					if !failClosed {
						if err != nil {
							t.Fatalf("PushToRegistry() error = %v, want the run to carry on", err)
						}
						return
					}
					if err == nil {
						t.Fatal("PushToRegistry() error = nil, want the run's failure")
					}
					if !strings.Contains(err.Error(), "the image is not published to ") || !strings.Contains(err.Error(), ", so this run fails (POLECAT_IMAGE_FAIL_CLOSED=1)") {
						t.Errorf("error = %q, want it to name the state and the switch", err)
					}
				})
			}
		})
	}
}

// TestPushToRegistryDoesNotRecordAPushThatTaggedNothing pins the record of a
// push to a push that happened: an image whose tags are all ones the step
// skips leaves the registry empty, and a stamp there would tell the next run
// the image is published.
func TestPushToRegistryDoesNotRecordAPushThatTaggedNothing(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:latest\n"}}.answer(t)
	if err := tr.PushToRegistry(); err != nil {
		t.Fatalf("PushToRegistry() error = %v, want the best-effort run to carry on", err)
	}
	if _, err := os.Stat(tr.Cfg.Published); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the push was recorded although no tag reached the registry (stat error %v)", err)
	}
}

// TestPushToRegistryReportsAScratchDirectoryItCannotMake drives the save
// directory the push makes under the lock: a run that cannot make it publishes
// nothing, carries on, and under POLECAT_IMAGE_FAIL_CLOSED=1 fails (gt-i893i).
func TestPushToRegistryReportsAScratchDirectoryItCannotMake(t *testing.T) {
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
			blocked := filepath.Join(tr.root, "blocked")
			if err := os.WriteFile(blocked, []byte("a file where a directory belongs"), 0o644); err != nil {
				t.Fatal(err)
			}
			tr.Cfg.BuildRoot = blocked
			tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:dev\n"}}.answer(t)
			err := tr.PushToRegistry()
			tr.wantLogged(t, "registry: cannot make a directory under "+blocked+"; the image is not published")
			if got := len(tr.callsTo("docker")); got != 0 {
				t.Errorf("docker was called %d times, want none: nothing to push into", got)
			}
			if !failClosed {
				if err != nil {
					t.Fatalf("PushToRegistry() error = %v, want the run to carry on", err)
				}
				return
			}
			// The failure's wording is PublishUnfinished's, pinned by
			// TestPublishUnfinishedNamesTheState; here the branch must fail.
			if err == nil {
				t.Error("PushToRegistry() = nil, want the run's failure under the switch")
			}
		})
	}
}

func TestPushToRegistryLogsARecordItCannotWrite(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Published = filepath.Join(tr.root, "missing", "polecat-image.published")
	tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:dev\n"}}.answer(t)
	if err := tr.PushToRegistry(); err != nil {
		t.Fatalf("PushToRegistry() error = %v: a record it cannot write is not the push's failure", err)
	}
	tr.wantLogged(t, "registry: cannot write "+tr.Cfg.Published+"; the next run pushes again")
	tr.wantLogged(t, "registry: pushed "+testRegistry+"/gastown-polecat:dev")
}

func TestFailClosedAndDefer(t *testing.T) {
	t.Parallel()
	t.Run("best effort", func(t *testing.T) {
		t.Parallel()
		tr := newTestRun(t)
		if err := tr.FailClosed("x"); err != nil {
			t.Errorf("FailClosed() = %v, want none while the switch is unset", err)
		}
		if out := tr.output(); out != "" {
			t.Errorf("log = %q, want nothing: a best-effort run only logs its cause", out)
		}
		if err := tr.Defer("deferred: because"); err != nil {
			t.Errorf("Defer() = %v, want none while the switch is unset", err)
		}
		tr.wantLogged(t, "deferred: because")
	})
	t.Run("fail closed", func(t *testing.T) {
		t.Parallel()
		tr := newTestRun(t)
		tr.Cfg.FailClosed = true
		err := tr.FailClosed("x")
		if err == nil || err.Error() != "x (POLECAT_IMAGE_FAIL_CLOSED=1)" {
			t.Errorf("FailClosed() = %v, want x (POLECAT_IMAGE_FAIL_CLOSED=1)", err)
		}
		tr.wantLogged(t, "ERROR: x (POLECAT_IMAGE_FAIL_CLOSED=1)")
		if err := tr.Defer("deferred: because"); err == nil {
			t.Error("Defer() = nil, want the run's failure under the switch")
		}
		tr.wantLogged(t, "deferred: because")
	})
}

func TestPublishUnfinishedNamesTheState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		firstPushed string
		want        string
	}{
		{
			name: "nothing pushed",
			want: "the image is not published to " + testRegistry + ", so this run fails",
		},
		{
			name:        "a first tag pushed",
			firstPushed: testRegistry + "/gastown-polecat:dev",
			want:        "the image is only partly published to " + testRegistry + ", so this run fails",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Cfg.FailClosed = true
			tr.FirstPushed = tc.firstPushed
			err := tr.PublishUnfinished()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("PublishUnfinished() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestRepublishReportsALockItCannotMake keeps a lock path this run cannot make
// apart from one another run holds: the retry fails rather than reporting the
// push as left to that run.
func TestRepublishReportsALockItCannotMake(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}}.answer(t)
	blocked := filepath.Join(tr.root, "blocked")
	if err := os.WriteFile(blocked, []byte("a file where a directory belongs"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.Cfg.LockDir = filepath.Join(blocked, "polecat-image.lock")
	err := tr.Republish()
	if err == nil || !strings.Contains(err.Error(), "cannot take the lock "+tr.Cfg.LockDir) {
		t.Errorf("Republish() = %v, want the lock it cannot make", err)
	}
	if anyCallNamed(tr.callsTo("docker"), "save") {
		t.Error("the push ran without the lock")
	}
	tr.wantLogged(t, "ERROR: cannot take the lock "+tr.Cfg.LockDir)
}

// TestRepublishLockWaitNamesTheLockFailsClosed keeps the fail-closed cause of a
// push left to the lock holder apart from one that was never built.
func TestRepublishLockWaitNamesTheLockFailsClosed(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.FailClosed = true
	tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}}.answer(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow)
	t.Cleanup(tr.Cleanup)
	err := tr.Republish()
	if err == nil || !strings.Contains(err.Error(), "another run holds the lock "+tr.Cfg.LockDir) {
		t.Errorf("Republish() = %v, want the lock that holds the push", err)
	}
	if strings.Contains(err.Error(), "was not built") {
		t.Errorf("Republish() = %v, want the lock named rather than a missing image", err)
	}
}

func TestRepublish(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		pending   bool
		held      bool
		wantLog   string
		wantPush  bool
		wantTaken bool
	}{
		{name: "nothing pending", pending: false},
		{name: "another run holds the lock", pending: true, held: true, wantLog: "registry: already running (lock PLACEHOLDER); the publish waits for the next run"},
		{name: "takes the lock and pushes", pending: true, wantPush: true, wantTaken: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.reply = publishReply{id: result{stdout: "sha256:abc\n"}, refs: result{stdout: "gastown-polecat:dev\n"}}.answer(t)
			if !tc.pending {
				if err := os.WriteFile(tr.Cfg.Published, []byte(tr.PublishStamp()+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.held {
				makeLock(t, tr.Cfg.LockDir, "9999", testNow)
			}
			t.Cleanup(tr.Cleanup)
			if err := tr.Republish(); err != nil {
				t.Fatalf("Republish() failed: %v", err)
			}
			if tc.wantLog != "" {
				tr.wantLogged(t, strings.ReplaceAll(tc.wantLog, "PLACEHOLDER", tr.Cfg.LockDir))
			}
			pushed := anyCallNamed(tr.callsTo("docker"), "save")
			if pushed != tc.wantPush {
				t.Errorf("the image was pushed: %v, want %v", pushed, tc.wantPush)
			}
			if tr.HaveLock != tc.wantTaken {
				t.Errorf("HaveLock = %v, want %v", tr.HaveLock, tc.wantTaken)
			}
			if tc.held {
				if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
					t.Errorf("the other run's lock now records pid %q, want 9999", got)
				}
			}
		})
	}
}
