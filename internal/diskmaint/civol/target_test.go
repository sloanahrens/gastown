package civol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// Placeholder container and image names, so a test never names a real one.
const (
	testDind   = "test-dind"
	testRunner = "test-runner"
	testImage  = "test-image"
)

// answer is one command's canned result.
type answer struct {
	out string
	err error
}

func (a answer) bytes() ([]byte, error) { return []byte(a.out), a.err }

// fakeDocker answers the docker commands the target runs from canned output and
// records each one, so a test asserts what was asked and no test reaches a real
// daemon.
type fakeDocker struct {
	dind, runner string
	calls        []string
	running      answer            // docker inspect -f {{.State.Running}}
	ps           answer            // docker exec <dind> docker ps -q
	starts       map[string]answer // job id -> StartedAt
	volumeList   answer            // docker exec <dind> docker volume ls
	sizes        map[string]answer // volume name -> du -sk
	listings     map[string]answer // volume name -> file or module listing
	moduleSizes  answer            // module directory sizes from du
	df           answer            // docker exec <runner> df -P /
	removes      map[string]answer // volume name -> throwaway container's rm
	modes        map[string]answer // volume name -> throwaway container's chmod
	prunes       map[string]answer // prune name (builder, image) -> docker prune -f
}

func (f *fakeDocker) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	if len(env) > 0 {
		return nil, fmt.Errorf("unexpected environment %q", env)
	}
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "docker" {
		return nil, fmt.Errorf("unexpected program %q", name)
	}
	switch {
	case len(args) >= 2 && args[0] == "inspect":
		return f.running.bytes()
	case len(args) < 3 || args[0] != "exec":
		return nil, fmt.Errorf("unexpected docker command %q", strings.Join(args, " "))
	}

	container, rest := args[1], args[2:]
	if container == f.runner && rest[0] == "df" {
		return f.df.bytes()
	}
	if container != f.dind || len(rest) < 2 || rest[0] != "docker" {
		return nil, fmt.Errorf("unexpected docker command %q", strings.Join(args, " "))
	}

	sub := rest[1:]
	switch sub[0] {
	case "ps":
		return f.ps.bytes()
	case "inspect":
		return f.starts[sub[len(sub)-1]].bytes()
	case "volume":
		if len(sub) != 4 || sub[1] != "ls" {
			return nil, fmt.Errorf("unexpected docker command %q", strings.Join(args, " "))
		}
		return f.volumeList.bytes()
	case "builder", "image":
		if len(sub) != 3 || sub[1] != "prune" || sub[2] != "-f" {
			return nil, fmt.Errorf("unexpected docker command %q", strings.Join(args, " "))
		}
		return f.prunes[sub[0]].bytes()
	case "run":
		return f.runAnswer(sub)
	}
	return nil, fmt.Errorf("unexpected docker command %q", strings.Join(args, " "))
}

// runAnswer answers one throwaway container by its entrypoint, or by the script
// it runs when the entrypoint is sh, which is the command's last argument.
func (f *fakeDocker) runAnswer(sub []string) ([]byte, error) {
	volume, entrypoint := "", ""
	for i, arg := range sub {
		switch {
		case (arg == "-v" || arg == "--volume") && i+1 < len(sub):
			volume, _, _ = strings.Cut(sub[i+1], ":")
		case arg == "--entrypoint" && i+1 < len(sub):
			entrypoint = sub[i+1]
		}
	}
	switch entrypoint {
	case "rm":
		return f.removes[volume].bytes()
	case "chmod":
		return f.modes[volume].bytes()
	}
	switch script := sub[len(sub)-1]; script {
	case sizeScript:
		return f.sizes[volume].bytes()
	case moduleSizesScript:
		return f.moduleSizes.bytes()
	case fileListingScript, moduleListingScript:
		return f.listings[volume].bytes()
	default:
		return nil, fmt.Errorf("unexpected script %q", script)
	}
}

// writeVerbs are the docker subcommands that could change state, so a plan that
// issues one would be trimming rather than planning.
var writeVerbs = []string{"rm", "rmi", "prune", "stop", "kill", "create", "build", "push", "pull", "tag", "load", "import", "commit", "start", "restart"}

// writes returns the calls that could change state, which a plan never makes.
func (f *fakeDocker) writes() []string {
	var out []string
	for _, c := range f.calls {
		if stateChanging(c) {
			out = append(out, c)
		}
	}
	return out
}

// stateChanging reports a docker command line that could change state: a verb
// that removes, stops or creates, or a volume mounted writable.
func stateChanging(call string) bool {
	fields := strings.Fields(call)
	for i, field := range fields {
		if slices.Contains(writeVerbs, field) {
			return true
		}
		var mount string
		switch {
		case field == "-v" || field == "--volume" || field == "--mount":
			if i+1 < len(fields) {
				mount = fields[i+1]
			}
		case strings.HasPrefix(field, "--mount="):
			mount = strings.TrimPrefix(field, "--mount=")
		}
		if mount != "" && !readOnlyMount(mount) {
			return true
		}
	}
	return false
}

// readOnlyMount reports a mount argument the container cannot write through: a
// -v/--volume value whose option list says ro, or a --mount value shown ro or
// readonly.
func readOnlyMount(arg string) bool {
	if strings.Contains(arg, "=") {
		for _, opt := range strings.Split(arg, ",") {
			if opt == "ro" || opt == "readonly" {
				return true
			}
		}
		return false
	}
	parts := strings.Split(arg, ":")
	return len(parts) > 2 && slices.Contains(strings.Split(parts[2], ","), "ro")
}

// newFake answers the reads of a CI runner with every volume present, empty and
// well under its cap, and no job running.
func newFake() *fakeDocker {
	return &fakeDocker{
		dind:       testDind,
		runner:     testRunner,
		running:    answer{out: "true\n"},
		volumeList: answer{out: gocacheVolume + "\n" + gomodVolume + "\n" + npmVolume + "\n"},
		sizes:      map[string]answer{gocacheVolume: {out: "1000\n"}, gomodVolume: {out: "1000\n"}, npmVolume: {out: "1000\n"}},
		starts:     map[string]answer{},
		listings:   map[string]answer{},
		removes:    map[string]answer{},
		modes:      map[string]answer{},
		prunes:     map[string]answer{},
		df: answer{out: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"overlay 102400 40960 61440 40% /\n"},
	}
}

// cancelRunner answers the fake's canned output, but cancels ctx around the
// first call whose command line contains want: before it, so that call fails as
// canceled, or after it, so the caller's next check sees the cancellation.
// Everything the target asks for after that is refused the way a canceled run
// is.
type cancelRunner struct {
	f      *fakeDocker
	cancel context.CancelFunc
	want   string
	after  bool
	fired  bool
}

// run is the Runner the target calls. fired records that a call matched want,
// so a test whose command changed fails instead of passing on nothing.
func (c *cancelRunner) run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	match := strings.Contains(name+" "+strings.Join(args, " "), c.want)
	if match && !c.after {
		c.fired = true
		c.cancel()
		return nil, context.Canceled
	}
	out, err := c.f.run(ctx, env, name, args...)
	if match && c.after {
		c.fired = true
		c.cancel()
	}
	return out, err
}

// target plans against the fake's canned docker.
func target(f *fakeDocker) *Target {
	return NewTarget(Config{Dind: f.dind, Runner: f.runner, Image: testImage, Run: f.run})
}

// plan runs the target's plan and fails the test if it errors.
func (f *fakeDocker) plan(t *testing.T) diskmaint.Plan {
	t.Helper()
	p, err := target(f).Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

// fileLine is one line of find's file listing.
func fileLine(path string, mtime time.Time, bytes int64) string {
	return fmt.Sprintf("%d.0\t%d\t%s/%s\n", mtime.Unix(), bytes, volumeRoot, path)
}

// moduleLine is one line of find's module listing.
func moduleLine(path string, mtime time.Time) string {
	return fmt.Sprintf("%d.0\t./%s\n", mtime.Unix(), path)
}

// moduleSizeLine is one line of du's module size listing, in KB.
func moduleSizeLine(path string, kb int64) string {
	return fmt.Sprintf("%d\t./%s\n", kb, path)
}

func TestName(t *testing.T) {
	t.Parallel()
	if got := target(newFake()).Name(); got != "civol" {
		t.Fatalf("Name() = %q, want civol", got)
	}
}

func TestNewTargetFillsDefaults(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.dind = defaultDind
	f.runner = defaultRunner
	tgt := NewTarget(Config{Run: f.run})

	if got := tgt.Name(); got != planTarget {
		t.Fatalf("Name() = %q, want %q", got, planTarget)
	}
	if _, err := tgt.Plan(context.Background(), diskmaint.Policy{}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(f.calls) == 0 || !strings.Contains(f.calls[0], defaultDind) {
		t.Fatalf("first call %q does not name the default dind container %q", f.calls, defaultDind)
	}
	image := false
	for _, c := range f.calls {
		if strings.Contains(c, defaultImage) {
			image = true
			break
		}
	}
	if !image {
		t.Fatalf("no call used the default image %q: %v", defaultImage, f.calls)
	}
}

// TestPlanTrimsOverCapGocache drives an over-cap build cache whose listing the
// fake returns, and asserts the plan removes its oldest entries first.
func TestPlanTrimsOverCapGocache(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.sizes[gocacheVolume] = answer{out: strconv.FormatInt(10*kbPerGB, 10) + "\n"}
	f.listings[gocacheVolume] = answer{out: fileLine("oldest.bin", testNow.Add(-5*day), 3*gb) +
		fileLine("older.bin", testNow.Add(-3*day), 3*gb) + fileLine("newest.bin", testNow.Add(-time.Hour), 2*gb)}

	p := f.plan(t)
	if p.Refused != "" {
		t.Fatalf("plan refused: %s", p.Refused)
	}
	want := []string{"ci-gocache:oldest.bin", "ci-gocache:older.bin"}
	if got := removed(p); !slices.Equal(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}
	for _, a := range p.Actions {
		if a.Kind != deleteKind {
			t.Errorf("action %q kind = %q, want %q", a.Ref, a.Kind, deleteKind)
		}
		if a.Bytes != 3*gb {
			t.Errorf("action %q bytes = %d, want %d", a.Ref, a.Bytes, 3*gb)
		}
	}
	if rule, ok := keptRule(p, "ci-gocache:newest.bin"); !ok || rule != ruleTargetMet {
		t.Fatalf("newest.bin kept rule = %q (kept %v), want %q", rule, ok, ruleTargetMet)
	}
	if w := f.writes(); len(w) > 0 {
		t.Fatalf("plan issued write commands: %v", w)
	}
}

func TestPlanDindNotRunning(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.running = answer{out: "false\n"}

	p := f.plan(t)
	if p.Refused != "" {
		t.Fatalf("plan refused: %s", p.Refused)
	}
	if len(p.Actions) > 0 {
		t.Fatalf("actions = %v, want none", removed(p))
	}
	if rule, ok := keptRule(p, testDind); !ok || rule != ruleDindDown {
		t.Fatalf("kept rule for %s = %q (kept %v), want %q", testDind, rule, ok, ruleDindDown)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %v, want only the running-state inspect", f.calls)
	}
}

// TestPlanJobGate drives the gated trim: the guard is read from the oldest
// running job's start, and an unreadable start makes the volume wait.
func TestPlanJobGate(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	overCap := func(f *fakeDocker) {
		f.sizes[gocacheVolume] = answer{out: strconv.FormatInt(10*kbPerGB, 10) + "\n"}
		f.listings[gocacheVolume] = answer{out: fileLine("before.bin", start.Add(-3*time.Hour), gb) +
			fileLine("inside.bin", start.Add(-30*time.Minute), gb)}
	}

	t.Run("earlier of two starts", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		overCap(f)
		f.ps = answer{out: "aaa\nbbb\n"}
		f.starts["aaa"] = answer{out: "2026-10-29T10:00:00.123456789Z\n"}
		f.starts["bbb"] = answer{out: "2026-10-29T09:00:00.987654321Z\n"}

		p := f.plan(t)
		if p.Refused != "" {
			t.Fatalf("plan refused: %s", p.Refused)
		}
		// A guard read from the later start would put inside.bin before the
		// cutoff and trim it, so keeping it pins the earlier start.
		if got := removed(p); !slices.Equal(got, []string{"ci-gocache:before.bin"}) {
			t.Fatalf("removed = %v, want only the entry before the guard", got)
		}
		if rule, ok := keptRule(p, "ci-gocache:inside.bin"); !ok || rule != ruleJobNewer {
			t.Fatalf("inside.bin kept rule = %q (kept %v), want %q", rule, ok, ruleJobNewer)
		}
	})

	// A short and a full-length value that is not a timestamp: both leave the
	// start unknown, and the plan then waits rather than trim around it.
	cases := []struct {
		name   string
		starts map[string]string
	}{
		{"short start", map[string]string{"aaa": "yesterday\n"}},
		{"start that does not parse", map[string]string{"aaa": "2026-99-99T99:99:99Z\n"}},
		{"one of two unknown", map[string]string{"aaa": "2026-10-29T09:00:00Z\n", "bbb": "2026-99-99T99:99:99Z\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			overCap(f)
			ids := make([]string, 0, len(c.starts))
			for id, start := range c.starts {
				ids = append(ids, id)
				f.starts[id] = answer{out: start}
			}
			f.ps = answer{out: strings.Join(ids, "\n") + "\n"}

			p := f.plan(t)
			if p.Refused != "" {
				t.Fatalf("plan refused: %s", p.Refused)
			}
			if len(p.Actions) > 0 {
				t.Fatalf("actions = %v, want none while the oldest start is unknown", removed(p))
			}
			if rule, ok := keptRule(p, gocacheVolume); !ok || rule != ruleJobUnknown {
				t.Fatalf("volume kept rule = %q (kept %v), want %q", rule, ok, ruleJobUnknown)
			}
		})
	}
}

// TestPlanModuleEntries reads the module cache by whole "<module>@<version>"
// directories and sizes each from du.
func TestPlanModuleEntries(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.sizes[gomodVolume] = answer{out: strconv.FormatInt(6*kbPerGB, 10) + "\n"}
	f.listings[gomodVolume] = answer{out: moduleLine("github.com/first@v1.0.0", testNow.Add(-5*day)) +
		moduleLine("github.com/second@v2.0.0", testNow.Add(-3*day)) +
		moduleLine("github.com/newest@v3.0.0", testNow.Add(-time.Hour))}
	f.moduleSizes = answer{out: moduleSizeLine("github.com/first@v1.0.0", 2*kbPerGB) +
		moduleSizeLine("github.com/second@v2.0.0", 2*kbPerGB) +
		moduleSizeLine("github.com/newest@v3.0.0", 1*kbPerGB)}

	p := f.plan(t)
	if p.Refused != "" {
		t.Fatalf("plan refused: %s", p.Refused)
	}
	want := []string{
		"ci-gomod:pkg/mod/github.com/first@v1.0.0",
		"ci-gomod:pkg/mod/github.com/second@v2.0.0",
	}
	if got := removed(p); !slices.Equal(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}
	if got := p.Actions[0].Bytes; got != 2*gb {
		t.Fatalf("first action bytes = %d, want %d", got, 2*gb)
	}
}

func TestPlanMissingVolume(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.volumeList = answer{out: gocacheVolume + "\n" + gomodVolume + "\n"}

	p := f.plan(t)
	if p.Refused != "" {
		t.Fatalf("a missing volume refused the plan: %s", p.Refused)
	}
	if rule, ok := keptRule(p, npmVolume); !ok || rule != ruleMissing {
		t.Fatalf("kept rule for %s = %q (kept %v), want %q", npmVolume, rule, ok, ruleMissing)
	}
}

// TestPlanRefusesFailedRead drives every read that can fail, one at a time, and
// asserts each one refuses the plan without naming an action.
func TestPlanRefusesFailedRead(t *testing.T) {
	t.Parallel()
	boom := errors.New("daemon unreachable")
	cases := []struct {
		name      string
		breakRead func(f *fakeDocker)
	}{
		{"dind state", func(f *fakeDocker) { f.running = answer{err: boom} }},
		{"dind state unexpected", func(f *fakeDocker) { f.running = answer{out: "maybe\n"} }},
		{"job list", func(f *fakeDocker) { f.ps = answer{err: boom} }},
		{"job start", func(f *fakeDocker) {
			f.ps = answer{out: "aaa\n"}
			f.starts["aaa"] = answer{err: boom}
		}},
		{"volume list", func(f *fakeDocker) { f.volumeList = answer{err: boom} }},
		{"volume list empty", func(f *fakeDocker) { f.volumeList = answer{out: "\n"} }},
		{"volume size", func(f *fakeDocker) { f.sizes[gocacheVolume] = answer{err: boom} }},
		{"volume listing", func(f *fakeDocker) { f.listings[gocacheVolume] = answer{err: boom} }},
		{"module listing", func(f *fakeDocker) { f.listings[gomodVolume] = answer{err: boom} }},
		{"module sizes", func(f *fakeDocker) { f.moduleSizes = answer{err: boom} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			f.sizes[gocacheVolume] = answer{out: strconv.FormatInt(10*kbPerGB, 10) + "\n"}
			f.listings[gocacheVolume] = answer{out: fileLine("old.bin", testNow.Add(-5*day), gb)}
			c.breakRead(f)

			p := f.plan(t)
			if p.Refused == "" {
				t.Fatal("plan was not refused on a failed read")
			}
			if len(p.Actions) > 0 {
				t.Fatalf("refused plan names actions: %v", removed(p))
			}
			if w := f.writes(); len(w) > 0 {
				t.Fatalf("plan issued write commands: %v", w)
			}
		})
	}
}

func TestPlanRefusesUnparseableListing(t *testing.T) {
	t.Parallel()
	t.Run("file listing", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.sizes[gocacheVolume] = answer{out: strconv.FormatInt(10*kbPerGB, 10) + "\n"}
		f.listings[gocacheVolume] = answer{out: "not a listing\n"}

		if p := f.plan(t); p.Refused == "" {
			t.Fatal("plan read an unparseable listing instead of refusing it")
		}
	})

	t.Run("module without a size", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.sizes[gomodVolume] = answer{out: strconv.FormatInt(6*kbPerGB, 10) + "\n"}
		f.listings[gomodVolume] = answer{out: moduleLine("github.com/one@v1.0.0", testNow.Add(-day))}
		f.moduleSizes = answer{out: moduleSizeLine("github.com/other@v1.0.0", kbPerGB)}

		if p := f.plan(t); p.Refused == "" {
			t.Fatal("plan sized a module the du listing did not name instead of refusing it")
		}
	})
}

// TestModuleListingExcludesDownloadCache pins the guard that keeps the go
// download cache out of a plan: a listing that names a cache path is refused,
// so no action can name one. The listing script also prunes the cache, which
// saves the du over it, but a unit test cannot run find, so the refusal is the
// behaviour these tests see.
func TestModuleListingExcludesDownloadCache(t *testing.T) {
	t.Parallel()

	// The sizes are canned so a listing the parser let through would reach the
	// plan as a kept row instead of refusing for a missing size.
	t.Run("listing that names the cache is refused", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.sizes[gomodVolume] = answer{out: strconv.FormatInt(6*kbPerGB, 10) + "\n"}
		f.listings[gomodVolume] = answer{out: moduleLine("cache/download/github.com/x/@v", testNow.Add(-5*day))}
		f.moduleSizes = answer{out: moduleSizeLine("cache/download/github.com/x/@v", kbPerGB)}

		p := f.plan(t)
		if p.Refused == "" {
			t.Fatalf("plan read a download-cache path: kept %v", p.Kept)
		}
		if len(p.Actions) > 0 {
			t.Fatalf("refused plan names actions: %v", removed(p))
		}
	})

	t.Run("listing that names a directory inside a module is refused", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.sizes[gomodVolume] = answer{out: strconv.FormatInt(6*kbPerGB, 10) + "\n"}
		f.listings[gomodVolume] = answer{out: moduleLine("github.com/one@v1.0.0/deeper/tree", testNow.Add(-5*day))}
		f.moduleSizes = answer{out: moduleSizeLine("github.com/one@v1.0.0/deeper/tree", kbPerGB)}

		if p := f.plan(t); p.Refused == "" {
			t.Fatalf("plan read a directory inside a module: kept %v", p.Kept)
		}
	})
}

// TestStateChanging self-tests the write guard the other tests rely on, so a
// plan that issues a state-changing command cannot pass unnoticed.
func TestStateChanging(t *testing.T) {
	t.Parallel()
	cases := []struct {
		call string
		want bool
	}{
		{"docker exec d docker ps -q", false},
		{"docker exec d docker volume ls --format {{.Name}}", false},
		{"docker exec d docker run --rm --user 0 -v c:/c:ro --entrypoint sh img -c du -sk /c", false},
		{"docker exec d docker run --rm --mount type=volume,src=c,dst=/c,readonly img", false},
		{"docker exec d docker run --rm -v c:/c img", true},
		{"docker exec d docker run --rm --mount type=volume,src=c,dst=/c img", true},
		{"docker exec d docker volume rm c", true},
		{"docker rmi img", true},
		{"docker volume prune -f", true},
	}
	for _, c := range cases {
		if got := stateChanging(c.call); got != c.want {
			t.Errorf("stateChanging(%q) = %t, want %t", c.call, got, c.want)
		}
	}
}

func TestMeasure(t *testing.T) {
	t.Parallel()
	f := newFake()
	// 40 GiB used of 100 GiB, in 1024-byte blocks, so the detail's GB figures
	// are exact and not rounded down to 0.0 or 0.1.
	f.df = answer{out: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		"overlay 104857600 41943040 62914560 40% /\n"}
	m, err := target(f).Measure(context.Background())
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if m.Used != 40*(1<<30) || m.Capacity != 100*(1<<30) {
		t.Fatalf("measurement = %+v, want used 40 GiB and capacity 100 GiB", m)
	}
	const wantDetail = "runner disk: 40.0 GB used of 100.0 GB"
	if m.Detail != wantDetail {
		t.Fatalf("detail = %q, want %q", m.Detail, wantDetail)
	}
}

func TestMeasureFailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("no runner", func(t *testing.T) {
		t.Parallel()
		if _, err := NewTarget(Config{}).Measure(context.Background()); err == nil {
			t.Fatal("measure without a runner did not error")
		}
	})
	t.Run("df fails", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.df = answer{err: errors.New("daemon unreachable")}
		if _, err := target(f).Measure(context.Background()); err == nil {
			t.Fatal("measure with a failed df did not error")
		}
	})
	t.Run("df does not parse", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.df = answer{out: "not df output\n"}
		if _, err := target(f).Measure(context.Background()); err == nil {
			t.Fatal("measure with unparseable df did not error")
		}
	})
}

// TestPlanStopsWhenCanceled drives every read of a plan, cancelling the
// caller's context at each in turn. A canceled run is the caller's to answer
// for: the plan says so as an error instead of refusing for a read that looked
// broken.
func TestPlanStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		want  string // the command the cancellation lands on
		after bool   // cancel after the call answers, not before
		ps    string // the job list the call sequence starts from
	}{
		{"dind state", "inspect -f {{.State.Running}}", false, ""},
		{"job list", "docker ps -q", false, ""},
		{"job start", "inspect -f {{.State.StartedAt}} aaa", false, "aaa\n"},
		{"after the job list", "docker ps -q", true, ""},
		{"volume list", "docker volume ls", false, ""},
		{"volume size", sizeScript, false, ""},
		{"file listing", fileListingScript, false, ""},
		{"module listing", moduleListingScript, false, ""},
		{"module sizes", moduleSizesScript, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			f.ps = answer{out: c.ps}
			f.starts["aaa"] = answer{out: "2026-10-29T09:00:00Z\n"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &cancelRunner{f: f, cancel: cancel, want: c.want, after: c.after}
			tgt := NewTarget(Config{Dind: f.dind, Runner: f.runner, Image: testImage, Run: r.run})

			if _, err := tgt.Plan(ctx, diskmaint.Policy{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("plan error = %v, want the caller's cancellation", err)
			}
			if !r.fired {
				t.Fatalf("no call matched %q: calls %v", c.want, f.calls)
			}
		})
	}
}

func TestMeasureStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelRunner{f: f, cancel: cancel, want: "df -P /"}
	tgt := NewTarget(Config{Image: testImage, Runner: f.runner, Run: r.run})

	if _, err := tgt.Measure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("measure error = %v, want the caller's cancellation", err)
	}
	if !r.fired {
		t.Fatalf("no call matched df: calls %v", f.calls)
	}
}
