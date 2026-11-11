package gtwrap

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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
	dirExit      int
	dirErr       error
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
	case strings.Contains(line, " -- test -d "):
		return "", f.dirExit, f.dirErr
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
				"sh", "-c", `cd "$1" && shift && exec "$@"`, "sh",
				"/Users/sloan/gt/rig/polecats", "gt", "status", "--line")
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
	// The tool and its arguments end the argv, so the directory is the fourth from the end.
	argv := f.attached[0]
	if got := argv[len(argv)-4]; got != podTownRoot {
		t.Fatalf("the directory argument is %q, want the pod town root %q", got, podTownRoot)
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

// TestKubectlChecksTheMappedDirectoryInThePod: before the tool runs, the directory
// the call maps onto is checked as a directory in the pod's container.
func TestKubectlChecksTheMappedDirectoryInThePod(t *testing.T) {
	t.Parallel()
	f := healthyKube()
	var note bytes.Buffer
	if _, err := testKubectl(f, &note).Launch(kubeCall(t, kubeTown+"/rig/polecats", false)); err != nil {
		t.Fatalf("Launch returned an error: %v", err)
	}
	want := []string{"kubectl", "--context", kubeContext, "-n", kubeNamespace, "exec", kubePod, "-c", kubeContainer, "--", "test", "-d", podTownRoot + "/rig/polecats"}
	if len(f.captured) == 0 || !slices.Equal(f.captured[len(f.captured)-1], want) {
		t.Fatalf("the last probe was %q, want the directory check %q", f.captured, want)
	}
}

// TestKubectlRefusesAMappedDirectoryMissingInThePod: a directory the pod does not
// have is the wrapper's refusal with exit 1, and the tool never runs. A check that
// does not run refuses too.
func TestKubectlRefusesAMappedDirectoryMissingInThePod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(*fakeKube)
		wantError string
	}{
		{name: "the directory is missing", setup: func(f *fakeKube) { f.dirExit = 1 }, wantError: "is not a directory in container daemon"},
		{name: "the directory check does not run", setup: func(f *fakeKube) { f.dirErr = errors.New("exec: refused") }, wantError: "check that"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := healthyKube()
			tt.setup(f)
			var note bytes.Buffer
			exit, err := testKubectl(f, &note).Launch(kubeCall(t, kubeTown+"/rig/gone", false))
			if exit != 1 || err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Launch = %d, %v; want 1 with an error containing %q", exit, err, tt.wantError)
			}
			if len(f.attached) != 0 {
				t.Fatalf("a call with a missing directory attached %q", f.attached)
			}
		})
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
