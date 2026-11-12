package gtwrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

const (
	kubeTown      = "/Users/sloan/gt"
	kubeContext   = "colima-gt-control"
	kubeNamespace = "gt"
	kubeDeploy    = "daemon"
	kubeContainer = "daemon"
	kubePod       = "gt-daemon-7f9c8d6b5-x2k4p"
	kubeSelector  = "app.kubernetes.io/name=daemon"
	kubeSelectorJ = `{"app.kubernetes.io/name":"daemon"}`
)

// fakeKube answers each kubectl probe from its fields, and records every command
// line it was asked for. The defaults are a healthy cluster: the context answers,
// the Deployment selects one Running pod, and the tool is on the container's PATH.
type fakeKube struct {
	readyExit    int
	readyErr     error
	selectorOut  string
	selectorExit int
	selectorErr  error
	podsOut      string
	podsExit     int
	podsErr      error
	toolOut      string
	toolExit     int
	toolErr      error
	attachExit   int
	attachErr    error

	captured [][]string
	attached [][]string
}

func healthyKube() *fakeKube {
	return &fakeKube{
		selectorOut: kubeSelectorJ,
		podsOut:     kubePod,
		toolOut:     "/usr/local/bin/gt\n",
	}
}

func (f *fakeKube) Attach(argv []string, env []string) (int, error) {
	f.attached = append(f.attached, argv)
	return f.attachExit, f.attachErr
}

// Capture answers by the probe's kind. Each kind is a different kubectl verb line, so
// matching on the joined argv is exact enough to tell them apart.
func (f *fakeKube) Capture(_ context.Context, argv []string) (string, int, error) {
	f.captured = append(f.captured, argv)
	line := strings.Join(argv, " ")
	switch {
	case strings.Contains(line, " --raw /readyz"):
		return "ok", f.readyExit, f.readyErr
	case strings.Contains(line, " deployment "):
		return f.selectorOut, f.selectorExit, f.selectorErr
	case strings.Contains(line, " pods "):
		return f.podsOut, f.podsExit, f.podsErr
	case strings.Contains(line, " exec "):
		return f.toolOut, f.toolExit, f.toolErr
	}
	panic("unexpected probe: " + line)
}

// testKubectl is the target the cluster fake drives, with kubectl on PATH and a
// note writer the test reads.
func testKubectl(shell Shell, note *bytes.Buffer) Kubectl {
	return Kubectl{
		Town:       kubeTown,
		Context:    kubeContext,
		Namespace:  kubeNamespace,
		Deployment: kubeDeploy,
		Container:  kubeContainer,
		Tool:       "gt",
		Shell:      shell,
		LookPath:   func(string) (string, error) { return "/usr/local/bin/kubectl", nil },
		Note:       note,
	}
}

// kubeCall is the call the wrapper makes for `gt status --line`, through Plan, so the
// environment the target sees is the filtered one.
func kubeCall(t *testing.T, dir string, tty bool) Call {
	t.Helper()
	env := []string{
		"GT_ROLE=polecat",
		"TERM=xterm-256color",
		// The caller's Dolt host is replaced, so this value must not reach the pod.
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"HOME=/Users/sloan",
		"CLAUDE_CODE_OAUTH_TOKEN=placeholder",
	}
	call, err := Plan("gt", []string{"status", "--line"}, env, dir, tty)
	if err != nil {
		t.Fatalf("Plan returned an error: %v", err)
	}
	return call
}

// execPrefix is the kubectl argv up to and including the pod, container and `--`
// that precede the env command.
func execPrefix(tty bool) []string {
	argv := []string{"kubectl", "--context", kubeContext, "-n", kubeNamespace, "exec", "-i"}
	if tty {
		argv = append(argv, "-t")
	}
	return append(argv, kubePod, "-c", kubeContainer, "--")
}

func TestKubectlExecArgvUnderTheTown(t *testing.T) {
	t.Parallel()
	for _, tty := range []bool{true, false} {
		t.Run(ttyName(tty), func(t *testing.T) {
			t.Parallel()
			f := healthyKube()
			var note bytes.Buffer
			exit, err := testKubectl(f, &note).Launch(kubeCall(t, kubeTown+"/rig/polecats", tty))
			if err != nil || exit != 0 {
				t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
			}
			if len(f.attached) != 1 {
				t.Fatalf("attached %d commands, want 1", len(f.attached))
			}
			want := append(execPrefix(tty),
				"env", "GT_ROLE=polecat", "TERM=xterm-256color", testDolt,
				"sh", "-c", podWalkUpScript, "sh",
				"/Users/sloan/gt/rig/polecats", podTownRoot, "/Users/sloan/gt/rig/polecats",
				"gt", "status", "--line")
			if !slices.Equal(f.attached[0], want) {
				t.Fatalf("attached argv\n got %q\nwant %q", f.attached[0], want)
			}
			if note.Len() != 0 {
				t.Fatalf("a directory under the town wrote a note: %q", note.String())
			}
		})
	}
}

func TestKubectlExecArgvOutsideTheTown(t *testing.T) {
	t.Parallel()
	f := healthyKube()
	var note bytes.Buffer
	outside := "/Users/sloan/Documents/scratch"
	exit, err := testKubectl(f, &note).Launch(kubeCall(t, outside, false))
	if err != nil || exit != 0 {
		t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
	}
	// The walk-up's three parameters precede the tool and its arguments, and a
	// directory outside the town maps onto the pod town root.
	argv := f.attached[0]
	if got := argv[len(argv)-6]; got != podTownRoot {
		t.Fatalf("the mapped directory argument is %q, want the pod town root %q", got, podTownRoot)
	}
	if got := argv[len(argv)-4]; got != outside {
		t.Fatalf("the caller's directory argument is %q, want %q", got, outside)
	}
	if !strings.Contains(note.String(), outside) || !strings.Contains(note.String(), podTownRoot) {
		t.Fatalf("note %q does not name the caller's directory and the pod town root", note.String())
	}
}

// TestKubectlKeepsCredentialsOutOfTheExecArgv: kubectl exec puts each forwarded
// value in argv, so a credential is dropped before argv is built, under any prefix,
// and a plain GT_ name crosses. The call is built directly, not through Plan, so
// the screen in Launch is what is under test.
func TestKubectlKeepsCredentialsOutOfTheExecArgv(t *testing.T) {
	t.Parallel()
	secrets := []struct{ name, value string }{
		{"GT_DOLT_PASSWORD", "placeholder-dolt-value"},
		{"BEADS_DOLT_PASSWORD", "placeholder-beads-value"},
		{"GT_API_KEY", "placeholder-key-value"},
		{"CLAUDE_CODE_OAUTH_TOKEN", "placeholder-oauth-value"},
	}
	env := []string{"GT_ROLE=polecat"}
	for _, s := range secrets {
		env = append(env, s.name+"="+s.value)
	}
	f := healthyKube()
	var note bytes.Buffer
	exit, err := testKubectl(f, &note).Launch(Call{Argv: []string{"gt", "status", "--line"}, Env: env, Dir: kubeTown})
	if err != nil || exit != 0 {
		t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
	}
	argv := strings.Join(f.attached[0], "\n")
	for _, s := range secrets {
		if strings.Contains(argv, s.name) || strings.Contains(argv, s.value) {
			t.Fatalf("the exec argv carries %s:\n%s", s.name, argv)
		}
		if !strings.Contains(note.String(), s.name) || strings.Contains(note.String(), s.value) {
			t.Fatalf("note %q must name %s and not its value", note.String(), s.name)
		}
	}
	if !slices.Contains(f.attached[0], "GT_ROLE=polecat") {
		t.Fatalf("the plain GT_ name did not cross into the argv: %q", f.attached[0])
	}
}

// TestKubectlRefusesAnEnvEntryWithNoValue: env would run a name with no "=" as a
// command, so the call is refused before the pod is reached.
func TestKubectlRefusesAnEnvEntryWithNoValue(t *testing.T) {
	t.Parallel()
	f := healthyKube()
	var note bytes.Buffer
	exit, err := testKubectl(f, &note).Launch(Call{Argv: []string{"gt", "status"}, Env: []string{"GT_ROLE"}, Dir: kubeTown})
	if exit != 1 || err == nil {
		t.Fatalf("Launch = %d, %v; want 1 and an error", exit, err)
	}
	if len(f.attached) != 0 {
		t.Fatalf("a refused call attached %q", f.attached)
	}
}

// podRun is one tool exec the walk-up script started.
type podRun struct {
	dir string
	env map[string]string
}

// podShell is the fake shell of the daemon pod. Capture answers the ladder's
// read-only probes as the healthy cluster does. Attach runs the sh -c script the
// target emits through an in-process POSIX interpreter whose stat handler reports
// only the directories the fake pod has, so a test reads the directory and the
// environment the tool would get in the pod.
type podShell struct {
	*fakeKube
	// dirs are the pod directories that exist.
	dirs map[string]bool
	// runs records the tool execs the script started.
	runs []podRun
	// stderr holds what the script wrote to the pod's standard error.
	stderr bytes.Buffer
}

func (p *podShell) Attach(argv []string, _ []string) (int, error) {
	p.attached = append(p.attached, argv)
	script, params, env, err := shellInvocation(argv)
	if err != nil {
		return 0, err
	}
	runner, err := interp.New(
		interp.Env(expand.ListEnviron(env...)),
		interp.Dir("/"),
		interp.Params(params...),
		interp.StatHandler(p.stat),
		interp.AccessHandler(func(context.Context, string, interp.AccessMode) error { return nil }),
		interp.ExecHandler(p.exec),
		interp.StdIO(nil, io.Discard, &p.stderr),
	)
	if err != nil {
		return 0, err
	}
	node, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		return 0, err
	}
	if err := runner.Run(context.Background(), node); err != nil {
		var status interp.ExitStatus
		if errors.As(err, &status) {
			return int(status), nil
		}
		return 0, err
	}
	return 0, nil
}

// shellInvocation takes the script, the positional parameters after $0 and the
// forwarded environment out of an exec command line.
func shellInvocation(argv []string) (script string, params, env []string, err error) {
	i := slices.Index(argv, "--")
	if i < 0 || i+2 >= len(argv) || argv[i+1] != "env" {
		return "", nil, nil, fmt.Errorf("the exec command line does not run env after --: %q", argv)
	}
	rest := argv[i+2:]
	for len(rest) > 0 && strings.Contains(rest[0], "=") {
		env = append(env, rest[0])
		rest = rest[1:]
	}
	if len(rest) < 7 || rest[0] != "sh" || rest[1] != "-c" || rest[3] != "sh" {
		return "", nil, nil, fmt.Errorf("the exec command line does not run a shell script: %q", rest)
	}
	return rest[2], rest[4:], env, nil
}

// stat reports a directory the fake pod has, and absence for everything else.
func (p *podShell) stat(_ context.Context, name string, _ bool) (fs.FileInfo, error) {
	if p.dirs[path.Clean(name)] {
		return podDirInfo{}, nil
	}
	return nil, fs.ErrNotExist
}

// podDirInfo is a directory in the fake pod.
type podDirInfo struct{}

func (podDirInfo) Name() string      { return "" }
func (podDirInfo) Size() int64       { return 0 }
func (podDirInfo) Mode() fs.FileMode { return fs.ModeDir | 0o755 }
func (podDirInfo) ModTime() time.Time {
	return time.Time{}
}
func (podDirInfo) IsDir() bool { return true }
func (podDirInfo) Sys() any    { return nil }

// exec answers dirname, as the pod's shell needs to walk up, and records every
// other command as the tool exec.
func (p *podShell) exec(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	if args[0] == "dirname" {
		fmt.Fprintln(hc.Stdout, path.Dir(args[1]))
		return nil
	}
	p.runs = append(p.runs, podRun{dir: hc.Dir, env: envMap(hc.Env)})
	return nil
}

// envMap renders an interpreter environment as a map, so a test reads one
// variable without depending on order.
func envMap(env expand.Environ) map[string]string {
	m := make(map[string]string)
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Kind == expand.String {
			m[name] = vr.Str
		}
		return true
	})
	return m
}

// TestPodLaunchWalksToNearestExistingAncestor: a mapped directory the pod lacks
// runs in the nearest existing ancestor under the pod town root, through the one
// exec that runs the tool, with no separate directory probe.
func TestPodLaunchWalksToNearestExistingAncestor(t *testing.T) {
	t.Parallel()
	pod := &podShell{fakeKube: healthyKube(), dirs: map[string]bool{
		podTownRoot:              true,
		podTownRoot + "/gastown": true,
	}}
	var note bytes.Buffer
	exit, err := testKubectl(pod, &note).Launch(kubeCall(t, kubeTown+"/gastown/crew/x", false))
	if err != nil || exit != 0 {
		t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
	}
	if len(pod.runs) != 1 {
		t.Fatalf("the tool ran %d times, want once; stderr: %q", len(pod.runs), pod.stderr.String())
	}
	if got, want := pod.runs[0].dir, podTownRoot+"/gastown"; got != want {
		t.Fatalf("the tool ran in %q, want the nearest existing ancestor %q", got, want)
	}
	for _, argv := range pod.captured {
		if line := strings.Join(argv, " "); strings.Contains(line, "test -d") {
			t.Fatalf("the target ran a separate directory probe: %q", line)
		}
	}
}

// TestPodLaunchSetsCallerCwdOnlyWhenWalked: the tool is told the Mac directory
// only when the pod directory differs from the mapped path. A directory that maps
// to itself, and one outside the town, keep the forwarded environment unchanged.
func TestPodLaunchSetsCallerCwdOnlyWhenWalked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dir     string
		dirs    map[string]bool
		wantSet bool
	}{
		{
			name:    "walked to an ancestor",
			dir:     kubeTown + "/gastown/crew/x",
			dirs:    map[string]bool{podTownRoot: true, podTownRoot + "/gastown": true},
			wantSet: true,
		},
		{
			name:    "the directory maps to itself",
			dir:     kubeTown + "/gastown/crew/x",
			dirs:    map[string]bool{podTownRoot: true, podTownRoot + "/gastown/crew/x": true},
			wantSet: false,
		},
		{
			name:    "outside the town",
			dir:     "/Users/sloan/Documents/scratch",
			dirs:    map[string]bool{podTownRoot: true},
			wantSet: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod := &podShell{fakeKube: healthyKube(), dirs: tt.dirs}
			var note bytes.Buffer
			exit, err := testKubectl(pod, &note).Launch(kubeCall(t, tt.dir, false))
			if err != nil || exit != 0 {
				t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
			}
			if len(pod.runs) != 1 {
				t.Fatalf("the tool ran %d times, want once; stderr: %q", len(pod.runs), pod.stderr.String())
			}
			got, set := pod.runs[0].env["GT_CALLER_CWD"]
			if set != tt.wantSet {
				t.Fatalf("GT_CALLER_CWD = %q, set = %v; want set = %v", got, set, tt.wantSet)
			}
			if tt.wantSet && got != tt.dir {
				t.Fatalf("GT_CALLER_CWD = %q, want the caller's directory %q", got, tt.dir)
			}
		})
	}
}

// TestPodLaunchRefusesWhenTownRootAbsent: when even the pod town root is absent,
// the wrapper refuses in one line naming it, exits 125, and the tool does not start.
func TestPodLaunchRefusesWhenTownRootAbsent(t *testing.T) {
	t.Parallel()
	pod := &podShell{fakeKube: healthyKube(), dirs: map[string]bool{}}
	var note bytes.Buffer
	exit, err := testKubectl(pod, &note).Launch(kubeCall(t, kubeTown+"/gastown/crew/x", false))
	if err != nil {
		t.Fatalf("Launch returned an error: %v", err)
	}
	if exit != 125 {
		t.Fatalf("Launch = %d, want 125", exit)
	}
	if len(pod.runs) != 0 {
		t.Fatalf("the tool ran in %q, want it not to start", pod.runs[0].dir)
	}
	line := strings.TrimSuffix(pod.stderr.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("the refusal is more than one line: %q", line)
	}
	if !strings.Contains(line, podTownRoot) {
		t.Fatalf("the refusal %q does not name the pod town root %q", line, podTownRoot)
	}
}

func TestPodDirMapsTheTownOntoThePodTownRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		dir    string
		want   string
		mapped bool
	}{
		{name: "the town itself", dir: kubeTown, want: podTownRoot, mapped: true},
		{name: "a directory under the town", dir: kubeTown + "/rig/polecats/cheedo", want: podTownRoot + "/rig/polecats/cheedo", mapped: true},
		{name: "a sibling sharing the town's prefix", dir: kubeTown + "-old/rig", want: podTownRoot, mapped: false},
		{name: "the parent of the town", dir: "/Users/sloan", want: podTownRoot, mapped: false},
		{name: "a directory elsewhere", dir: "/tmp/work", want: podTownRoot, mapped: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, mapped := podDir(kubeTown, tt.dir)
			if got != tt.want || mapped != tt.mapped {
				t.Fatalf("podDir(%q, %q) = %q, %v; want %q, %v", kubeTown, tt.dir, got, mapped, tt.want, tt.mapped)
			}
		})
	}
}

// TestKubectlLookupsAreExactPerCall pins the probe argv of the ladder: the context,
// the Deployment's own selector read, and the pod list filtered to Running pods.
func TestKubectlLookupsAreExactPerCall(t *testing.T) {
	t.Parallel()
	f := healthyKube()
	var note bytes.Buffer
	if _, err := testKubectl(f, &note).Launch(kubeCall(t, kubeTown, false)); err != nil {
		t.Fatalf("Launch returned an error: %v", err)
	}
	want := [][]string{
		{"kubectl", "--context", kubeContext, "get", "--raw", "/readyz"},
		{"kubectl", "--context", kubeContext, "-n", kubeNamespace, "get", "deployment", kubeDeploy, "-o", "jsonpath={.spec.selector.matchLabels}"},
		{"kubectl", "--context", kubeContext, "-n", kubeNamespace, "get", "pods", "-l", kubeSelector, "--field-selector=status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}"},
	}
	if len(f.captured) < len(want) {
		t.Fatalf("ran %d probes, want at least %d", len(f.captured), len(want))
	}
	for i, argv := range want {
		if !slices.Equal(f.captured[i], argv) {
			t.Fatalf("probe %d\n got %q\nwant %q", i, f.captured[i], argv)
		}
	}
}

// TestKubectlDoctorRungFailures drives each rung to its failure and asserts the
// diagnosis names it. Launch refuses the same call with exit 1 and no exec.
func TestKubectlDoctorRungFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		lookPath   error
		setup      func(*fakeKube)
		wantReason string
	}{
		{
			name:       "kubectl absent from PATH",
			lookPath:   errors.New("not found"),
			wantReason: "kubectl is not on PATH",
		},
		{
			name:       "an unreachable context",
			setup:      func(f *fakeKube) { f.readyExit = 1 },
			wantReason: "is not reachable",
		},
		{
			name:       "a context whose probe does not run",
			setup:      func(f *fakeKube) { f.readyErr = errors.New("exec: refused") },
			wantReason: "is not reachable",
		},
		{
			name:       "a Deployment selector read that does not run",
			setup:      func(f *fakeKube) { f.selectorErr = errors.New("exec: refused") },
			wantReason: "cannot be read",
		},
		{
			name:       "a Deployment selector read that exits non-zero",
			setup:      func(f *fakeKube) { f.selectorExit = 1 },
			wantReason: "cannot be read",
		},
		{
			name:       "a tool check that does not run",
			setup:      func(f *fakeKube) { f.toolErr = errors.New("exec: refused") },
			wantReason: "check that gt answers in container daemon",
		},
		{
			name:       "a Deployment selector that is not a label map",
			setup:      func(f *fakeKube) { f.selectorOut = "not json" },
			wantReason: "cannot be read",
		},
		{
			name:       "a Deployment selector that names no label",
			setup:      func(f *fakeKube) { f.selectorOut = "{}" },
			wantReason: "cannot be read",
		},
		{
			name:       "no Running pod",
			setup:      func(f *fakeKube) { f.podsOut = "" },
			wantReason: "has 0 Running pods, and the call needs exactly one",
		},
		{
			name:       "two Running pods",
			setup:      func(f *fakeKube) { f.podsOut = kubePod + " gt-daemon-7f9c8d6b5-zz9qr" },
			wantReason: "has 2 Running pods, and the call needs exactly one",
		},
		{
			name:       "a pod name the wrapper refuses",
			setup:      func(f *fakeKube) { f.podsOut = "Bad_Name" },
			wantReason: "refuses to pass to kubectl",
		},
		{
			name:       "the tool missing in the container",
			setup:      func(f *fakeKube) { f.toolOut = "" },
			wantReason: "does not answer in container daemon",
		},
		{
			name:       "the tool check exits non-zero",
			setup:      func(f *fakeKube) { f.toolExit = 1; f.toolOut = "/usr/local/bin/gt" },
			wantReason: "does not answer in container daemon",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := healthyKube()
			if tt.setup != nil {
				tt.setup(f)
			}
			var note bytes.Buffer
			k := testKubectl(f, &note)
			if tt.lookPath != nil {
				k.LookPath = func(string) (string, error) { return "", tt.lookPath }
			}
			diag, err := k.Doctor()
			if err != nil {
				t.Fatalf("Doctor returned an error: %v", err)
			}
			if diag.Rung != RungDown {
				t.Fatalf("rung = %s, want down; reason %q", diag.Rung, diag.Reason)
			}
			if !strings.Contains(diag.Reason, tt.wantReason) {
				t.Fatalf("reason %q does not contain %q", diag.Reason, tt.wantReason)
			}
		})
	}
}

// TestKubectlLaunchRefusesWhenNoRungHolds: a rung that fails before the pod is
// known refuses the call with exit 1 and runs nothing in the container.
func TestKubectlLaunchRefusesWhenNoRungHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*fakeKube)
	}{
		{name: "an unreachable context", setup: func(f *fakeKube) { f.readyExit = 1 }},
		{name: "no Running pod", setup: func(f *fakeKube) { f.podsOut = "" }},
		{name: "two Running pods", setup: func(f *fakeKube) { f.podsOut = kubePod + " gt-daemon-7f9c8d6b5-zz9qr" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := healthyKube()
			tt.setup(f)
			var note bytes.Buffer
			exit, err := testKubectl(f, &note).Launch(kubeCall(t, kubeTown, false))
			if exit != 1 || err == nil {
				t.Fatalf("Launch = %d, %v; want 1 and an error", exit, err)
			}
			if len(f.attached) != 0 {
				t.Fatalf("a refused call attached %q", f.attached)
			}
		})
	}
}

// TestKubectlRefusesACallOrToolItCannotRun: a call with no tool, and a doctor for a
// name the wrapper was not installed as, are refused before any probe runs.
func TestKubectlRefusesACallOrToolItCannotRun(t *testing.T) {
	t.Parallel()
	f := healthyKube()
	var note bytes.Buffer
	k := testKubectl(f, &note)
	if exit, err := k.Launch(Call{Dir: kubeTown}); exit != 1 || err == nil {
		t.Fatalf("Launch of a call with no tool = %d, %v; want 1 and an error", exit, err)
	}
	k.Tool = "gt-wrap"
	if _, err := k.Doctor(); err == nil {
		t.Fatal("Doctor answered for a tool name the wrapper is not installed as")
	}
	if len(f.captured) != 0 || len(f.attached) != 0 {
		t.Fatalf("a refused call reached the cluster: probes %q, attached %q", f.captured, f.attached)
	}
}

func TestKubectlDoctorReportsExecWhenEveryRungHolds(t *testing.T) {
	t.Parallel()
	f := healthyKube()
	var note bytes.Buffer
	diag, err := testKubectl(f, &note).Doctor()
	if err != nil {
		t.Fatalf("Doctor returned an error: %v", err)
	}
	if diag.Rung != RungExec {
		t.Fatalf("rung = %s, want exec; reason %q", diag.Rung, diag.Reason)
	}
	if diag.Container != kubePod+"/"+kubeContainer {
		t.Fatalf("container = %q, want %q", diag.Container, kubePod+"/"+kubeContainer)
	}
	if !strings.Contains(diag.Reason, kubePod) || !strings.Contains(diag.Reason, "gt answers") {
		t.Fatalf("reason %q does not name the pod and the tool", diag.Reason)
	}
	if len(f.attached) != 0 {
		t.Fatal("Doctor ran a command in the container")
	}
}

// TestKubectlRefusesAnUnwiredOrIncompleteTarget: a missing dependency or config
// field refuses the call, and nothing reaches the cluster.
func TestKubectlRefusesAnUnwiredOrIncompleteTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Kubectl)
		want   string
	}{
		{name: "an empty context", change: func(k *Kubectl) { k.Context = "" }, want: "context"},
		{name: "an empty namespace", change: func(k *Kubectl) { k.Namespace = "" }, want: "namespace"},
		{name: "an empty deployment", change: func(k *Kubectl) { k.Deployment = "" }, want: "deployment"},
		{name: "an empty container", change: func(k *Kubectl) { k.Container = "" }, want: "container"},
		{name: "a context read as a flag", change: func(k *Kubectl) { k.Context = "--kubeconfig=/tmp/x" }, want: "context"},
		{name: "no town", change: func(k *Kubectl) { k.Town = "" }, want: "town"},
		{name: "no kubectl check", change: func(k *Kubectl) { k.LookPath = nil }, want: "kubectl check"},
		{name: "no note writer", change: func(k *Kubectl) { k.Note = nil }, want: "note writer"},
		{name: "no shell", change: func(k *Kubectl) { k.Shell = nil }, want: "shell"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := healthyKube()
			var note bytes.Buffer
			k := testKubectl(f, &note)
			tt.change(&k)
			exit, err := k.Launch(kubeCall(t, kubeTown, false))
			if exit != 1 || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Launch = %d, %v; want 1 with an error naming %q", exit, err, tt.want)
			}
			if _, err := k.Doctor(); err == nil {
				t.Fatal("Doctor answered for a target it refuses to run")
			}
			if len(f.captured) != 0 || len(f.attached) != 0 {
				t.Fatalf("a refused target reached the cluster: probes %q, attached %q", f.captured, f.attached)
			}
		})
	}
}

// TestPodTownRootMatchesTheRenderedDeployment pins podTownRoot to the town path
// the daemon Deployment is rendered with, so the two cannot drift apart.
func TestPodTownRootMatchesTheRenderedDeployment(t *testing.T) {
	t.Parallel()
	if podTownRoot != k8sdaemon.TownRootPath {
		t.Fatalf("podTownRoot = %q, want the rendered TownRootPath %q", podTownRoot, k8sdaemon.TownRootPath)
	}
}
