package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
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
}

func newRehearseHost() *rehearseHost {
	return &rehearseHost{
		clock: time.Unix(1_000_000, 0),
		files: map[string][]byte{
			rehearseLive + "/settings/config.json":         []byte(`{"landing":{"work_root":"/opt/missing","clones":"/h/landing/x","town":"/live/gt/.runtime/y"},"gate":"host.docker.internal:28443","forge":"https://example.test/api/v1","api_key":"sk-live-key","ref":"${KEEP_ME}","auth":{"token":"tok-123"},"daemon":{"container_off":` + offJSON() + `}}`),
			rehearseLive + "/settings/secrets.env":         []byte("RAW=raw-secret-value\n"),
			rehearseLive + "/settings/daemon.env":          []byte("# comment\nDEEPSEEK_KEY=sk-secret-value\nexport PATH=/usr/bin:/bin\n"),
			rehearseLive + "/mayor/town.json":              []byte(`{"name":"gt"}`),
			rehearseLive + "/daemon/container/compose.yml": []byte("services:\n  daemon:\n    volumes:\n      - type: bind\n        source: /live/gt\n        target: /live/gt\n      - type: bind\n        source: /h/landing\n        target: /h/landing\n      - type: volume\n        source: v\n        target: /vol\n"),
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
		newID: func() string { return "t1" },
		now:   func() time.Time { return h.clock },
		sleep: func(d time.Duration) { h.clock = h.clock.Add(d); h.slept += d },
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
		"/opt/missing: named by a setting", "host.docker.internal:28443: does not accept a TCP connect",
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
		if strings.Contains(string(out), leak) || slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, leak) }) {
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
		listed := s.name == "polecat-image"
		if s.class != stepRuns && !listed && !strings.Contains(got, s.name+" ("+string(s.class)+") is not in daemon.container_off") {
			t.Errorf("%s (%s) is not reported:\n%s", s.name, s.class, got)
		}
		if (s.class == stepRuns || listed) && strings.Contains(got, s.name+" (") {
			t.Errorf("%s was reported although it needs no entry", s.name)
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
