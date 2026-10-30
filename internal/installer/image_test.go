package installer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/imagebuild"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

// imageRun is a run whose checkout holds the polecat-image program and the
// build output, and whose image builds are recorded instead of run. Its
// settings come from the case's map, never the host's environment, and its
// files are answered rather than written, so the case starts no process and
// writes no executable file (gt-tpg0p.12).
type imageRun struct {
	*testRun
	builds []ImageBuild
	code   int
	err    error
	// files is the paths the fake checkout holds, and whether each can be
	// run.
	files map[string]bool
}

func newImageRun(t *testing.T) *imageRun {
	t.Helper()
	ir := &imageRun{testRun: newTestRun(t), files: map[string]bool{}}
	ir.GetEnv(nil)
	ir.addFile(ir.program(), false)
	ir.addFile(ir.buildOutput(), true)
	ir.Run.Files.IsFile = func(path string) bool {
		_, ok := ir.files[path]
		return ok
	}
	ir.Run.Files.IsExecutable = func(path string) bool {
		return ir.files[path]
	}
	ir.ImageRunner = func(b ImageBuild) (int, error) {
		ir.builds = append(ir.builds, b)
		return ir.code, ir.err
	}
	// No sling runs and the clock does not wait, so no case starts pgrep or
	// sleeps unless it says so (gt-rfwr4).
	ir.Run.SlingDeferral = func() string { return "" }
	ir.Run.ImageSleep = func(time.Duration) {}
	return ir
}

// addFile puts path in the fake checkout, executable when run is set.
func (ir *imageRun) addFile(path string, run bool) { ir.files[path] = run }

// GetEnv sets the INSTALL_GT_IMAGE_* answers the run gets.
func (ir *imageRun) GetEnv(values map[string]string) {
	ir.Run.Getenv = func(key string) string { return values[key] }
}

// program is the polecat-image program's default path in the checkout.
func (ir *imageRun) program() string {
	return filepath.Join(ir.Paths.RigDir, imageProgramRel)
}

// buildOutput is the build output's default path in the checkout.
func (ir *imageRun) buildOutput() string {
	return filepath.Join(ir.Paths.RigDir, "gt")
}

// town gives the run a town root with no settings file.
func (ir *imageRun) town(t *testing.T) string {
	t.Helper()
	ir.Paths.TownRoot = t.TempDir()
	return ir.Paths.TownRoot
}

// config writes settings/config.json in the town root.
func (ir *imageRun) config(t *testing.T, body string) {
	t.Helper()
	town := ir.Paths.TownRoot
	if town == "" {
		town = ir.town(t)
	}
	dir := filepath.Join(town, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// step runs the image step against a resolved target of c2222222 replacing
// c1111111.
func (ir *imageRun) step(t *testing.T) Exit {
	t.Helper()
	ir.resolved("c1111111")
	return ir.ImageStep()
}

// output is everything the run wrote to stdout.
func (ir *imageRun) output() string { return ir.out.String() }

// has reports whether the run's output names text.
func (ir *imageRun) has(text string) bool { return strings.Contains(ir.output(), text) }

// escalation is the last escalation's fingerprint, or "".
func (ir *imageRun) fingerprint() string {
	escalation := ir.escalation()
	if len(escalation) == 0 {
		return ""
	}
	return escalation[len(escalation)-1]
}

// The image is built at the target commit before the swap, on the build output
// rather than the installed binary, and the step reports no result of its own:
// the install carries on (gt-tpg0p.12).
func TestImageStepBuildsBeforeTheSwap(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)

	if code := ir.step(t); code != ExitOK {
		t.Fatalf("ImageStep() = %d, want %d", code, ExitOK)
	}
	want := "[install-gt] polecat image: build is local only and not published: no town root\n" +
		"[install-gt] polecat image: building c2222222 before the binary swap (log /dev/null)\n" +
		"[install-gt] polecat image: in force at c2222222\n"
	if ir.output() != want {
		t.Errorf("output = %q, want %q", ir.output(), want)
	}
	if len(ir.builds) != 1 {
		t.Fatalf("builds = %+v, want one", ir.builds)
	}
	build := ir.builds[0]
	if build.Program != ir.program() {
		t.Errorf("program = %q, want %q", build.Program, ir.program())
	}
	if build.Alarm != 1200*time.Second {
		t.Errorf("alarm = %v, want 1200s", build.Alarm)
	}
	if build.Detached {
		t.Error("the build before the swap must be waited for")
	}
	if build.GT != "" || build.Fingerprint != "" {
		t.Errorf("escalation = %q/%q, want none before the swap", build.GT, build.Fingerprint)
	}
	// A run with no town root names no town root and no lock: an empty setting
	// would overwrite whatever the build inherits.
	wantEnv := []string{
		"POLECAT_IMAGE_GT=" + ir.buildOutput(),
		imageFailClosedEnv + "=1",
	}
	if strings.Join(build.Env, "\n") != strings.Join(wantEnv, "\n") {
		t.Errorf("env = %q, want %q", build.Env, wantEnv)
	}
	if ir.has("RESULT") {
		t.Errorf("output names a result line: %q", ir.output())
	}
}

// A build that does not finish fails the install closed: the run ends
// failed/build-failed with the plugin's own reason repeated from the log, and
// the old binary is untouched because nothing was swapped (gt-xzo8a).
func TestImageStepFailsTheInstallWhenTheBuildFails(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	logPath := filepath.Join(t.TempDir(), "image.log")
	if err := os.WriteFile(logPath, []byte("building\nthe image\nERROR: docker is down\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ir.GetEnv(map[string]string{"INSTALL_GT_IMAGE_LOG": logPath})
	ir.code = 1

	if code := ir.step(t); code != ExitFailed {
		t.Fatalf("ImageStep() = %d, want %d", code, ExitFailed)
	}
	for _, want := range []string{
		"polecat image: the build for c2222222 did not finish (exit 1); the binary was not swapped",
		"polecat image: ERROR: docker is down",
		"FAILED: the sandbox image for c2222222 was not built and published; " + ir.Paths.GT() +
			" was left at c1111111 and the daemon was not told to restart",
		"install-gt: RESULT failed c2222222 c1111111 build-failed",
	} {
		if !ir.has(want) {
			t.Errorf("output lacks %q:\n%s", want, ir.output())
		}
	}
	if got := ir.fingerprint(); got != "install-gt:build-failed" {
		t.Errorf("escalation fingerprint = %q, want install-gt:build-failed", got)
	}
}

// Without a SlingDeferral seam the sling is asked of pgrep for SlingPattern, and
// the exit status decides: no match builds, a match defers to the bound, and a
// pgrep that cannot answer defers too, so it fails closed (gt-jie3d).
func TestImageSlingProbeUsesPgrep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		pgrepExit int
		wantExit  Exit
		wantBuilt bool
	}{
		{name: "no sling matches: built", pgrepExit: 1, wantExit: ExitOK, wantBuilt: true},
		{name: "a sling matches: deferred past the bound", pgrepExit: 0, wantExit: ExitFailed},
		{name: "pgrep cannot answer: deferred past the bound", pgrepExit: 2, wantExit: ExitFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			ir.Run.SlingDeferral = nil
			ir.respond = func(string, string, []string) ([]byte, []byte, int) {
				return nil, nil, tc.pgrepExit
			}

			if code := ir.step(t); code != tc.wantExit {
				t.Fatalf("ImageStep() = %d, want %d:\n%s", code, tc.wantExit, ir.output())
			}
			if len(ir.cmds) == 0 {
				t.Fatal("no pgrep was run, so the sling was not asked")
			}
			want := []string{"pgrep", "-f", polecatimage.SlingPattern}
			for i, got := range ir.cmds {
				if !reflect.DeepEqual(got, want) {
					t.Errorf("command %d = %q, want %q", i, got, want)
				}
			}
			if built := len(ir.builds) == 1; built != tc.wantBuilt {
				t.Errorf("builds = %d, want built = %v", len(ir.builds), tc.wantBuilt)
			}
		})
	}
}

// The sling wait comes out of the build's alarm: a build before the swap runs
// within INSTALL_GT_IMAGE_ALARM as a whole, so the wait cannot run the install
// past the window the alarm names. A wait that leaves no time fails the step,
// since an alarm of zero would leave the build with no bound (gt-jie3d).
func TestImageSlingWaitIsWithinAlarm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		alarm         string
		deferredReads int
		wantExit      Exit
		wantAlarm     time.Duration
		wantSleeps    int
		// wantLog is the line a failed step logs, when it fails.
		wantLog string
	}{
		{name: "no wait keeps the whole alarm", alarm: "", deferredReads: 0, wantExit: ExitOK,
			wantAlarm: 1200 * time.Second, wantSleeps: 0},
		{name: "a wait comes out of the alarm", alarm: "900", deferredReads: 2, wantExit: ExitOK,
			wantAlarm: 870 * time.Second, wantSleeps: 2},
		{name: "a wait that clears inside the alarm leaves the rest", alarm: "60", deferredReads: 2, wantExit: ExitOK,
			wantAlarm: 30 * time.Second, wantSleeps: 2},
		{name: "a wait still deferred at the alarm fails", alarm: "60", deferredReads: -1, wantExit: ExitFailed,
			wantSleeps: 4, wantLog: "sandbox sling in progress after 1m0s; the build is not run"},
		{name: "a wait that clears exactly at the alarm fails", alarm: "60", deferredReads: 4, wantExit: ExitFailed,
			wantSleeps: 4, wantLog: "the sling wait used the whole alarm of 1m0s; the build is not run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			ir.GetEnv(map[string]string{"INSTALL_GT_IMAGE_ALARM": tc.alarm})
			reads := 0
			ir.Run.SlingDeferral = func() string {
				reads++
				if tc.deferredReads < 0 || reads <= tc.deferredReads {
					return "sandbox sling in progress"
				}
				return ""
			}
			var sleeps []time.Duration
			ir.Run.ImageSleep = func(d time.Duration) { sleeps = append(sleeps, d) }

			if code := ir.step(t); code != tc.wantExit {
				t.Fatalf("ImageStep() = %d, want %d:\n%s", code, tc.wantExit, ir.output())
			}
			if len(sleeps) != tc.wantSleeps {
				t.Fatalf("sleeps = %v, want %d polls", sleeps, tc.wantSleeps)
			}
			if tc.wantExit == ExitFailed {
				if len(ir.builds) != 0 {
					t.Errorf("builds = %d, want none once the wait used the alarm", len(ir.builds))
				}
				if !ir.has(tc.wantLog) {
					t.Errorf("output lacks %q:\n%s", tc.wantLog, ir.output())
				}
				return
			}
			if len(ir.builds) != 1 {
				t.Fatalf("builds = %d, want 1", len(ir.builds))
			}
			if got := ir.builds[0].Alarm; got != tc.wantAlarm {
				t.Errorf("build alarm = %v, want %v", got, tc.wantAlarm)
			}
		})
	}
}

// A build before the swap waits out a gt sling in flight instead of failing on
// the first deferral. The sling reads are the clock's only input: a build that
// is not deferred never polls, one deferred twice builds after the second poll,
// and one still deferred past imageSlingBound fails with the image's own error
// and runs no build (gt-rfwr4).
func TestImageWaitsForSling(t *testing.T) {
	t.Parallel()
	const deferred = "sandbox sling in progress"
	cases := []struct {
		name string
		// deferredReads is how many sling reads answer deferred before one
		// answers clear; -1 never clears.
		deferredReads int
		wantExit      Exit
		wantBuilds    int
		wantSleeps    int
	}{
		{name: "built first time: no poll", deferredReads: 0, wantExit: ExitOK, wantBuilds: 1, wantSleeps: 0},
		{name: "deferred twice then built", deferredReads: 2, wantExit: ExitOK, wantBuilds: 1, wantSleeps: 2},
		{name: "deferred past the bound fails", deferredReads: -1, wantExit: ExitFailed, wantBuilds: 0,
			wantSleeps: int(imageSlingBound / imageSlingPoll)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			reads := 0
			ir.Run.SlingDeferral = func() string {
				reads++
				if tc.deferredReads < 0 || reads <= tc.deferredReads {
					return deferred
				}
				return ""
			}
			var sleeps []time.Duration
			ir.Run.ImageSleep = func(d time.Duration) { sleeps = append(sleeps, d) }

			if code := ir.step(t); code != tc.wantExit {
				t.Fatalf("ImageStep() = %d, want %d:\n%s", code, tc.wantExit, ir.output())
			}
			if len(ir.builds) != tc.wantBuilds {
				t.Errorf("builds = %d, want %d", len(ir.builds), tc.wantBuilds)
			}
			if len(sleeps) != tc.wantSleeps {
				t.Fatalf("sleeps = %v, want %d polls of %v", sleeps, tc.wantSleeps, imageSlingPoll)
			}
			for _, d := range sleeps {
				if d != imageSlingPoll {
					t.Errorf("slept %v, want %v", d, imageSlingPoll)
				}
			}
			if tc.wantExit == ExitFailed {
				if !ir.has("FAILED: the sandbox image for c2222222 was not built and published") {
					t.Errorf("output lacks the image's fail-closed error:\n%s", ir.output())
				}
				if !ir.has("sandbox sling in progress after 3m0s; the build is not run") {
					t.Errorf("output does not say the wait ran out:\n%s", ir.output())
				}
				if got := ir.fingerprint(); got != "install-gt:build-failed" {
					t.Errorf("escalation fingerprint = %q, want install-gt:build-failed", got)
				}
			}
		})
	}
}

// The log's tail is five lines, as the script's tail -n 5 prints, so a noisy
// build does not bury the install's own lines.
func TestImageStepTailsAtMostFiveLogLines(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	logPath := filepath.Join(t.TempDir(), "image.log")
	body := "1\n2\n3\n4\n5\n6\n7\n"
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ir.GetEnv(map[string]string{"INSTALL_GT_IMAGE_LOG": logPath})
	ir.code = 1

	if code := ir.step(t); code != ExitFailed {
		t.Fatalf("ImageStep() = %d, want %d", code, ExitFailed)
	}
	for _, want := range []string{"3", "4", "5", "6", "7"} {
		if !ir.has("[install-gt] polecat image: " + want + "\n") {
			t.Errorf("output lacks log line %q:\n%s", want, ir.output())
		}
	}
	for _, gone := range []string{"1", "2"} {
		if ir.has("[install-gt] polecat image: " + gone + "\n") {
			t.Errorf("output repeats log line %q beyond the last five:\n%s", gone, ir.output())
		}
	}
}

// A build the step cannot start is a build that did not finish: no plugin on
// disk, or no build output to name the commit with, both fail the install
// before the swap and run nothing.
func TestImageStepRefusesWithoutTheProgramOrBuildOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(*imageRun)
		wantLog string
	}{
		{
			name: "no plugin",
			setup: func(ir *imageRun) {
				delete(ir.files, ir.program())
			},
			wantLog: "polecat image: no plugin at '" +
				filepath.Join("RIG", "plugins", "polecat-image", "bin", "polecat-image") +
				"', so the image cannot be built before the swap; the binary was not swapped",
		},
		{
			name: "no build output",
			setup: func(ir *imageRun) {
				ir.addFile(ir.buildOutput(), false)
			},
			wantLog: "polecat image: no build output at RIG/gt, so the image was not built",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			tc.setup(ir)
			want := strings.ReplaceAll(tc.wantLog, "RIG", ir.Paths.RigDir)
			if code := ir.step(t); code != ExitFailed {
				t.Fatalf("ImageStep() = %d, want %d", code, ExitFailed)
			}
			if !ir.has(want) {
				t.Errorf("output lacks %q:\n%s", want, ir.output())
			}
			if len(ir.builds) != 0 {
				t.Errorf("builds = %+v, want none", ir.builds)
			}
			if got := ir.fingerprint(); got != "install-gt:build-failed" {
				t.Errorf("escalation fingerprint = %q, want install-gt:build-failed", got)
			}
		})
	}
}

// An alarm that is not a whole number of seconds above zero, or that is too
// large for a duration, would leave the build with no bound: the default is
// used and the value named, and a usable one is used as given.
func TestImageStepAlarm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		value     string
		wantAlarm time.Duration
		wantNamed bool
	}{
		{name: "unset takes the default", value: "", wantAlarm: 1200 * time.Second},
		{name: "a whole number is used", value: "900", wantAlarm: 900 * time.Second},
		{name: "the largest duration is used", value: "9223372036", wantAlarm: 9223372036 * time.Second},
		{name: "zero is named", value: "0", wantAlarm: 1200 * time.Second, wantNamed: true},
		{name: "a word is named", value: "soon", wantAlarm: 1200 * time.Second, wantNamed: true},
		{name: "a leading zero is named", value: "01200", wantAlarm: 1200 * time.Second, wantNamed: true},
		{name: "a value a duration cannot hold is named", value: "9223372037", wantAlarm: 1200 * time.Second, wantNamed: true},
		{name: "a value past int is named", value: "9223372036854775808", wantAlarm: 1200 * time.Second, wantNamed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			if tc.value != "" {
				ir.GetEnv(map[string]string{"INSTALL_GT_IMAGE_ALARM": tc.value})
			}
			if code := ir.step(t); code != ExitOK {
				t.Fatalf("ImageStep() = %d, want %d", code, ExitOK)
			}
			if len(ir.builds) != 1 || ir.builds[0].Alarm != tc.wantAlarm {
				t.Fatalf("builds = %+v, want an alarm of %v", ir.builds, tc.wantAlarm)
			}
			named := ir.has("INSTALL_GT_IMAGE_ALARM='" + tc.value + "' is not a usable number of seconds")
			if named != tc.wantNamed {
				t.Errorf("named the value = %v, want %v:\n%s", named, tc.wantNamed, ir.output())
			}
		})
	}
}

// The registry and crane come from the town's settings/config.json daemon.env,
// and every way that read can fail runs the build local-only with its own
// reason. A value the config gives is exported to the build; one
// it leaves out is absent from the environment the build inherits, so an
// ambient value still answers for it.
func TestImageStepDaemonEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		noFile  bool
		readErr error
		wantWhy string
		wantEnv []string
		notEnv  []string
		// wantFailed says the config could not be read, so the fail-closed
		// step before the swap ends the install rather than build local-only
		// (gt-tpg0p.12).
		wantFailed bool
	}{
		{
			name:    "config.json gives both",
			body:    `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":"localhost:5001","POLECAT_IMAGE_CRANE":"/crane/bin/crane"}}}`,
			wantEnv: []string{"POLECAT_IMAGE_REGISTRY=localhost:5001", "POLECAT_IMAGE_CRANE=/crane/bin/crane"},
		},
		{
			name:    "a missing config.json",
			noFile:  true,
			wantWhy: "TOWN/settings/config.json is missing",
			notEnv:  []string{"POLECAT_IMAGE_REGISTRY="},
		},
		{
			name:       "a config.json that cannot be read",
			body:       `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":"localhost:5001"}}}`,
			readErr:    errors.New("permission denied"),
			wantWhy:    "TOWN/settings/config.json could not be read",
			notEnv:     []string{"POLECAT_IMAGE_REGISTRY="},
			wantFailed: true,
		},
		{
			name:       "a malformed config.json",
			body:       `{"daemon":`,
			wantWhy:    "TOWN/settings/config.json is not readable JSON",
			notEnv:     []string{"POLECAT_IMAGE_REGISTRY="},
			wantFailed: true,
		},
		{
			name:       "a daemon that is not an object",
			body:       `{"daemon":[]}`,
			wantWhy:    "TOWN/settings/config.json is not readable JSON",
			notEnv:     []string{"POLECAT_IMAGE_REGISTRY="},
			wantFailed: true,
		},
		{
			name:    "no daemon.env object at all",
			body:    `{"daemon":{}}`,
			wantWhy: "TOWN/settings/config.json has no daemon.env",
			notEnv:  []string{"POLECAT_IMAGE_REGISTRY="},
		},
		{
			name:       "a daemon.env that is not an object",
			body:       `{"daemon":{"env":"oops"}}`,
			wantWhy:    "daemon.env in TOWN/settings/config.json is a string, not an object",
			notEnv:     []string{"POLECAT_IMAGE_REGISTRY="},
			wantFailed: true,
		},
		{
			name:    "a key the config lacks",
			body:    `{"daemon":{"env":{"POLECAT_IMAGE_CRANE":"/crane/bin/crane"}}}`,
			wantWhy: "no POLECAT_IMAGE_REGISTRY in daemon.env of TOWN/settings/config.json",
			wantEnv: []string{"POLECAT_IMAGE_CRANE=/crane/bin/crane"},
			notEnv:  []string{"POLECAT_IMAGE_REGISTRY="},
		},
		{
			name:       "a value that is not a string",
			body:       `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":7001}}}`,
			wantWhy:    "POLECAT_IMAGE_REGISTRY in daemon.env of TOWN/settings/config.json is not a string",
			notEnv:     []string{"POLECAT_IMAGE_REGISTRY="},
			wantFailed: true,
		},
		{
			name:    "an empty value",
			body:    `{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":""}}}`,
			wantWhy: "no POLECAT_IMAGE_REGISTRY in daemon.env of TOWN/settings/config.json",
			notEnv:  []string{"POLECAT_IMAGE_REGISTRY="},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			town := ir.town(t)
			if !tc.noFile {
				ir.config(t, tc.body)
			}
			if tc.readErr != nil {
				cfg := filepath.Join(town, "settings", "config.json")
				ir.Run.Files.ReadFile = func(path string) ([]byte, error) {
					if path == cfg {
						return nil, tc.readErr
					}
					return os.ReadFile(path)
				}
			}
			why := strings.ReplaceAll(tc.wantWhy, "TOWN", town)
			if tc.wantFailed {
				// A config that cannot be read leaves the registry unknown, so
				// the fail-closed step ends the install instead of building an
				// image no one publishes (gt-tpg0p.12).
				if code := ir.step(t); code != ExitFailed {
					t.Fatalf("ImageStep() = %d, want %d:\n%s", code, ExitFailed, ir.output())
				}
				if len(ir.builds) != 0 {
					t.Errorf("builds = %+v, want none", ir.builds)
				}
				if !ir.has("the town's image registry could not be read (" + why + ")") {
					t.Errorf("output lacks the unreadable reason %q:\n%s", why, ir.output())
				}
				if !ir.has("the sandbox image for " + ir.Run.C.Expected + " cannot be published") {
					t.Errorf("output does not say the image cannot be published:\n%s", ir.output())
				}
				if got := ir.fingerprint(); got != "install-gt:build-failed" {
					t.Errorf("escalation fingerprint = %q, want install-gt:build-failed", got)
				}
				return
			}
			if code := ir.step(t); code != ExitOK {
				t.Fatalf("ImageStep() = %d, want %d:\n%s", code, ExitOK, ir.output())
			}
			if len(ir.builds) != 1 {
				t.Fatalf("builds = %+v, want one", ir.builds)
			}
			env := ir.builds[0].Env
			for _, want := range tc.wantEnv {
				if !containsLine(env, want) {
					t.Errorf("env = %q, want %q", env, want)
				}
			}
			for _, not := range tc.notEnv {
				for _, have := range env {
					if strings.HasPrefix(have, not) {
						t.Errorf("env = %q, want no %s", env, not)
					}
				}
			}
			if tc.wantWhy != "" {
				if !ir.has("polecat image: build is local only and not published: " + why) {
					t.Errorf("output lacks the reason %q:\n%s", why, ir.output())
				}
			} else if ir.has("build is local only and not published") {
				t.Errorf("output says local only with a registry configured:\n%s", ir.output())
			}
		})
	}
}

// A registry the daemon already exported reaches the build through the
// environment it inherits, so the step does not call that build local-only
// (gt-ijudv).
func TestImageStepAmbientRegistryIsNotLocalOnly(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	ir.town(t)
	ir.GetEnv(map[string]string{"POLECAT_IMAGE_REGISTRY": "localhost:7001"})

	if code := ir.step(t); code != ExitOK {
		t.Fatalf("ImageStep() = %d, want %d", code, ExitOK)
	}
	if ir.has("build is local only and not published") {
		t.Errorf("output calls the build local-only with an ambient registry:\n%s", ir.output())
	}
	if len(ir.builds) != 1 {
		t.Fatalf("builds = %+v, want one", ir.builds)
	}
	for _, have := range ir.builds[0].Env {
		if strings.HasPrefix(have, "POLECAT_IMAGE_REGISTRY=") {
			t.Errorf("env = %q, want the inherited registry left alone", ir.builds[0].Env)
		}
	}
}

// The town root's .runtime directory is where the log and the plugin's lock go
// by default, and the log's directory is created before the build writes it.
func TestImageStepTownDefaults(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	town := ir.town(t)

	if code := ir.step(t); code != ExitOK {
		t.Fatalf("ImageStep() = %d, want %d", code, ExitOK)
	}
	if len(ir.builds) != 1 {
		t.Fatalf("builds = %+v, want one", ir.builds)
	}
	build := ir.builds[0]
	if want := filepath.Join(town, ".runtime", "polecat-image.lock"); build.Lock != want {
		t.Errorf("lock = %q, want %q", build.Lock, want)
	}
	if want := filepath.Join(town, ".runtime", "polecat-image-install.log"); build.Log != want {
		t.Errorf("log = %q, want %q", build.Log, want)
	}
	if info, err := os.Stat(filepath.Join(town, ".runtime")); err != nil || !info.IsDir() {
		t.Errorf("the log directory was not created: %v", err)
	}
	for _, want := range []string{
		"POLECAT_IMAGE_TOWN_ROOT=" + town,
		"POLECAT_IMAGE_LOCK=" + filepath.Join(town, ".runtime", "polecat-image.lock"),
	} {
		if !containsLine(build.Env, want) {
			t.Errorf("env = %q, want %q", build.Env, want)
		}
	}
}

// Every INSTALL_GT_IMAGE_* the step reads overrides its default, so an operator
// (and a test) can point the build at its own program, output, log and lock.
func TestImageStepSettingsOverrides(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	root := t.TempDir()
	program := filepath.Join(root, "polecat-image")
	output := filepath.Join(root, "gt-build")
	ir.addFile(program, false)
	ir.addFile(output, true)
	ir.GetEnv(map[string]string{
		"INSTALL_GT_IMAGE_RUN":    program,
		"INSTALL_GT_IMAGE_BINARY": output,
		"INSTALL_GT_IMAGE_LOG":    filepath.Join(root, "logs", "build.log"),
		"INSTALL_GT_IMAGE_LOCK":   filepath.Join(root, "lock"),
	})

	if code := ir.step(t); code != ExitOK {
		t.Fatalf("ImageStep() = %d, want %d", code, ExitOK)
	}
	build := ir.builds[0]
	if build.Program != program || build.Log != filepath.Join(root, "logs", "build.log") || build.Lock != filepath.Join(root, "lock") {
		t.Errorf("build = %+v, want the named program, log and lock", build)
	}
	if !containsLine(build.Env, "POLECAT_IMAGE_GT="+output) {
		t.Errorf("env = %q, want the named build output", build.Env)
	}
	if info, err := os.Stat(filepath.Join(root, "logs")); err != nil || !info.IsDir() {
		t.Errorf("the log directory was not created: %v", err)
	}
}

// --no-image-first starts the build after the swap, detached and best effort:
// it escalates under its own fingerprint, is not told to fail closed, and never
// changes what the run reports (gt-vhn89, gt-efupr).
func TestImageAfterStartsADetachedBuild(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)

	ir.resolved("c1111111")
	ir.ImageAfter()

	if len(ir.builds) != 1 {
		t.Fatalf("builds = %+v, want one", ir.builds)
	}
	build := ir.builds[0]
	if !build.Detached {
		t.Error("the build after the swap must not be waited for")
	}
	if build.GT != ir.Paths.GT() || build.Fingerprint != imageBuildFailed {
		t.Errorf("escalation = %q/%q, want the detached build's own", build.GT, build.Fingerprint)
	}
	for _, have := range build.Env {
		if strings.HasPrefix(have, imageFailClosedEnv+"=") {
			t.Errorf("env = %q, want no %s: the install already stands", build.Env, imageFailClosedEnv)
		}
	}
	if want := "polecat image: rebuild for c2222222 started in the background (log /dev/null)"; !ir.has(want) {
		t.Errorf("output lacks %q:\n%s", want, ir.output())
	}
	if ir.has("RESULT") {
		t.Errorf("output names a result line: %q", ir.output())
	}
	if len(ir.execs) != 0 {
		t.Errorf("escalations = %q, want none", ir.execs)
	}
}

// A detached build that cannot even be started escalates, and the install
// stands: the binary is already swapped and the failure is the image's.
func TestImageAfterSpawnFailureEscalates(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	ir.err = errors.New("no such session")

	ir.resolved("c1111111")
	ir.ImageAfter()

	if !ir.has("polecat image: the rebuild for c2222222 could not be started: no such session") {
		t.Errorf("output lacks the start failure:\n%s", ir.output())
	}
	if got := ir.fingerprint(); got != imageBuildFailed {
		t.Errorf("escalation fingerprint = %q, want %s", got, imageBuildFailed)
	}
}

// A detached rebuild its preflight refuses never fails the install, but it does
// escalate under the build's own fingerprint: nothing else reports a rebuild that
// never ran, and the image stays stale until someone notices (gt-tpg0p.12).
func TestImageAfterWithoutAProgramOrBuildOutputEscalates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(*imageRun)
		wantLog string
		wantWhy string
	}{
		{
			name: "no plugin",
			setup: func(ir *imageRun) {
				delete(ir.files, ir.program())
			},
			wantLog: "polecat image: no plugin at '" +
				filepath.Join("RIG", "plugins", "polecat-image", "bin", "polecat-image") +
				"', so the image cannot be built after the swap; the binary was already swapped",
			wantWhy: "no plugin at 'RIG/plugins/polecat-image/bin/polecat-image'",
		},
		{
			name: "no build output",
			setup: func(ir *imageRun) {
				ir.addFile(ir.buildOutput(), false)
			},
			wantLog: "polecat image: no build output at RIG/gt, so the image was not built",
			wantWhy: "no build output at RIG/gt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			tc.setup(ir)

			ir.resolved("c1111111")
			ir.ImageAfter()

			if want := strings.ReplaceAll(tc.wantLog, "RIG", ir.Paths.RigDir); !ir.has(want) {
				t.Errorf("output lacks %q:\n%s", want, ir.output())
			}
			if len(ir.builds) != 0 {
				t.Errorf("builds = %+v, want none", ir.builds)
			}
			if got := ir.fingerprint(); got != imageBuildFailed {
				t.Errorf("escalation fingerprint = %q, want %s", got, imageBuildFailed)
			}
			if want := strings.ReplaceAll(tc.wantWhy, "RIG", ir.Paths.RigDir); !strings.Contains(strings.Join(ir.escalation(), " "), want) {
				t.Errorf("escalation does not name the reason %q:\n%q", want, ir.escalation())
			}
			if ir.has("RESULT") {
				t.Errorf("output names a result line: %q", ir.output())
			}
		})
	}
}

// A rebuild after the swap cannot fail the install, so an unreadable registry
// is a log line and the build still runs, local-only: the binary is already in
// place and the registry keeps the old image until a later install reads the
// settings (gt-tpg0p.12).
func TestImageAfterWithAnUnreadableRegistryLogsAndCarriesOn(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	town := ir.town(t)
	ir.config(t, `{"daemon":`)

	ir.resolved("c1111111")
	ir.ImageAfter()

	if len(ir.builds) != 1 || !ir.builds[0].Detached {
		t.Fatalf("builds = %+v, want one detached rebuild", ir.builds)
	}
	want := "polecat image: the town's image registry could not be read (" +
		filepath.Join(town, "settings", "config.json") + " is not readable JSON), " +
		"so the rebuild is local only and the registry keeps the old image"
	if !ir.has(want) {
		t.Errorf("output lacks %q:\n%s", want, ir.output())
	}
	if len(ir.escalation()) != 0 {
		t.Errorf("escalation = %q, want none for an install that already stands", ir.escalation())
	}
	if containsLine(ir.builds[0].Env, imageFailClosedEnv+"=1") {
		t.Errorf("env = %q, want a detached build not told to fail closed", ir.builds[0].Env)
	}
}

// Finalize is where the detached rebuild is asked for: after the marker and the
// receipt, before the run states its result, and only when the install did not
// build the image first.
func TestFinalizeRebuildsTheImageAfterTheSwapWhenAsked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		imageFirst bool
		wantBuilds int
	}{
		{name: "image first never rebuilds after the swap", imageFirst: true, wantBuilds: 0},
		{name: "no-image-first rebuilds after the swap", imageFirst: false, wantBuilds: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ir := newImageRun(t)
			ir.Cfg.ImageFirst = tc.imageFirst
			ir.resolved("c1111111")

			if code := ir.Finalize(); code != ExitOK {
				t.Fatalf("Finalize() = %d, want %d:\n%s", code, ExitOK, ir.output())
			}
			if len(ir.builds) != tc.wantBuilds {
				t.Fatalf("builds = %+v, want %d", ir.builds, tc.wantBuilds)
			}
			if tc.wantBuilds == 1 && !ir.builds[0].Detached {
				t.Error("the rebuild after the swap must be detached")
			}
			if want := "install-gt: RESULT installed c2222222 c1111111 -"; !containsLine(ir.lines(), want) {
				t.Errorf("output lacks %q:\n%s", want, ir.output())
			}
		})
	}
}

// A detached rebuild that never runs escalates without changing the install's
// own result: the binary is already in place, so the run still reports
// installed, and the escalation is what says the image is stale (gt-tpg0p.12).
func TestFinalizeStatesInstalledWhenTheRebuildNeverRuns(t *testing.T) {
	t.Parallel()
	ir := newImageRun(t)
	ir.Cfg.ImageFirst = false
	delete(ir.files, ir.program())
	ir.resolved("c1111111")

	if code := ir.Finalize(); code != ExitOK {
		t.Fatalf("Finalize() = %d, want %d:\n%s", code, ExitOK, ir.output())
	}
	if want := "install-gt: RESULT installed c2222222 c1111111 -"; !containsLine(ir.lines(), want) {
		t.Errorf("output lacks %q:\n%s", want, ir.output())
	}
	if got := ir.fingerprint(); got != imageBuildFailed {
		t.Errorf("escalation fingerprint = %q, want %s", got, imageBuildFailed)
	}
}

// A build log that cannot be opened is a build that did not run, and the step
// reports it rather than starting the program without its log.
func TestRunImageBuildCannotOpenTheLog(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	code, err := tr.Run.runImageBuild(ImageBuild{Log: filepath.Join(file, "log")})

	if err == nil || code != 1 {
		t.Errorf("runImageBuild() = %d, %v; want 1 and the open error", code, err)
	}
}

// A build before the swap runs in this process under the supervisor, with the
// log as its streams and the settings the step gave it; the supervisor is the
// seam, so the case starts no build (gt-tpg0p.12).
func TestRunImageBuildRunsUnderTheSupervisor(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	log := filepath.Join(t.TempDir(), "build.log")
	var got imagebuild.Config
	tr.Run.ImageSupervisor = func(cfg imagebuild.Config) int {
		got = cfg
		fmt.Fprint(cfg.Stdout, "building\n")
		fmt.Fprint(cfg.Stderr, "warning\n")
		return 7
	}

	code, err := tr.Run.runImageBuild(ImageBuild{
		Program: "/rig/polecat-image",
		Env:     []string{"POLECAT_IMAGE_GT=/rig/gt"},
		Alarm:   90 * time.Second,
		Lock:    "/town/lock",
		Log:     log,
	})

	if err != nil || code != 7 {
		t.Fatalf("runImageBuild() = %d, %v; want the supervisor's 7", code, err)
	}
	if got.Alarm != 90*time.Second || got.Lock != "/town/lock" ||
		!reflect.DeepEqual(got.Command, []string{"/rig/polecat-image"}) {
		t.Errorf("config = %+v, want the build's program, alarm and lock", got)
	}
	if got.GT != "" || got.Fingerprint != "" {
		t.Errorf("escalation = %q/%q, want none for a build whose caller is still there", got.GT, got.Fingerprint)
	}
	if !containsLine(got.Env, "POLECAT_IMAGE_GT=/rig/gt") {
		t.Errorf("env = %q, want the build settings", got.Env)
	}
	body, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatalf("read the build log: %v", readErr)
	}
	if want := "building\nwarning\n"; string(body) != want {
		t.Errorf("log = %q, want the build's streams %q", body, want)
	}
}

// startImageWorker reports each way a detached build cannot be started, and
// starts the worker with the arguments, settings and session it needs.
func TestStartImageWorker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		log     func(t *testing.T) string
		exe     func() (string, error)
		start   func(*exec.Cmd) error
		wantErr bool
	}{
		{
			name:    "the program cannot name itself",
			log:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "log") },
			exe:     func() (string, error) { return "", errors.New("no executable") },
			start:   func(*exec.Cmd) error { return nil },
			wantErr: true,
		},
		{
			name: "the log cannot be opened",
			log: func(t *testing.T) string {
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(file, "log")
			},
			exe:     func() (string, error) { return "/gt-installer", nil },
			start:   func(*exec.Cmd) error { return nil },
			wantErr: true,
		},
		{
			name:    "the worker cannot be started",
			log:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "log") },
			exe:     func() (string, error) { return "/gt-installer", nil },
			start:   func(*exec.Cmd) error { return errors.New("no session") },
			wantErr: true,
		},
		{
			name:  "a started worker is released",
			log:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "log") },
			exe:   func() (string, error) { return "/gt-installer", nil },
			start: func(*exec.Cmd) error { return nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var started *exec.Cmd
			released := false
			err := startImageWorker(ImageBuild{
				Program:     "/rig/polecat-image",
				Env:         []string{"POLECAT_IMAGE_TOWN_ROOT=/town"},
				Alarm:       20 * time.Minute,
				Lock:        "/town/lock",
				GT:          "/bin/gt",
				Fingerprint: imageBuildFailed,
				Log:         tc.log(t),
				Detached:    true,
			}, workerExec{
				executable: tc.exe,
				start: func(cmd *exec.Cmd) error {
					started = cmd
					return tc.start(cmd)
				},
				release: func(*os.Process) error { released = true; return nil },
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("startImageWorker() = nil, want an error")
				}
				if released {
					t.Error("a worker that never started was released")
				}
				return
			}
			if err != nil {
				t.Fatalf("startImageWorker(): %v", err)
			}
			if started == nil || !released {
				t.Fatalf("started = %v, released = %v, want both", started != nil, released)
			}
			wantArgs := []string{"/gt-installer", ImageWorkerFlag, "--alarm", "1200", "--lock", "/town/lock",
				"--gt", "/bin/gt", "--fingerprint", imageBuildFailed, "--", "/rig/polecat-image"}
			if !reflect.DeepEqual(started.Args, wantArgs) {
				t.Errorf("args = %q, want %q", started.Args, wantArgs)
			}
			if started.SysProcAttr == nil || !started.SysProcAttr.Setsid {
				t.Error("the worker was not started in a session of its own")
			}
			if !containsLine(started.Env, "POLECAT_IMAGE_TOWN_ROOT=/town") {
				t.Errorf("env = %q, want the build settings", started.Env)
			}
		})
	}
}

// The argv a worker is started with carries only what it was given: no --lock
// or escalation flags for a build without them.
func TestWorkerArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		build ImageBuild
		want  []string
	}{
		{
			name:  "every setting",
			build: ImageBuild{Program: "/p", Alarm: 90 * time.Second, Lock: "/l", GT: "/gt", Fingerprint: imageBuildFailed},
			want:  []string{ImageWorkerFlag, "--alarm", "90", "--lock", "/l", "--gt", "/gt", "--fingerprint", imageBuildFailed, "--", "/p"},
		},
		{
			name:  "no lock and no escalation",
			build: ImageBuild{Program: "/p", Alarm: 1200 * time.Second},
			want:  []string{ImageWorkerFlag, "--alarm", "1200", "--", "/p"},
		},
		{
			name:  "a fingerprint without a gt is not passed",
			build: ImageBuild{Program: "/p", Alarm: 60 * time.Second, Lock: "/l", Fingerprint: imageBuildFailed},
			want:  []string{ImageWorkerFlag, "--alarm", "60", "--lock", "/l", "--", "/p"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := workerArgs(tc.build); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("workerArgs() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A detached build's worker parses the arguments the installer gives it, and
// refuses an invocation that names no build.
func TestParseImageWorker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		args      []string
		wantAlarm time.Duration
		wantLock  string
		wantGT    string
		wantPrint string
		wantCmd   []string
		wantErr   bool
	}{
		{
			name:      "a full invocation",
			args:      []string{"--alarm", "1200", "--lock", "/town/lock", "--gt", "/bin/gt", "--fingerprint", "install-gt:image-build-failed", "--", "/rig/polecat-image"},
			wantAlarm: 1200 * time.Second,
			wantLock:  "/town/lock",
			wantGT:    "/bin/gt",
			wantPrint: "install-gt:image-build-failed",
			wantCmd:   []string{"/rig/polecat-image"},
		},
		{
			name:      "no lock and no escalation",
			args:      []string{"--alarm", "60", "--", "/rig/polecat-image"},
			wantAlarm: 60 * time.Second,
			wantCmd:   []string{"/rig/polecat-image"},
		},
		{name: "an alarm of zero", args: []string{"--alarm", "0", "--", "/rig/polecat-image"}, wantErr: true},
		{name: "an unknown flag", args: []string{"--bogus", "--", "/rig/polecat-image"}, wantErr: true},
		{name: "no program", args: []string{"--alarm", "60", "--"}, wantErr: true},
		{name: "a fingerprint without a gt", args: []string{"--alarm", "60", "--fingerprint", "fp", "--", "p"}, wantErr: true},
		{name: "a gt without a fingerprint", args: []string{"--alarm", "60", "--gt", "/bin/gt", "--", "p"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr strings.Builder
			cfg, err := parseImageWorker(tc.args, &stderr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseImageWorker(%q) = %+v, want an error", tc.args, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseImageWorker(%q): %v", tc.args, err)
			}
			if cfg.Alarm != tc.wantAlarm || cfg.Lock != tc.wantLock || cfg.GT != tc.wantGT || cfg.Fingerprint != tc.wantPrint {
				t.Errorf("config = %+v, want alarm %v, lock %q, gt %q, fingerprint %q",
					cfg, tc.wantAlarm, tc.wantLock, tc.wantGT, tc.wantPrint)
			}
			if !reflect.DeepEqual(cfg.Command, tc.wantCmd) {
				t.Errorf("command = %q, want %q", cfg.Command, tc.wantCmd)
			}
		})
	}
}

// The worker detaches before it runs the build, and returns the build's status.
func TestImageWorkerDetachesThenRuns(t *testing.T) {
	t.Parallel()
	var order []string
	var got []string
	code := imageWorker(
		[]string{"--alarm", "60", "--", "/rig/polecat-image"},
		&strings.Builder{},
		func() { order = append(order, "detach") },
		func(cfg imagebuild.Config) int {
			order = append(order, "run")
			got = cfg.Command
			return 7
		},
	)

	if code != 7 {
		t.Errorf("imageWorker() = %d, want the build's 7", code)
	}
	if strings.Join(order, ",") != "detach,run" {
		t.Errorf("order = %q, want detach before run", order)
	}
	if len(got) != 1 || got[0] != "/rig/polecat-image" {
		t.Errorf("command = %q, want the program", got)
	}
}

// A worker invocation that names no build is refused with its usage, before
// anything is detached or run.
func TestImageWorkerRefusesABadInvocation(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	ran := false
	code := imageWorker(
		[]string{"--alarm", "0", "--", "/rig/polecat-image"},
		&stderr,
		func() { ran = true },
		func(imagebuild.Config) int { ran = true; return 0 },
	)

	if code != 2 {
		t.Errorf("imageWorker() = %d, want 2", code)
	}
	if ran {
		t.Error("a refused invocation detached or ran a build")
	}
	if !strings.Contains(stderr.String(), ImageWorkerFlag) {
		t.Errorf("stderr = %q, want the usage", stderr.String())
	}
}
