package cmd

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// rehearseLive is the fake live town; the scratch root is outside it.
const (
	rehearseLive = "/live/gt"
	rehearseRoot = "/scratch/rehearsal/town"
	rehearseImg  = "gt-daemon:test"
)

type fakeEntry struct {
	name string
	mode fs.FileMode
}

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return e.mode.IsDir() }
func (e fakeEntry) Type() fs.FileMode          { return e.mode.Type() }
func (e fakeEntry) Info() (fs.FileInfo, error) { return nil, errors.New("not used") }

// rehearseHost is the fake host: a recording filesystem, a fake clock and a
// runner that fails any argv containing one of the failing substrings.
type rehearseHost struct {
	files     map[string][]byte // what readFile serves: the live settings and the scratch state
	failing   []string          // a command whose joined argv contains one of these fails
	runs      [][]string
	writes    []string
	mkdirs    []string
	clock     time.Time
	slept     time.Duration
	heartbeat bool // whether the scratch state.json records a heartbeat
	readErr   map[string]error
	mkdirErr  error
	writeErr  error
}

func newRehearseHost() *rehearseHost {
	return &rehearseHost{
		clock: time.Unix(1_000_000, 0),
		files: map[string][]byte{
			rehearseLive + "/settings/config.json": []byte(`{"landing":{"work_root":"/opt/missing"},"gate":"host.docker.internal:28443"}`),
			rehearseLive + "/settings/daemon.env":  []byte("# comment\nDEEPSEEK_KEY=sk-secret-value\nexport PATH=/usr/bin:/bin\n"),
			rehearseLive + "/mayor/town.json":      []byte(`{"name":"gt"}`),
		},
		heartbeat: true,
	}
}

func (h *rehearseHost) deps(out *bytes.Buffer) rehearseDeps {
	return rehearseDeps{
		out: out,
		run: func(_ context.Context, _ string, _ []string, argv []string) error {
			h.runs = append(h.runs, argv)
			joined := strings.Join(argv, " ")
			for _, f := range h.failing {
				if strings.Contains(joined, f) {
					return errors.New("exit status 1")
				}
			}
			return nil
		},
		readFile: func(p string) ([]byte, error) {
			if err := h.readErr[p]; err != nil {
				return nil, err
			}
			if p == rehearseRoot+"/daemon/state.json" {
				if h.heartbeat {
					return []byte(`{"last_heartbeat":"2026-11-01T12:00:00Z"}`), nil
				}
				return nil, fs.ErrNotExist
			}
			if b, ok := h.files[p]; ok {
				return b, nil
			}
			return nil, fs.ErrNotExist
		},
		readDir: func(p string) ([]os.DirEntry, error) {
			if err := h.readErr[p]; err != nil {
				return nil, err
			}
			return []os.DirEntry{fakeEntry{"config.json", 0}, fakeEntry{"daemon.env", 0}, fakeEntry{"sub", fs.ModeDir}}, nil
		},
		mkdirAll: func(p string, _ os.FileMode) error { h.mkdirs = append(h.mkdirs, p); return h.mkdirErr },
		write: func(p string, data []byte, _ os.FileMode) error {
			h.writes = append(h.writes, p)
			h.files[p] = data
			return h.writeErr
		},
		now:   func() time.Time { return h.clock },
		sleep: func(d time.Duration) { h.clock = h.clock.Add(d); h.slept += d },
	}
}

func rehearseFlagsFor() rehearseFlags {
	return rehearseFlags{liveRoot: rehearseLive, root: rehearseRoot, image: rehearseImg, budget: time.Minute}
}

func TestContainerRehearsalListsEveryFailingCheck(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.failing = []string{"validate", "command -v kubectl", "command -v openshell", "test -e /opt/missing", "/dev/tcp/host.docker.internal/28443"}
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if err == nil {
		t.Fatal("a rehearsal with failing required checks returned nil")
	}
	got := out.String()
	for _, want := range []string{
		"config-validate  FAIL", "binaries         FAIL", "paths            FAIL", "addresses        FAIL", "heartbeat        ok",
		"kubectl: not on the image's PATH", "openshell: not on the image's PATH",
		"/opt/missing: named by a setting", "host.docker.internal:28443: does not accept a TCP connect",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the table lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(err.Error(), "4 required check(s)") {
		t.Errorf("error = %v, want the count of failed checks", err)
	}
}

func TestContainerRehearsalPassesWhenEveryCheckPasses(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	var out bytes.Buffer
	if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out)); err != nil {
		t.Fatalf("error = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "passed every required check") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestContainerRehearsalBlanksDaemonEnvValues(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	var out bytes.Buffer
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	staged := string(h.files[rehearseRoot+"/settings/daemon.env"])
	if want := "# comment\nDEEPSEEK_KEY=\nexport PATH=\n"; staged != want {
		t.Errorf("staged daemon.env = %q, want %q (keys kept, values gone)", staged, want)
	}
	for p, b := range h.files {
		if strings.HasPrefix(p, rehearseRoot) && (strings.Contains(string(b), "sk-secret-value") || strings.Contains(string(b), "/usr/bin")) {
			t.Errorf("%s holds a value from the source daemon.env", p)
		}
	}
	if string(h.files[rehearseRoot+"/settings/config.json"]) != string(h.files[rehearseLive+"/settings/config.json"]) {
		t.Error("config.json was not copied unchanged")
	}
}

func TestContainerRehearsalTouchesNothingLive(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.heartbeat = false
	f := rehearseFlagsFor()
	var out bytes.Buffer
	_ = runDaemonRehearse(context.Background(), f, h.deps(&out))
	if len(h.writes) == 0 {
		t.Fatal("nothing was staged")
	}
	for _, p := range append(append([]string{}, h.writes...), h.mkdirs...) {
		if !pathWithin(p, rehearseRoot) {
			t.Errorf("a write targets %s, outside the scratch root", p)
		}
	}
	for _, argv := range h.runs {
		for _, a := range argv {
			if strings.Contains(a, rehearseLive) {
				t.Errorf("a command names the live town: %v", argv)
			}
		}
		if len(argv) > 3 && argv[3] == "run" && argv[4] == "-d" {
			if !slices.Contains(argv, "none") {
				t.Errorf("the entrypoint container has a network: %v", argv)
			}
		}
	}
}

func TestContainerRehearsalWaitIsBounded(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.heartbeat = false
	f := rehearseFlagsFor()
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), f, h.deps(&out))
	if err == nil || !strings.Contains(out.String(), "heartbeat        FAIL") || !strings.Contains(out.String(), "no heartbeat in 1m0s") {
		t.Fatalf("a container that never heartbeats must fail at the budget; err=%v\n%s", err, out.String())
	}
	if h.slept < f.budget || h.slept > f.budget+rehearsePoll {
		t.Errorf("waited %s on the fake clock, want the %s budget (plus at most one poll)", h.slept, f.budget)
	}
	last := h.runs[len(h.runs)-1]
	if !slices.Contains(last, "rm") {
		t.Errorf("the entrypoint container was not removed; last command %v", last)
	}
}

func TestContainerRehearsalReportsAContainerThatDoesNotStart(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.failing = []string{"run -d"}
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if err == nil || !strings.Contains(out.String(), "the entrypoint container did not start") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
}

func TestContainerRehearsalRefusesBeforeWritingAnything(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*rehearseFlags)
		want   string
	}{
		"no image":       {func(f *rehearseFlags) { f.image = "" }, "--image are required"},
		"relative live":  {func(f *rehearseFlags) { f.liveRoot = "live" }, "required"},
		"root is live":   {func(f *rehearseFlags) { f.root = rehearseLive }, "not the live town"},
		"root in config": {func(f *rehearseFlags) { f.root = rehearseLive + "/settings/x" }, "not the live town"},
		"relative root":  {func(f *rehearseFlags) { f.root = "rel" }, "not the live town"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRehearseHost()
			f := rehearseFlagsFor()
			tc.mutate(&f)
			err := runDaemonRehearse(context.Background(), f, h.deps(&bytes.Buffer{}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if len(h.writes) != 0 || len(h.runs) != 0 {
				t.Errorf("a refused rehearsal wrote %v and ran %v", h.writes, h.runs)
			}
		})
	}
}

func TestContainerRehearsalDefaultRootIsUnderTheLiveRuntime(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	f := rehearseFlagsFor()
	f.root = ""
	_ = runDaemonRehearse(context.Background(), f, h.deps(&bytes.Buffer{}))
	want := filepath.Join(rehearseLive, ".runtime", "rehearsal", "town", "settings", "config.json")
	if len(h.writes) == 0 || !slices.Contains(h.writes, want) {
		t.Errorf("writes = %v, want %s among them", h.writes, want)
	}
}

func TestContainerRehearsalStagingFailuresAreNamed(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		set  func(*rehearseHost)
		want string
	}{
		"settings dir":  {func(h *rehearseHost) { h.readErr = map[string]error{rehearseLive + "/settings": boom} }, "live settings directory"},
		"settings file": {func(h *rehearseHost) { h.readErr = map[string]error{rehearseLive + "/settings/config.json": boom} }, "live setting config.json"},
		"town.json":     {func(h *rehearseHost) { h.readErr = map[string]error{rehearseLive + "/mayor/town.json": boom} }, "mayor/town.json"},
		"mkdir":         {func(h *rehearseHost) { h.mkdirErr = boom }, "create"},
		"write":         {func(h *rehearseHost) { h.writeErr = boom }, "write"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRehearseHost()
			tc.set(h)
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&bytes.Buffer{}))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, boom) {
				t.Fatalf("error = %v, want %q wrapping the cause", err, tc.want)
			}
			if len(h.runs) != 0 {
				t.Errorf("a failed staging still ran %v", h.runs)
			}
		})
	}
}

func TestBlankEnvValuesKeepsKeysOnly(t *testing.T) {
	t.Parallel()
	got := string(blankEnvValues([]byte("A=1\n\n# c=2\nB=x=y\nnoequals\n")))
	if want := "A=\n\n# c=2\nB=\nnoequals\n"; got != want {
		t.Errorf("blankEnvValues = %q, want %q", got, want)
	}
}
