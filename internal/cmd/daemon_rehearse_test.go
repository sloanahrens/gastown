package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/templates"
)

// rehearseLive is the fake live town; the scratch root is outside it.
const (
	rehearseLive = "/live/gt"
	rehearseDir  = "/live/gt/.runtime/rehearsal/run-t1"
	rehearseRoot = rehearseDir + "/town"
	rehearseImg  = "gt-daemon:test"
)

// fakeExit is a command's non-zero exit, as *exec.ExitError reports it.
type fakeExit int

func (e fakeExit) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

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
	captures  [][]string // argv of every capture (docker logs), by the capture seam
	writes    []string
	mkdirs    []string
	clock     time.Time
	slept     time.Duration
	heartbeat bool // whether the scratch state.json records a heartbeat
	readErr   map[string]error
	mkdirErr  error
	writeErr  error
	removed   []string
	removeErr []error           // consumed one per removeAll call
	links     map[string]string // evalSymlinks answers; other paths resolve to themselves
	codes     map[string]int    // exit code of a failing command by substring; default 1
	plugins   []string          // plugin directories under <live>/plugins
	log       string            // what docker logs of the entrypoint container returns
	logErr    error
	home      string // what userHomeDir answers
	homeErr   error  // when set, userHomeDir fails with it
}

func newRehearseHost() *rehearseHost {
	return &rehearseHost{
		clock: time.Unix(1_000_000, 0),
		files: map[string][]byte{
			rehearseLive + "/settings/config.json":         []byte(`{"landing":{"work_root":"/opt/missing","clones":"/h/landing/x","town":"/live/gt/.runtime/y"},"gate":"host.docker.internal:28443","forge":"https://example.test/api/v1","api_key":"sk-live-key","ref":"${KEEP_ME}","auth":{"token":"tok-123"},"daemon":{"container_off":` + offJSON() + `}}`),
			rehearseLive + "/settings/secrets.env":         []byte("RAW=raw-secret-value\n"),
			rehearseLive + "/settings/daemon.env":          []byte("# comment\nDEEPSEEK_KEY=sk-secret-value\nexport PATH=/usr/bin:/bin\n"),
			rehearseLive + "/mayor/town.json":              []byte(`{"name":"gt","dolt":{"host":"127.0.0.1","container_host":"host.docker.internal","port":3307}}`),
			rehearseLive + "/daemon/container/compose.yml": []byte("services:\n  daemon:\n    volumes:\n      - type: bind\n        source: /live/gt\n        target: /live/gt\n      - type: bind\n        source: /h/landing\n        target: /h/landing\n      - type: volume\n        source: v\n        target: /vol\n"),
		},
		heartbeat: true,
		home:      "/home/operator",
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
					code := 1
					for sub, c := range h.codes {
						if strings.Contains(joined, sub) {
							code = c
						}
					}
					return fakeExit(code)
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
			if strings.HasPrefix(p, rehearseLive+"/plugins/") && strings.HasSuffix(p, "/plugin.md") {
				return []byte("+++\n+++\n"), nil
			}
			return nil, fs.ErrNotExist
		},
		readDir: func(p string) ([]os.DirEntry, error) {
			if err := h.readErr[p]; err != nil {
				return nil, err
			}
			if p == rehearseLive {
				return []os.DirEntry{fakeEntry{"plugins", fs.ModeDir}, fakeEntry{"settings", fs.ModeDir}}, nil
			}
			if p == rehearseLive+"/plugins" {
				var es []os.DirEntry
				for _, n := range h.plugins {
					es = append(es, fakeEntry{n, fs.ModeDir})
				}
				return es, nil
			}
			if p != rehearseLive+"/settings" {
				return nil, fs.ErrNotExist
			}
			return []os.DirEntry{fakeEntry{"config.json", 0}, fakeEntry{"daemon.env", 0}, fakeEntry{"secrets.env", 0}, fakeEntry{"sub", fs.ModeDir}}, nil
		},
		mkdirAll: func(p string, _ os.FileMode) error { h.mkdirs = append(h.mkdirs, p); return h.mkdirErr },
		write: func(p string, data []byte, _ os.FileMode) error {
			h.writes = append(h.writes, p)
			h.files[p] = data
			return h.writeErr
		},
		removeAll: func(p string) error {
			h.removed = append(h.removed, p)
			if len(h.removeErr) > 0 {
				err := h.removeErr[0]
				h.removeErr = h.removeErr[1:]
				return err
			}
			return nil
		},
		evalSymlinks: func(p string) (string, error) {
			if t, ok := h.links[p]; ok {
				return t, nil
			}
			return p, nil
		},
		capture: func(_ context.Context, _ string, _ []string, argv []string) ([]byte, error) {
			h.captures = append(h.captures, argv)
			return []byte(h.log), h.logErr
		},
		userHomeDir: func() (string, error) { return h.home, h.homeErr },
		newID:       func() string { return "t1" },
		now:         func() time.Time { return h.clock },
		sleep:       func(d time.Duration) { h.clock = h.clock.Add(d); h.slept += d },
	}
}

func rehearseFlagsFor() rehearseFlags {
	return rehearseFlags{liveRoot: rehearseLive, image: rehearseImg, budget: time.Minute}
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
		"config-validate  FAIL", "binaries         FAIL", "plugins          FAIL", "paths            FAIL", "addresses        FAIL", "heartbeat        ok",
		"kubectl: not on the image's PATH", "openshell: not on the image's PATH",
		"/opt/missing: named by landing.work_root", "host.docker.internal:28443: does not accept a TCP connect",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the table lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(err.Error(), "5 required check(s)") {
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
}

// A credential in a settings file other than daemon.env does not survive into
// the scratch copy or the output: secret-named JSON values are blanked, a
// ${NAME} reference stays, and a settings file that is neither JSON nor
// daemon.env is not staged at all.
func TestContainerRehearsalDoesNotStageCredentialsFromOtherSettingsFiles(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	var out bytes.Buffer
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	staged := string(h.files[rehearseRoot+"/settings/config.json"])
	for _, secret := range []string{"sk-live-key", "tok-123"} {
		if strings.Contains(staged, secret) || strings.Contains(out.String(), secret) {
			t.Errorf("%q survived into the staged copy or the output:\n%s", secret, staged)
		}
	}
	if !strings.Contains(staged, "/opt/missing") {
		t.Errorf("the redaction removed more than the secret values:\n%s", staged)
	}
	if _, ok := h.files[rehearseRoot+"/settings/secrets.env"]; ok {
		t.Error("a settings file that is neither JSON nor daemon.env was staged")
	}
}

func TestContainerRehearsalRefusesASettingsFileItCannotRedact(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.files[rehearseLive+"/settings/config.json"] = []byte("not json {")
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&bytes.Buffer{}))
	if err == nil || !strings.Contains(err.Error(), "is not JSON") || len(h.writes) != 0 {
		t.Fatalf("err=%v writes=%v", err, h.writes)
	}
}

// A URL in a setting is not a file path: its path part is never looked for in
// the image.
func TestContainerRehearsalDoesNotTakeAURLForAPath(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	var out bytes.Buffer
	if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out)); err != nil {
		t.Fatalf("a settings file whose only odd value is a URL failed the rehearsal: %v\n%s", err, out.String())
	}
	for _, argv := range h.runs {
		if j := strings.Join(argv, " "); strings.Contains(j, "test -e") && strings.Contains(j, "example.test") {
			t.Errorf("the URL was probed as a path: %v", argv)
		}
	}
}

// The scratch town is removed on every exit, and a leftover is removed first.
func TestContainerRehearsalRemovesTheScratchTownOnEveryExit(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*rehearseHost){
		"success":        func(*rehearseHost) {},
		"failed checks":  func(h *rehearseHost) { h.failing = []string{"validate"} },
		"failed staging": func(h *rehearseHost) { h.writeErr = errors.New("boom") },
		"no heartbeat":   func(h *rehearseHost) { h.heartbeat = false },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			set(h)
			_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&bytes.Buffer{}))
			if len(h.removed) != 1 || h.removed[0] != rehearseDir {
				t.Errorf("removed %v, want exactly this run's directory %s", h.removed, rehearseDir)
			}
		})
	}
}

func TestContainerRehearsalReportsARunDirectoryItCannotRemove(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	h := newRehearseHost()
	h.removeErr = []error{boom}
	if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&bytes.Buffer{})); !errors.Is(err, boom) {
		t.Errorf("a final removal that fails must be reported, got %v", err)
	}
}

// Nothing is deleted that this run did not create and mark: a directory whose
// marker is missing or names another run is left alone, and the rehearsal has
// no flag that names a root to delete.
func TestContainerRehearsalDeletesOnlyADirectoryItMarked(t *testing.T) {
	t.Parallel()
	for name, marker := range map[string][]byte{"no marker": nil, "another run's marker": []byte("gt rehearsal run run-other\n")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			d := h.deps(&bytes.Buffer{})
			write := d.write
			d.write = func(p string, data []byte, m os.FileMode) error {
				if strings.HasSuffix(p, rehearseMarker) {
					if marker == nil {
						return nil // the marker never lands
					}
					data = marker
				}
				return write(p, data, m)
			}
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), d)
			if err == nil || !strings.Contains(err.Error(), "does not carry this run's marker") || len(h.removed) != 0 {
				t.Errorf("err=%v removed=%v, want a refusal to delete", err, h.removed)
			}
		})
	}
	if daemonRehearseCmd.Flags().Lookup("root") != nil {
		t.Error("the rehearsal takes a --root: it must delete only a directory it created itself")
	}
}

// The run directory is chosen on resolved paths: one that leads out of the live
// town is refused before anything is written, and so is an unresolvable one.
func TestContainerRehearsalRunDirectoryStaysInsideTheLiveTown(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		links map[string]string
		bad   string
		want  string
	}{
		"runtime links out":    {map[string]string{rehearseLive + "/.runtime/rehearsal/run-t1": "/elsewhere/run-t1"}, "", "outside the live town"},
		"live unresolvable":    {nil, rehearseLive, "cannot be resolved"},
		"run dir unresolvable": {nil, rehearseDir, "cannot be resolved"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			h.links = tc.links
			d := h.deps(&bytes.Buffer{})
			if tc.bad != "" {
				inner := d.evalSymlinks
				d.evalSymlinks = func(p string) (string, error) {
					if p == tc.bad {
						return "", boom
					}
					return inner(p)
				}
			}
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), d)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(h.writes) != 0 || len(h.removed) != 0 || len(h.runs) != 0 {
				t.Fatalf("err=%v writes=%v removed=%v runs=%v", err, h.writes, h.removed, h.runs)
			}
		})
	}
}

func TestContainerRehearsalReportsAContainerThatExits(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.heartbeat = false
	h.failing = []string{"exec gt-run-t1"}
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if err == nil || !strings.Contains(out.String(), "exited before a heartbeat") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
	if h.slept != 0 {
		t.Errorf("waited %s for a container that had already exited", h.slept)
	}
}

func TestContainerRehearsalSeparatesADockerFailureFromAMissingBinary(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.failing = []string{"command -v tmux"}
	h.codes = map[string]int{"command -v tmux": 125}
	var out bytes.Buffer
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if !strings.Contains(out.String(), "tmux: the probe did not run") || strings.Contains(out.String(), "tmux: not on the image's PATH") {
		t.Errorf("a docker failure (exit 125) read as a missing binary:\n%s", out.String())
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
	for _, p := range append(append(append([]string{}, h.writes...), h.mkdirs...), h.removed...) {
		if !pathWithin(p, rehearseDir) {
			t.Errorf("a write targets %s, outside the scratch root", p)
		}
	}
	for _, argv := range h.runs {
		for _, a := range argv {
			if strings.Contains(strings.ReplaceAll(a, rehearseDir, ""), rehearseLive) {
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
		"no image":      {func(f *rehearseFlags) { f.image = "" }, "--image are required"},
		"relative live": {func(f *rehearseFlags) { f.liveRoot = "live" }, "required"},
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

func TestContainerRehearsalFailsLoudlyWhenAPinnedImageIsNotServed(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.failing = []string{"pull " + templates.DindImage}
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if err == nil || !strings.Contains(out.String(), "images           FAIL") || !strings.Contains(out.String(), templates.DindImage+": cannot be pulled by digest") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
}

// A path under a bind of the rendered compose is not looked for in the image; a
// path outside every bind is. The compose file itself is required.
func TestContainerRehearsalChecksPathsAgainstTheComposeBinds(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	var out bytes.Buffer
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	var probed []string
	for _, argv := range h.runs {
		if j := strings.Join(argv, " "); strings.Contains(j, "--entrypoint test") {
			probed = append(probed, argv[len(argv)-1])
		}
	}
	if !slices.Equal(probed, []string{"/opt/missing"}) {
		t.Errorf("probed %v, want only /opt/missing (/h/landing/x and /live/gt/.runtime/y lie under binds)", probed)
	}
	for name, content := range map[string][]byte{"missing": nil, "unparseable": []byte("services: [")} {
		h := newRehearseHost()
		if content == nil {
			delete(h.files, rehearseLive+"/daemon/container/compose.yml")
		} else {
			h.files[rehearseLive+"/daemon/container/compose.yml"] = content
		}
		var out bytes.Buffer
		err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
		if err == nil || !strings.Contains(out.String(), "paths            FAIL") || !strings.Contains(out.String(), "compose") {
			t.Errorf("%s compose: err=%v\n%s", name, err, out.String())
		}
	}
}

// A docker call that blocks ends at its deadline instead of hanging the
// rehearsal, in the heartbeat wait and in a check.
func TestContainerRehearsalBoundsEveryDockerCall(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.heartbeat = false
	f := rehearseFlagsFor()
	f.budget = 50 * time.Millisecond
	d := h.deps(&bytes.Buffer{})
	inner := d.run
	d.run = func(ctx context.Context, dir string, env, argv []string) error {
		if slices.Contains(argv, "exec") { // the liveness probe hangs until its context ends
			<-ctx.Done()
			return ctx.Err()
		}
		return inner(ctx, dir, env, argv)
	}
	var out bytes.Buffer
	d.out = &out
	_ = runDaemonRehearse(context.Background(), f, d)
	if !strings.Contains(out.String(), "no heartbeat in 50ms") {
		t.Errorf("a hung docker call must end the wait at the budget:\n%s", out.String())
	}
}

// An engine that is down or an image that is unknown (docker's own exit codes,
// not the probe's) is reported as a docker failure for every probe kind.
func TestContainerRehearsalReportsDockerFailuresAsDockerFailures(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.failing = []string{"validate", "test -e /opt/missing", "/dev/tcp/host.docker.internal/28443"}
	h.codes = map[string]int{"validate": 125, "test -e": 125, "/dev/tcp": 125}
	var out bytes.Buffer
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	got := out.String()
	for _, want := range []string{"gt config validate: the probe did not run", "/opt/missing: the probe did not run", "host.docker.internal:28443: the probe did not run"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, wrong := range []string{"the settings do not load", "absent from the image", "does not accept a TCP connect"} {
		if strings.Contains(got, wrong) {
			t.Errorf("a docker failure read as %q:\n%s", wrong, got)
		}
	}
}

// One docker call in a check that hangs fails that probe at the call budget and
// leaves the other checks to run.
func TestContainerRehearsalBoundsAHungCheckCall(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	d := h.deps(&bytes.Buffer{})
	d.callBudget = 20 * time.Millisecond
	inner := d.run
	d.run = func(ctx context.Context, dir string, env, argv []string) error {
		if slices.Contains(argv, "command -v tmux") {
			<-ctx.Done()
			return ctx.Err()
		}
		return inner(ctx, dir, env, argv)
	}
	var out bytes.Buffer
	d.out = &out
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), d)
	if err == nil || !strings.Contains(out.String(), "tmux: the probe did not run") || !strings.Contains(out.String(), "heartbeat        ok") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
}

// A failure after the run directory and its marker exist (making a settings
// directory, writing a staged file) is named, and the run directory is still
// removed.
func TestContainerRehearsalNamesAStagedFileItCannotWrite(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, want := range map[string]string{"mkdir": "create " + rehearseRoot, "write": "write " + rehearseRoot} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			d := h.deps(&bytes.Buffer{})
			mk, wr := d.mkdirAll, d.write
			d.mkdirAll = func(p string, m os.FileMode) error {
				if name == "mkdir" && strings.HasPrefix(p, rehearseRoot) {
					return boom
				}
				return mk(p, m)
			}
			d.write = func(p string, data []byte, m os.FileMode) error {
				if name == "write" && strings.HasPrefix(p, rehearseRoot) {
					return boom
				}
				return wr(p, data, m)
			}
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), d)
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), want) || len(h.removed) != 1 {
				t.Fatalf("err=%v removed=%v, want %q wrapping the cause and the run dir removed", err, h.removed, want)
			}
		})
	}
}

// A secret-named key is blanked whatever its value is: a string, a string holding
// a ${NAME} reference, a number, an object, an array, a bool or null. Nothing of
// the value reaches the staged copy, and the type is kept.
func TestContainerRehearsalRedactionFailsClosedForEveryValueType(t *testing.T) {
	t.Parallel()
	in := `{"api_key":"sk-1","token_ref":"${TOKEN_NAME}","secret_n":12345,"secret_obj":{"user":"u-leak","pw":"p-leak"},` +
		`"passwords":["a-leak","b-leak"],"private_flag":true,"credential":null,"plain":"keep-me"}`
	out, values, err := redactSettingsJSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"sk-1", "TOKEN_NAME", "12345", "u-leak", "p-leak", "a-leak", "b-leak"} {
		if strings.Contains(string(out), leak) || slices.ContainsFunc(values, func(v settingValue) bool { return strings.Contains(v.text, leak) }) {
			t.Errorf("%q survived redaction:\n%s", leak, out)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"api_key": "", "token_ref": "", "secret_n": float64(0), "secret_obj": map[string]any{},
		"passwords": []any{}, "private_flag": false, "credential": nil, "plain": "keep-me"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staged = %v, want %v", got, want)
	}
}

// offJSON is the container_off list a passing town carries: every name the
// audit table says cannot run in the container.
func offJSON() string {
	b, _ := json.Marshal(containerOffNames())
	return string(b)
}

// Every host-side or needs-a-seam step must be in daemon.container_off in the
// staged settings; each missing one is named with its reason.
func TestContainerRehearsalPluginsCheckFlagsEveryUnlistedHostSideEntry(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.files[rehearseLive+"/settings/config.json"] = []byte(`{"daemon":{"container_off":["polecat-image"]}}`)
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	got := out.String()
	if err == nil || !strings.Contains(got, "plugins          FAIL") {
		t.Fatalf("err=%v\n%s", err, got)
	}
	for _, s := range daemonSteps {
		listed := s.Name == "polecat-image"
		if s.Class != stepRuns && !listed && !strings.Contains(got, s.Name+" ("+string(s.Class)+") is not in daemon.container_off") {
			t.Errorf("%s (%s) is not reported:\n%s", s.Name, s.Class, got)
		}
		if (s.Class == stepRuns || listed) && strings.Contains(got, s.Name+" (") {
			t.Errorf("%s was reported although it needs no entry", s.Name)
		}
	}
	// A patrol explicitly disabled is off for every supervisor and needs no entry.
	h2 := newRehearseHost()
	h2.files[rehearseLive+"/settings/config.json"] = []byte(`{"daemon":{"container_off":` + offJSONWithout("landing_worker") + `,"patrols":{"landing_worker":{"enabled":false}}}}`)
	var out2 bytes.Buffer
	if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h2.deps(&out2)); err != nil {
		t.Errorf("a disabled patrol still failed: %v\n%s", err, out2.String())
	}
}

func offJSONWithout(name string) string {
	var keep []string
	for _, n := range containerOffNames() {
		if n != name {
			keep = append(keep, n)
		}
	}
	b, _ := json.Marshal(keep)
	return string(b)
}

// A plugin directory or a patrol the audit table does not name fails the check,
// as does a container_off name it does not know and a malformed settings shape.
func TestContainerRehearsalPluginsCheckRefusesAnUnclassifiedPlugin(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.plugins = []string{"mystery", "polecat-image"}
	h.files[rehearseLive+"/settings/config.json"] = settingsWithOff(append(containerOffNames(), "typo-plugin"), `"new_patrol":{"enabled":true}`)
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	got := out.String()
	for _, want := range []string{"mystery: a plugin the audit table does not classify", "new_patrol: a patrol the audit table does not classify", "typo-plugin: in daemon.container_off but unknown"} {
		if err == nil || !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "polecat-image: a plugin") {
		t.Error("a classified plugin was reported")
	}
	h = newRehearseHost()
	h.files[rehearseLive+"/settings/config.json"] = []byte(`{"daemon":{"container_off":"polecat-image"}}`) // not a list
	out.Reset()
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if !strings.Contains(out.String(), "does not give daemon.container_off") {
		t.Errorf("a container_off that is not a list:\n%s", out.String())
	}
}

// Every program a runs-in-container step execs must be on the image's PATH.
func TestContainerRehearsalPluginsCheckNamesAMissingProgram(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.failing = []string{"command -v dolt"}
	var out bytes.Buffer
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if !strings.Contains(out.String(), "dolt: not on the image's PATH, needed by jsonl_git_backup") {
		t.Errorf("the missing program is not tied to its step:\n%s", out.String())
	}
	h = newRehearseHost()
	h.failing = []string{"command -v ps"}
	h.codes = map[string]int{"command -v ps": 125}
	out.Reset()
	_ = runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	if !strings.Contains(out.String(), "ps: the probe did not run") {
		t.Errorf("a docker failure on a step program:\n%s", out.String())
	}
}

// Jev patrols are in-process readers and need no row each.
func TestContainerRehearsalClassifiesJevPatrols(t *testing.T) {
	t.Parallel()
	if !stepNamed("jev_landing", kindPatrol) || slices.Contains(containerOffNames(), "jev_landing") {
		t.Error("jev_landing must be known and run in the container")
	}
	if stepNamed("jevish", kindPatrol) || stepNamed("jev_landing", kindPlugin) {
		t.Error("a name that only resembles the prefix was classified")
	}
}

// checkRowOf returns one check's row of the rehearsal table and the indented
// lines under it, up to the next row.
func checkRowOf(table, name string) []string {
	var row []string
	in := false
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, " ") {
			if in {
				row = append(row, line)
			}
			continue
		}
		in = strings.HasPrefix(line, name+" ")
		if in {
			row = append(row, line)
		}
	}
	return row
}

// The seven rows the container daemon's own log shows running on 2026-11-08 are
// stepRuns. The plugins row test builds its container_off list from
// containerOffNames(), so a row reclassified out of stepRuns slides into that
// list unnoticed; this test names the seven so that reclassification turns red
// (gt-oquf0m).
func TestDaemonRehearseLogShownStepsRunInTheContainer(t *testing.T) {
	t.Parallel()
	logShown := []string{
		"app-promote",
		"devops-reporter",
		"release-reporter",
		"town-metrics",
		"landing_worker",
		"disk_maintenance",
		"doctor_dog",
	}
	rows := make(map[string]daemonStep, len(daemonSteps))
	for _, s := range daemonSteps {
		rows[s.Name] = s
	}
	for _, name := range logShown {
		s, ok := rows[name]
		if !ok {
			t.Errorf("%s has no row in the audit table", name)
			continue
		}
		if s.Class != stepRuns {
			t.Errorf("%s is %s, want %s: the 2026-11-08 container log shows it running", name, s.Class, stepRuns)
		}
	}
}

// With the four names the log never mentions missing from daemon.container_off,
// the plugins row ends with one line naming exactly those four. A settings file
// that holds them passes the row. This pins that the four are not stepRuns: one
// classed running leaves the missing list and the exact summary line turns red.
// The log-shown rows are pinned by TestDaemonRehearseLogShownStepsRunInTheContainer,
// because this test derives its container_off list from containerOffNames() and
// so cannot see one of them reclassified.
func TestDaemonRehearsePluginsFailureListsTheEntriesToAdd(t *testing.T) {
	t.Parallel()
	four := []string{"dolt_server", "health_notify", "landing_infra", "resource_sampler"}
	var keep []string
	for _, n := range containerOffNames() {
		if !slices.Contains(four, n) {
			keep = append(keep, n)
		}
	}
	h := newRehearseHost()
	h.files[rehearseLive+"/settings/config.json"] = settingsWithOff(keep, "")
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	row := checkRowOf(out.String(), "plugins")
	if err == nil || len(row) == 0 || !strings.HasPrefix(row[0], "plugins          FAIL") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
	const summary = "    add these names to daemon.container_off: dolt_server, health_notify, landing_infra, resource_sampler"
	if last := row[len(row)-1]; last != summary {
		t.Errorf("the plugins row ends with %q, want %q:\n%s", last, summary, out.String())
	}
	if n := strings.Count(out.String(), "add these names"); n != 1 {
		t.Errorf("the summary line appears %d times, want once:\n%s", n, out.String())
	}
	for _, name := range four {
		if !strings.Contains(out.String(), name+" (") {
			t.Errorf("%s is not reported as missing:\n%s", name, out.String())
		}
	}

	pass := newRehearseHost()
	var passOut bytes.Buffer
	if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), pass.deps(&passOut)); err != nil {
		t.Fatalf("a settings file holding every name failed: %v\n%s", err, passOut.String())
	}
	if row := checkRowOf(passOut.String(), "plugins"); len(row) == 0 || !strings.HasPrefix(row[0], "plugins          ok") {
		t.Errorf("the plugins row does not pass with the names listed:\n%s", passOut.String())
	}
}

// A path a setting names that the image lacks and no bind of the live compose
// covers fails the paths row with the key that names it and says what clears
// the row. A compose that binds the path passes the row, and the path is not
// probed.
func TestDaemonRehearsePathsNamesTheSettingKey(t *testing.T) {
	t.Parallel()
	const claude = "/Users/sloan/.claude"
	settings := []byte(`{"daemon":{"container_off":` + offJSON() + `,"patrols":{"wisp_reaper":{"extra_beads_roots":["` + claude + `"]}}}}`)

	missing := newRehearseHost()
	missing.files[rehearseLive+"/settings/config.json"] = settings
	missing.failing = []string{"-e " + claude}
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), missing.deps(&out))
	row := checkRowOf(out.String(), "paths")
	if err == nil || len(row) == 0 || !strings.HasPrefix(row[0], "paths            FAIL") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
	for _, want := range []string{
		claude + ": named by daemon.patrols.wisp_reaper.extra_beads_roots[0], absent from the image",
		"the row stays red until a bead lands whose fix makes the image carry it, mounts a bind of the live compose over it, or drops the setting",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the paths row lacks %q:\n%s", want, out.String())
		}
	}

	bound := newRehearseHost()
	bound.files[rehearseLive+"/settings/config.json"] = settings
	bound.files[rehearseLive+"/daemon/container/compose.yml"] = []byte("services:\n  daemon:\n    volumes:\n      - type: bind\n        source: " + claude + "\n        target: " + claude + "\n")
	bound.failing = []string{"-e " + claude}
	var boundOut bytes.Buffer
	if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), bound.deps(&boundOut)); err != nil {
		t.Fatalf("a compose that binds the path failed the rehearsal: %v\n%s", err, boundOut.String())
	}
	if row := checkRowOf(boundOut.String(), "paths"); len(row) == 0 || !strings.HasPrefix(row[0], "paths            ok") {
		t.Errorf("the paths row does not pass under the bind:\n%s", boundOut.String())
	}
}

// The key path a paths failure prints cannot read as a different one: a member
// whose name holds a dot is quoted, so it differs from two nested members, and a
// top-level list's index is rooted instead of left bare.
func TestDaemonRehearsePathsNameUnambiguousSettingKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		settings string
		path     string
		want     string
	}{
		{
			name:     "a member name holding a dot is quoted",
			settings: `{"daemon":{"container_off":` + offJSON() + `},"a.b":"/keys/dotted"}`,
			path:     "/keys/dotted",
			want:     `/keys/dotted: named by ["a.b"], absent from the image`,
		},
		{
			name:     "two nested members keep the dotted form",
			settings: `{"daemon":{"container_off":` + offJSON() + `},"a":{"b":"/keys/nested"}}`,
			path:     "/keys/nested",
			want:     `/keys/nested: named by a.b, absent from the image`,
		},
		{
			name:     "a top-level list index names its root",
			settings: `["/keys/listed"]`,
			path:     "/keys/listed",
			want:     `/keys/listed: named by (top level)[0], absent from the image`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			h.files[rehearseLive+"/settings/config.json"] = []byte(tc.settings)
			h.failing = []string{"-e " + tc.path}
			var out bytes.Buffer
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
			if err == nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("err=%v, the paths row lacks %q:\n%s", err, tc.want, out.String())
			}
		})
	}
}

// A plugin or a patrol the audit table does not name fails the rehearsal until it
// is classified, whatever the rest of the settings hold.
func TestDaemonRehearseUnclassifiedStepStillFails(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	h.plugins = []string{"mystery-plugin"}
	h.files[rehearseLive+"/settings/config.json"] = settingsWithOff(containerOffNames(), `"brand_new_patrol":{"enabled":true}`)
	var out bytes.Buffer
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
	row := checkRowOf(out.String(), "plugins")
	if err == nil || len(row) == 0 || !strings.HasPrefix(row[0], "plugins          FAIL") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"mystery-plugin: a plugin the audit table does not classify",
		"brand_new_patrol: a patrol the audit table does not classify",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plugins row lacks %q:\n%s", want, out.String())
		}
	}
}

// settingsWithOff is a config.json whose daemon section lists off and carries
// the extra patrols entries.
func settingsWithOff(off []string, patrols string) []byte {
	b, _ := json.Marshal(off)
	return []byte(`{"daemon":{"container_off":` + string(b) + `,"patrols":{` + patrols + `}}}`)
}

// A patrol the daemon's config type declares, enabled by its default and absent
// from the settings, is still checked against the table.
func TestContainerRehearsalPluginsCheckKnowsPatrolsTheSettingsOmit(t *testing.T) {
	t.Parallel()
	if got := unclassifiedNames([]string{"landing_worker", "brand_new_default_on_patrol", "jev_x"}); !slices.Equal(got, []string{"brand_new_default_on_patrol"}) {
		t.Errorf("unclassifiedNames = %v", got)
	}
	// Every patrol the real type declares is classified, so a patrol added to
	// config.PatrolsConfig without a table row fails here.
	if got := unclassifiedNames(knownPatrolKeys()); len(got) != 0 {
		t.Errorf("patrols the audit table does not classify: %v", got)
	}
	if len(knownPatrolKeys()) < 20 {
		t.Errorf("knownPatrolKeys found %d keys; the reflection over config.PatrolsConfig is not reading the tags", len(knownPatrolKeys()))
	}
}

func TestContainerRehearsalPluginsCheckFailsForAnUnlistedDefaultOnPatrol(t *testing.T) {
	t.Parallel()
	h := newRehearseHost()
	d := h.deps(&bytes.Buffer{})
	d.patrolKeys = func() []string { return []string{"landing_worker", "brand_new_default_on_patrol"} }
	var out bytes.Buffer
	d.out = &out
	err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), d)
	if err == nil || !strings.Contains(out.String(), "brand_new_default_on_patrol: a patrol the daemon knows") {
		t.Fatalf("err=%v\n%s", err, out.String())
	}
}

// The check that stops an unclassified plugin must not pass when it could not
// look: an unlistable town, plugins directory, or an unreadable plugin.md fails
// it, and a rig without a plugins directory does not.
func TestContainerRehearsalPluginsCheckFailsWhenItCannotLook(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		set  func(*rehearseHost)
		want string
	}{
		"town unlistable":    {func(h *rehearseHost) { h.readErr = map[string]error{rehearseLive: boom} }, "cannot be listed, so its rigs' plugins are unknown"},
		"plugins unlistable": {func(h *rehearseHost) { h.readErr = map[string]error{rehearseLive + "/plugins": boom} }, "plugins directory " + rehearseLive + "/plugins cannot be listed"},
		"plugin.md unreadable": {func(h *rehearseHost) {
			h.plugins = []string{"polecat-image"}
			h.readErr = map[string]error{rehearseLive + "/plugins/polecat-image/plugin.md": boom}
		}, "cannot be read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			tc.set(h)
			var out bytes.Buffer
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
			if err == nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("err=%v, want %q:\n%s", err, tc.want, out.String())
			}
		})
	}
	h := newRehearseHost() // the default fixture has a town with no rig plugins directory
	if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&bytes.Buffer{})); err != nil {
		t.Errorf("a town whose rigs have no plugins directory failed: %v", err)
	}
}

// The entrypoint container has no network, so its bd handshake fails for want of
// Dolt and the daemon exits there: that line is the pass. A state.json heartbeat
// still passes, and any other exit, or no line by the budget, fails with the log
// tail printed.
func TestContainerRehearsalHeartbeatPassesAtTheBdHandshake(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	var tail strings.Builder
	for i := range 60 {
		tail.WriteString("tail-" + strconv.Itoa(i) + "\n")
	}
	for name, tc := range map[string]struct {
		heartbeat bool
		exits     bool // the container exits before the budget; else it runs to it
		log       string
		logErr    error
		pass      bool
		want      []string // each must be in the output
		notWant   []string // none may be in the output
	}{
		"handshake line, container exited":                {exits: true, log: "starting\n" + rehearseHandshakeLine + "\n", pass: true},
		"handshake line, container running at the budget": {log: rehearseHandshakeLine + "\n", pass: true},
		"state.json heartbeat":                            {heartbeat: true, pass: true},
		"another error, container exited": {exits: true, log: "Error: time zone database missing\n",
			want: []string{"exited before a heartbeat", "    Error: time zone database missing"}},
		"empty log, container exited": {exits: true, want: []string{"the log is empty, so it holds no bd handshake line"}},
		"no handshake line at the budget": {log: "Error: time zone database missing\n",
			want: []string{"no heartbeat in 1m0s", "    Error: time zone database missing"}},
		"log cannot be read": {exits: true, logErr: boom, want: []string{"its log could not be read"}},
		"log tail is the last lines": {exits: true, log: tail.String(),
			want: []string{"    tail-20\n", "    tail-59\n"}, notWant: []string{"    tail-19\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			h.heartbeat = tc.heartbeat
			if tc.exits {
				h.failing = []string{"exec gt-run-t1"}
			}
			h.log, h.logErr = tc.log, tc.logErr
			var out bytes.Buffer
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
			got := out.String()
			// The log is docker logs of this run's container, read only when no
			// state.json heartbeat ends the wait first.
			readLog := slices.ContainsFunc(h.captures, func(a []string) bool {
				return slices.Contains(a, "logs") && slices.Contains(a, "gt-run-t1")
			})
			if readLog == tc.heartbeat {
				t.Errorf("docker logs of the entrypoint read = %v, want %v", readLog, !tc.heartbeat)
			}
			if tc.pass {
				if err != nil || !strings.Contains(got, "heartbeat        ok") {
					t.Fatalf("err=%v\n%s", err, got)
				}
				return
			}
			if err == nil || !strings.Contains(got, "heartbeat        FAIL") {
				t.Fatalf("err=%v\n%s", err, got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the output lacks %q:\n%s", want, got)
				}
			}
			for _, not := range tc.notWant {
				if strings.Contains(got, not) {
					t.Errorf("the output holds %q, outside the log tail:\n%s", not, got)
				}
			}
		})
	}
}

// A town.json address the probe cannot dial (a host with shell text in it, a port
// past the range) fails the addresses row without a probe of that address: the
// host is never spliced into the text the image's shell runs.
func TestContainerRehearsalRefusesAnUndialableTownJsonAddress(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		dolt      string
		probeText string // no probe may carry this text
	}{
		"shell text in the host": {`{"container_host":"host.docker.internal;touch pwned","port":3307}`, "touch"},
		"port past the range":    {`{"container_host":"host.docker.internal","port":70000}`, "70000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			h.files[rehearseLive+"/mayor/town.json"] = []byte(`{"dolt":` + tc.dolt + `}`)
			var out bytes.Buffer
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
			if err == nil || !strings.Contains(out.String(), "addresses        FAIL") || !strings.Contains(out.String(), "is not a host:port the probe can dial") {
				t.Fatalf("err=%v\n%s", err, out.String())
			}
			for _, argv := range h.runs {
				if j := strings.Join(argv, " "); strings.Contains(j, "/dev/tcp/") && strings.Contains(j, tc.probeText) {
					t.Errorf("the refused address reached a probe: %v", argv)
				}
			}
		})
	}
}

// The Dolt address the staged town.json names (container_host, else host, with
// the port) is probed from the image over the bridge network, with the other
// host.docker.internal addresses. A town.json that cannot be read or parsed, or
// that names no host or no port, fails the addresses row.
func TestContainerRehearsalProbesTheTownJsonDoltAddress(t *testing.T) {
	t.Parallel()
	const probe = "/dev/tcp/host.docker.internal/3307"
	t.Run("container_host is probed over the bridge", func(t *testing.T) {
		t.Parallel()
		h := newRehearseHost()
		var out bytes.Buffer
		if err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out)); err != nil {
			t.Fatalf("err=%v\n%s", err, out.String())
		}
		var bridged, elsewhere int
		for _, argv := range h.runs {
			if j := strings.Join(argv, " "); strings.Contains(j, probe) {
				if slices.Contains(argv, "bridge") {
					bridged++
				} else {
					elsewhere++
				}
			}
		}
		if bridged != 1 || elsewhere != 0 {
			t.Errorf("the town.json address probed over the bridge %d time(s) and elsewhere %d time(s), want 1 and 0; runs %v", bridged, elsewhere, h.runs)
		}
	})
	t.Run("a refused connect fails the row", func(t *testing.T) {
		t.Parallel()
		h := newRehearseHost()
		h.failing = []string{probe}
		var out bytes.Buffer
		err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
		if err == nil || !strings.Contains(out.String(), "host.docker.internal:3307: does not accept a TCP connect") {
			t.Fatalf("err=%v\n%s", err, out.String())
		}
	})
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		set  func(*rehearseHost)
		want string
	}{
		"unreadable": {func(h *rehearseHost) { h.readErr = map[string]error{rehearseRoot + "/mayor/town.json": boom} }, "cannot be read"},
		"unparseable": {func(h *rehearseHost) {
			h.files[rehearseLive+"/mayor/town.json"] = []byte("{")
		}, "does not parse"},
		"no port": {func(h *rehearseHost) {
			h.files[rehearseLive+"/mayor/town.json"] = []byte(`{"dolt":{"host":"host.docker.internal"}}`)
		}, "names no dolt port"},
		"no host": {func(h *rehearseHost) {
			h.files[rehearseLive+"/mayor/town.json"] = []byte(`{"dolt":{"port":3307}}`)
		}, "names no dolt host"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			tc.set(h)
			var out bytes.Buffer
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
			if err == nil || !strings.Contains(out.String(), "addresses        FAIL") || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("err=%v, want %q:\n%s", err, tc.want, out.String())
			}
		})
	}
}

// The three accepted paths print as accepted with their reasons and are not
// probed, so they do not fail the paths row. Any other absent path still fails:
// the landing path the compose rewrite binds is not accepted.
func TestContainerRehearsalListsAcceptedPaths(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	imessage := filepath.Join(home, ".claude", "tools", "imessage-push.sh")
	ntfy := filepath.Join(home, ".config", "gt", "ntfy-push.sh")
	accepted := `{"gotmp":"/Users/Shared/gt-gotmp.noindex","imessage":"` + imessage + `","ntfy":"` + ntfy + `"}`
	t.Run("the three print as accepted and are not probed", func(t *testing.T) {
		t.Parallel()
		h := newRehearseHost()
		h.home = home
		h.files[rehearseLive+"/settings/config.json"] = []byte(`{"paths":` + accepted + `,"daemon":{"container_off":` + offJSON() + `}}`)
		// A probe of any of the three would fail, so a probed accepted path fails the row.
		h.failing = []string{"-e " + home, "-e /Users/Shared/gt-gotmp.noindex"}
		var out bytes.Buffer
		err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
		got := out.String()
		if err != nil {
			t.Fatalf("accepted paths failed the rehearsal: %v\n%s", err, got)
		}
		for _, want := range []string{
			"paths            ok",
			"    accepted: /Users/Shared/gt-gotmp.noindex (GOTMPDIR is host-only and dropped from the container env)",
			"    accepted: " + imessage + " (escalation notify, deferred by the operator)",
			"    accepted: " + ntfy + " (escalation notify, deferred by the operator)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the output lacks %q:\n%s", want, got)
			}
		}
		for _, run := range h.runs {
			if n := len(run); n >= 2 && run[n-2] == "-e" && slices.Contains([]string{"/Users/Shared/gt-gotmp.noindex", imessage, ntfy}, run[n-1]) {
				t.Errorf("an accepted path was probed: %v", run)
			}
		}
	})
	t.Run("the landing path is not accepted", func(t *testing.T) {
		t.Parallel()
		h := newRehearseHost()
		h.files[rehearseLive+"/settings/config.json"] = []byte(`{"landing":"/Users/user/gt-landing.noindex","daemon":{"container_off":` + offJSON() + `}}`)
		h.failing = []string{"test -e /Users/user/gt-landing.noindex"}
		var out bytes.Buffer
		err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
		if err == nil || !strings.Contains(out.String(), "paths            FAIL") || !strings.Contains(out.String(), "/Users/user/gt-landing.noindex: named by landing,") {
			t.Fatalf("err=%v\n%s", err, out.String())
		}
	})
}

// TestDaemonRehearseAuditPathsFollowHome: the two accepted notify paths are
// built from the account's home directory, never a literal. A run whose home is
// a temp directory accepts the notify scripts there; a settings value under
// another account's home is not accepted; and a home the run cannot determine
// fails the row rather than silently dropping the accept list.
func TestDaemonRehearseAuditPathsFollowHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	imessage := filepath.Join(home, ".claude", "tools", "imessage-push.sh")
	ntfy := filepath.Join(home, ".config", "gt", "ntfy-push.sh")
	t.Run("the notify paths follow the home the run is given", func(t *testing.T) {
		t.Parallel()
		h := newRehearseHost()
		h.home = home
		h.files[rehearseLive+"/settings/config.json"] = []byte(`{"paths":{"imessage":"` + imessage + `","ntfy":"` + ntfy + `"},"daemon":{"container_off":` + offJSON() + `}}`)
		// A probe of either accepted path would fail, so a probed one fails the row.
		h.failing = []string{"-e " + home}
		var out bytes.Buffer
		err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
		got := out.String()
		if err != nil {
			t.Fatalf("the notify paths under the run's home failed the rehearsal: %v\n%s", err, got)
		}
		for _, want := range []string{
			"paths            ok",
			"    accepted: " + imessage + " (escalation notify, deferred by the operator)",
			"    accepted: " + ntfy + " (escalation notify, deferred by the operator)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the output lacks %q:\n%s", want, got)
			}
		}
	})
	t.Run("another account's notify path is not accepted", func(t *testing.T) {
		t.Parallel()
		const other = "/home/other/.claude/tools/imessage-push.sh"
		h := newRehearseHost()
		h.home = home
		h.files[rehearseLive+"/settings/config.json"] = []byte(`{"paths":{"imessage":"` + other + `"},"daemon":{"container_off":` + offJSON() + `}}`)
		h.failing = []string{"test -e " + other}
		var out bytes.Buffer
		err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
		if err == nil || !strings.Contains(out.String(), "paths            FAIL") || !strings.Contains(out.String(), other+": named by paths.imessage,") {
			t.Fatalf("err=%v\n%s", err, out.String())
		}
	})
	for _, tc := range []struct {
		name    string
		home    string
		homeErr error
		want    string
	}{
		{"the home cannot be determined", "", errors.New("HOME is unset"), "the home directory cannot be determined"},
		{"the home is not absolute", "relative/home", nil, "the home directory is not an absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRehearseHost()
			h.home, h.homeErr = tc.home, tc.homeErr
			var out bytes.Buffer
			err := runDaemonRehearse(context.Background(), rehearseFlagsFor(), h.deps(&out))
			if err == nil || !strings.Contains(out.String(), "paths            FAIL") || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("err=%v, want %q:\n%s", err, tc.want, out.String())
			}
		})
	}
}
