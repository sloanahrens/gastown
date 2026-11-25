package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/deploy/k8s"
	"github.com/steveyegge/gastown/deploy/k8s/sbx"
)

// sbxEngineObjects is a desired set in an order its own file names would sort
// differently, so a render that sorts or renumbers the set fails the order test.
var sbxEngineObjects = []k8s.StaticObject{
	{Kind: "Namespace", Name: "agent-sandbox-system", Doc: []byte("namespace doc\n")},
	{Kind: "Deployment", Name: "agent-sandbox-controller", Doc: []byte("deployment doc\n")},
	{Kind: "PersistentVolumeClaim", Name: "gt-cache", Doc: []byte("claim doc\n")},
}

// sbxEngineDeps is the render and the writer over a fake render, with a read
// and a helm binary that fail if the write path reaches them.
func sbxEngineDeps(t *testing.T, set sbx.Set, received *sbx.Inputs) clusterRenderDeps {
	t.Helper()
	return clusterRenderDeps{
		readFile: func(string) ([]byte, error) {
			t.Error("runSbxRender read a file itself; sbx.Render reads through the reader it was given")
			return nil, errors.New("planted read")
		},
		renderSbx: func(_ context.Context, in sbx.Inputs) (sbx.Set, error) {
			*received = in
			return set, nil
		},
		runHelm: func(context.Context, string, ...string) ([]byte, error) {
			t.Error("runSbxRender ran helm itself; the render runs through the runner it was given")
			return nil, errors.New("planted helm")
		},
		writeFile: func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		out:       io.Discard,
	}
}

// The files carry sbx.Render's order, numbered as the control render numbers
// its own, and each holds the object's reviewed document.
func TestClusterRenderSbxEngineWritesTheDesiredSetInApplyOrder(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	var received sbx.Inputs
	d := sbxEngineDeps(t, sbx.Set{Objects: sbxEngineObjects}, &received)
	err := runSbxRender(d, sbxRenderInputs{OutDir: out, Manifests: "m", Chart: "c", Values: "v"})
	if err != nil {
		t.Fatalf("runSbxRender(): %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatalf("reading %s: %v", out, err)
	}
	want := []string{
		"01-namespace-agent-sandbox-system.yaml",
		"02-deployment-agent-sandbox-controller.yaml",
		"03-persistentvolumeclaim-gt-cache.yaml",
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("files in %s = %q, want %q", out, got, want)
	}
	for i, name := range want {
		body, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.Equal(body, sbxEngineObjects[i].Doc) {
			t.Errorf("%s = %q, want the object's document %q", name, body, sbxEngineObjects[i].Doc)
		}
	}
}

// After the files, the render prints one expected Secret line per name in the
// set's Secrets, in the set's order, and nothing else about them.
func TestClusterRenderSbxEnginePrintsTheExpectedSecretNames(t *testing.T) {
	t.Parallel()
	var printed bytes.Buffer
	var received sbx.Inputs
	d := sbxEngineDeps(t, sbx.Set{
		Objects: sbxEngineObjects,
		Secrets: []string{"openshell-server-tls", "openshell-jwt-keys"},
	}, &received)
	d.out = &printed
	d.writeFile = func(path string, data []byte) error {
		if printed.Len() > 0 {
			t.Errorf("runSbxRender wrote %s after it printed %q; the Secret names follow the files",
				filepath.Base(path), printed.String())
		}
		return os.WriteFile(path, data, 0o644)
	}
	err := runSbxRender(d, sbxRenderInputs{OutDir: t.TempDir(), Manifests: "m", Chart: "c", Values: "v"})
	if err != nil {
		t.Fatalf("runSbxRender(): %v", err)
	}
	want := "expected Secret: openshell-server-tls\nexpected Secret: openshell-jwt-keys\n"
	if got := printed.String(); got != want {
		t.Fatalf("printed %q, want %q", got, want)
	}
}

// A set that names no Secrets prints no expected Secret line: the render says
// nothing about Secrets it does not expect, and reads neither their values nor
// the cluster. The deps plant a failing reader and helm runner, so a read along
// the render's inputs surfaces, and the files the set does hold are still
// written, so an empty output cannot come from a render that did nothing.
func TestClusterRenderSbxEnginePrintsNoLineForAnEmptySecretSet(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	var printed bytes.Buffer
	var received sbx.Inputs
	d := sbxEngineDeps(t, sbx.Set{Objects: sbxEngineObjects}, &received)
	d.out = &printed
	if err := runSbxRender(d, sbxRenderInputs{OutDir: out, Manifests: "m", Chart: "c", Values: "v"}); err != nil {
		t.Fatalf("runSbxRender(): %v", err)
	}
	if names := dirNames(t, out); len(names) != len(sbxEngineObjects) {
		t.Fatalf("%d files in %s, want the set's %d objects, so the empty output is the render's", len(names), out, len(sbxEngineObjects))
	}
	if got := printed.String(); got != "" {
		t.Fatalf("printed %q for a set with no Secrets, want no line and no Secret value", got)
	}
}

// The three pinned paths and the helm binary reach sbx.Render through the
// inputs: the render reads the manifest path it was given, and its helm call
// runs the binary in.Helm with the arguments it was handed.
func TestClusterRenderSbxEngineHandsTheInputsToTheRender(t *testing.T) {
	t.Parallel()
	var received sbx.Inputs
	var ranName string
	var ranArgs []string
	d := sbxEngineDeps(t, sbx.Set{}, &received)
	d.runHelm = func(_ context.Context, name string, args ...string) ([]byte, error) {
		ranName, ranArgs = name, slices.Clone(args)
		return []byte("chart"), nil
	}
	err := runSbxRender(d, sbxRenderInputs{
		OutDir:    t.TempDir(),
		Manifests: "/pin/sandbox.yaml",
		Chart:     "/chart",
		Values:    "/values.yaml",
		Helm:      "/opt/homebrew/bin/helm",
	})
	if err != nil {
		t.Fatalf("runSbxRender(): %v", err)
	}
	if want := "/pin/sandbox.yaml"; received.AgentSandboxPath != want {
		t.Errorf("AgentSandboxPath = %q, want %q", received.AgentSandboxPath, want)
	}
	if want := "/chart"; received.ChartDir != want {
		t.Errorf("ChartDir = %q, want %q", received.ChartDir, want)
	}
	if want := "/values.yaml"; received.ValuesPath != want {
		t.Errorf("ValuesPath = %q, want %q", received.ValuesPath, want)
	}
	if received.Read == nil || received.Run == nil {
		t.Fatalf("Inputs = %+v, want a reader and a runner", received)
	}
	args := []string{"template", "/chart", "-f", "/values.yaml"}
	if _, err := received.Run(context.Background(), "helm", args...); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if want := "/opt/homebrew/bin/helm"; ranName != want {
		t.Errorf("helm ran as %q, want the binary in.Helm %q", ranName, want)
	}
	if !slices.Equal(ranArgs, args) {
		t.Errorf("helm args = %q, want %q", ranArgs, args)
	}
}

// A render error and a write error come back naming what failed, and neither
// leaves a file behind.
func TestClusterRenderSbxEngineReturnsTheRenderAndWriteErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		objects []k8s.StaticObject
		render  error
		write   error
		want    []string
	}{
		{
			name:   "a render error",
			render: errors.New("planted render"),
			want:   []string{"cannot render the gt-sbx desired set", "planted render"},
		},
		{
			name:    "a write error",
			objects: sbxEngineObjects[:1],
			write:   errors.New("planted write"),
			want:    []string{"cannot write", "01-namespace-agent-sandbox-system.yaml", "planted write"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			d := sbxEngineDeps(t, sbx.Set{Objects: tc.objects}, &sbx.Inputs{})
			if tc.render != nil {
				d.renderSbx = func(context.Context, sbx.Inputs) (sbx.Set, error) {
					return sbx.Set{}, tc.render
				}
			}
			d.writeFile = func(string, []byte) error {
				if tc.write == nil {
					t.Fatal("the engine wrote a file beside the render error")
				}
				return tc.write
			}
			err := runSbxRender(d, sbxRenderInputs{OutDir: out, Manifests: "m", Chart: "c", Values: "v"})
			if err == nil {
				t.Fatalf("runSbxRender() = nil, want an error naming %q", tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			entries, err := os.ReadDir(out)
			if err != nil {
				t.Fatalf("reading %s: %v", out, err)
			}
			if len(entries) != 0 {
				t.Errorf("%d files written, want none", len(entries))
			}
		})
	}
}

// refuseSbxRender runs runSbxRender over the fakes with in, failing if the
// render ran or the error does not name want, and returns the error.
func refuseSbxRender(t *testing.T, in sbxRenderInputs, want string) error {
	t.Helper()
	d := sbxEngineDeps(t, sbx.Set{Objects: sbxEngineObjects}, &sbx.Inputs{})
	d.renderSbx = func(context.Context, sbx.Inputs) (sbx.Set, error) {
		t.Error("the render ran, want the refusal first")
		return sbx.Set{}, nil
	}
	err := runSbxRender(d, in)
	if err == nil {
		t.Fatalf("runSbxRender() = nil, want an error naming %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
	return err
}

// A missing input is refused by name before the render runs, and the refusal writes nothing.
func TestClusterRenderSbxEngineRefusesAMissingInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		drop func(*sbxRenderInputs)
		want string
	}{
		{"manifests", func(in *sbxRenderInputs) { in.Manifests = "" }, "no manifests input"},
		{"chart", func(in *sbxRenderInputs) { in.Chart = "" }, "no chart input"},
		{"values", func(in *sbxRenderInputs) { in.Values = "" }, "no values input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			in := sbxRenderInputs{OutDir: out, Manifests: "m", Chart: "c", Values: "v"}
			tc.drop(&in)
			refuseSbxRender(t, in, tc.want)
			if names := dirNames(t, out); len(names) != 0 {
				t.Errorf("%d files written, want none", len(names))
			}
		})
	}
}

// A bad out directory is refused by name before the render runs, and the refusal writes nothing.
func TestClusterRenderSbxEngineRefusesABadOutDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T) string
		want   string
		verify func(t *testing.T, out string)
	}{
		{
			name: "not empty",
			setup: func(t *testing.T) string {
				out := t.TempDir()
				if err := os.WriteFile(filepath.Join(out, "other.yaml"), []byte("another apply set\n"), 0o644); err != nil {
					t.Fatalf("planting a file: %v", err)
				}
				return out
			},
			want: "is not empty",
			verify: func(t *testing.T, out string) {
				if names := dirNames(t, out); !slices.Equal(names, []string{"other.yaml"}) {
					t.Errorf("files in %s = %q, want only the planted file", out, names)
				}
			},
		},
		{
			name:  "does not exist",
			setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			want:  "does not exist",
			verify: func(t *testing.T, out string) {
				if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("out directory %s = %v, want it still absent: the refusal creates no directory", out, err)
				}
			},
		},
		{
			name: "is a file",
			setup: func(t *testing.T) string {
				out := filepath.Join(t.TempDir(), "out")
				if err := os.WriteFile(out, []byte("not a directory\n"), 0o644); err != nil {
					t.Fatalf("writing the file: %v", err)
				}
				return out
			},
			want: "cannot be read as a directory",
		},
		{
			name:  "unnamed",
			setup: func(t *testing.T) string { return "" },
			want:  "no out directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := tc.setup(t)
			err := refuseSbxRender(t, sbxRenderInputs{OutDir: out, Manifests: "m", Chart: "c", Values: "v"}, tc.want)
			if out != "" && !strings.Contains(err.Error(), out) {
				t.Errorf("error %q does not name the out directory %s", err, out)
			}
			if tc.verify != nil {
				tc.verify(t, out)
			}
		})
	}
}

// sbxEngineHelmOutput is the chart output production sbx.OpenShell parses: the
// gateway StatefulSet it sizes Guaranteed and the certgen hook Job that names
// the three certificate Secrets. It is not the pulled chart, so the test stays
// hermetic, while the helm call and its arguments are production's.
const sbxEngineHelmOutput = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: openshell
  namespace: openshell
spec:
  template:
    spec:
      containers:
        - name: openshell
---
apiVersion: batch/v1
kind: Job
metadata:
  name: openshell-certgen
  namespace: openshell
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
spec:
  template:
    spec:
      containers:
        - name: certgen
          args:
            - --server-secret-name
            - openshell-server-tls
            - --client-secret-name
            - openshell-client-tls
            - --jwt-secret-name
            - openshell-jwt-keys
`

// sbxEngineCall is one program the render ran, with its arguments.
type sbxEngineCall struct {
	name string
	args []string
}

// The render runs exactly one program, the helm binary it was given, and hands
// it the chart directory and the values file. The render step is production
// sbx.OpenShell over the recorded runner, so the arguments are the ones
// production names and not ones the test restates.
func TestClusterRenderSbxEngineRunsOnlyTheNamedHelmCall(t *testing.T) {
	t.Parallel()
	var calls []sbxEngineCall
	d := sbxEngineDeps(t, sbx.Set{}, &sbx.Inputs{})
	d.renderSbx = func(ctx context.Context, in sbx.Inputs) (sbx.Set, error) {
		chart, err := sbx.OpenShell(ctx, in.Run, in.ChartDir, in.ValuesPath)
		if err != nil {
			return sbx.Set{}, err
		}
		return sbx.Set{Objects: chart.Objects}, nil
	}
	d.runHelm = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, sbxEngineCall{name: name, args: slices.Clone(args)})
		return []byte(sbxEngineHelmOutput), nil
	}
	err := runSbxRender(d, sbxRenderInputs{
		OutDir:    t.TempDir(),
		Manifests: "m",
		Chart:     "/chart",
		Values:    "/values.yaml",
		Helm:      "/opt/helm",
	})
	if err != nil {
		t.Fatalf("runSbxRender(): %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("%d programs run, want only the helm call: %v", len(calls), calls)
	}
	call := calls[0]
	if want := "/opt/helm"; call.name != want {
		t.Errorf("the render ran %q, want the helm binary it was given, %q", call.name, want)
	}
	if want := "/chart"; !slices.Contains(call.args, want) {
		t.Errorf("helm args = %q, want the chart directory %q", call.args, want)
	}
	at := slices.Index(call.args, "-f")
	if at < 0 || at+1 >= len(call.args) || call.args[at+1] != "/values.yaml" {
		t.Errorf("helm args = %q, want -f followed by the values file %q", call.args, "/values.yaml")
	}
}

// fileNameNumber splits a rendered file name into the number it is prefixed
// with and the width that number is padded to.
func fileNameNumber(t *testing.T, name string) (number, width int) {
	t.Helper()
	width = len(name) - len(strings.TrimLeft(name, "0123456789"))
	number, err := strconv.Atoi(name[:width])
	if err != nil {
		t.Fatalf("file %q does not start with a file number: %v", name, err)
	}
	return number, width
}

// The gt-sbx files are numbered in sbx.Render's order and padded to the width
// the control render pads its own, so the two apply sets read alike. The width
// is read from a control render, not restated here: a width change in either
// render alone leaves this red.
func TestClusterRenderSbxEngineNumbersWithTheControlRenderWidth(t *testing.T) {
	t.Parallel()
	control := clusterRenderFixture(t)
	if err := runClusterRender(clusterRenderTestDeps(t, io.Discard), control); err != nil {
		t.Fatalf("control render: %v", err)
	}
	controlNames := dirNames(t, control.out)
	if len(controlNames) == 0 {
		t.Fatal("the control render wrote no file, so this test cannot read its number width")
	}
	_, wantWidth := fileNameNumber(t, controlNames[0])

	out := t.TempDir()
	var received sbx.Inputs
	d := sbxEngineDeps(t, sbx.Set{Objects: sbxEngineObjects}, &received)
	if err := runSbxRender(d, sbxRenderInputs{OutDir: out, Manifests: "m", Chart: "c", Values: "v"}); err != nil {
		t.Fatalf("runSbxRender(): %v", err)
	}
	names := dirNames(t, out)
	if len(names) != len(sbxEngineObjects) {
		t.Fatalf("%d files in %s, want %d", len(names), out, len(sbxEngineObjects))
	}
	for _, name := range names {
		number, width := fileNameNumber(t, name)
		if width != wantWidth {
			t.Errorf("file %q is numbered with %d digits, want the control render's %d", name, width, wantWidth)
		}
		if number < 1 || number > len(sbxEngineObjects) {
			t.Errorf("file %q is numbered %d, outside the set's %d objects", name, number, len(sbxEngineObjects))
			continue
		}
		object := sbxEngineObjects[number-1]
		want := fmt.Sprintf("%0*d-%s-%s.yaml", wantWidth, number, strings.ToLower(object.Kind), object.Name)
		if name != want {
			t.Errorf("file %q is numbered %d, but sbx.Render returns %s %s at that position, so the name wants to be %q",
				name, number, object.Kind, object.Name, want)
		}
	}
}

// --cluster gt-sbx reaches runSbxRender through runClusterRender with the three
// paths and the --helm binary, and the command prints the engine's Secret lines.
// The sandbox render reads no town, so the town lookup is planted to fail.
func TestClusterRenderSbxClusterDispatchesToTheEngine(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	var got sbx.Inputs
	var helmName string
	var stdout bytes.Buffer
	d := clusterRenderDeps{
		findTown: func() (string, error) {
			t.Error("the gt-sbx render looked up the town; it reads none")
			return "", errors.New("planted town")
		},
		readFile: func(string) ([]byte, error) {
			t.Error("the gt-sbx render read a file itself; sbx.Render reads through the reader it was given")
			return nil, errors.New("planted read")
		},
		renderSbx: func(ctx context.Context, in sbx.Inputs) (sbx.Set, error) {
			got = in
			if _, err := in.Run(ctx, "helm", "template"); err != nil {
				return sbx.Set{}, err
			}
			return sbx.Set{Objects: sbxEngineObjects, Secrets: []string{"openshell-secrets"}}, nil
		},
		runHelm: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			helmName = name
			return nil, nil
		},
		writeFile: func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		out:       &stdout,
	}
	f := clusterRenderFlags{
		cluster:      clusterSbx,
		out:          out,
		sbxManifests: "/pin/agent-sandbox.yaml",
		sbxChart:     "/chart",
		sbxValues:    "/values.yaml",
		helm:         "/opt/helm",
	}
	if err := runClusterRender(d, f); err != nil {
		t.Fatalf("runClusterRender(--cluster %s): %v", clusterSbx, err)
	}
	if got.AgentSandboxPath != "/pin/agent-sandbox.yaml" || got.ChartDir != "/chart" || got.ValuesPath != "/values.yaml" {
		t.Errorf("runSbxRender got manifests %q, chart %q, values %q; want the three paths it was given",
			got.AgentSandboxPath, got.ChartDir, got.ValuesPath)
	}
	if want := "/opt/helm"; helmName != want {
		t.Errorf("the render ran %q, want the --helm binary %q", helmName, want)
	}
	if want := "expected Secret: openshell-secrets\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want the engine's output %q", stdout.String(), want)
	}
}

// TestClusterRenderSbxClusterFlagsAreRegistered: the gt-sbx flags exist with
// their defaults, so a control render that names none of them keeps the
// control cluster and empty sbx inputs.
func TestClusterRenderSbxClusterFlagsAreRegistered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, want string }{
		{"cluster", clusterControl},
		{"sbx-manifests", ""},
		{"sbx-chart", ""},
		{"sbx-values", ""},
		{"helm", "helm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flag := townClusterRenderCmd.Flags().Lookup(tc.name)
			if flag == nil {
				t.Fatalf("--%s is not registered on gt town cluster render", tc.name)
			}
			if flag.DefValue != tc.want {
				t.Errorf("--%s defaults to %q, want %q", tc.name, flag.DefValue, tc.want)
			}
		})
	}
}

// TestClusterRenderSbxClusterRefusals: each refused run is named before the town,
// the engine or a write; an empty cluster renders the control set.
func TestClusterRenderSbxClusterRefusals(t *testing.T) {
	t.Parallel()
	sbxRun := clusterRenderFlags{cluster: clusterSbx, sbxManifests: "m", sbxChart: "c", sbxValues: "v"}
	controlRun := clusterRenderFlags{cluster: clusterControl}
	for _, tc := range []struct {
		name string
		base clusterRenderFlags
		edit func(*clusterRenderFlags)
		want []string
	}{
		{"an unknown cluster", clusterRenderFlags{}, func(f *clusterRenderFlags) { f.cluster = "gt-prod" }, []string{"gt-prod", clusterControl, clusterSbx}},
		{"gt-sbx without --sbx-manifests", sbxRun, func(f *clusterRenderFlags) { f.sbxManifests = "" }, []string{"--sbx-manifests"}},
		{"gt-sbx without --sbx-chart", sbxRun, func(f *clusterRenderFlags) { f.sbxChart = "" }, []string{"--sbx-chart"}},
		{"gt-sbx without --sbx-values", sbxRun, func(f *clusterRenderFlags) { f.sbxValues = "" }, []string{"--sbx-values"}},
		{"gt-sbx without --out", sbxRun, func(f *clusterRenderFlags) { f.out = "" }, []string{"--out"}},
		{"a control run with --sbx-manifests", controlRun, func(f *clusterRenderFlags) { f.sbxManifests = "m" }, []string{"--sbx-manifests"}},
		{"a control run with --sbx-chart", controlRun, func(f *clusterRenderFlags) { f.sbxChart = "c" }, []string{"--sbx-chart"}},
		{"a control run with --sbx-values", controlRun, func(f *clusterRenderFlags) { f.sbxValues = "v" }, []string{"--sbx-values"}},
		{"an empty cluster with --sbx-values", clusterRenderFlags{}, func(f *clusterRenderFlags) { f.sbxValues = "v" }, []string{"--sbx-values"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := filepath.Join(t.TempDir(), "out")
			f := tc.base
			f.out = out
			tc.edit(&f)
			d := clusterRenderDeps{
				findTown: func() (string, error) {
					t.Error("the render looked up the town before it refused the run")
					return "", errors.New("planted town")
				},
				renderSbx: func(context.Context, sbx.Inputs) (sbx.Set, error) {
					t.Error("the engine ran before it refused the run")
					return sbx.Set{}, errors.New("planted render")
				},
				getenv:   func(string) string { return "" },
				userHome: func() (string, error) { return "", errors.New("planted home") },
				out:      io.Discard,
			}
			err := runClusterRender(d, f)
			if err == nil {
				t.Fatalf("runClusterRender() = nil, want a refusal naming %q", tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if names := dirNames(t, out); names != nil {
				t.Errorf("files in %s = %q, want none", out, names)
			}
		})
	}

	// An empty cluster is the control cluster: it writes the control set, byte for byte.
	town := clusterRenderTestTown(t)
	control := clusterRenderFixture(t)
	control.cluster = clusterControl
	control.out = filepath.Join(t.TempDir(), "control")
	empty := control
	empty.cluster = ""
	empty.out = filepath.Join(t.TempDir(), "empty")
	for _, f := range []clusterRenderFlags{control, empty} {
		if err := runClusterRender(clusterRenderDepsAt(t, town, io.Discard), f); err != nil {
			t.Fatalf("runClusterRender(--cluster %q): %v", f.cluster, err)
		}
	}
	names := dirNames(t, control.out)
	if len(names) == 0 {
		t.Fatal("the control render wrote no file, so there is nothing to compare")
	}
	if got := dirNames(t, empty.out); !slices.Equal(got, names) {
		t.Fatalf("an empty cluster wrote %q, want the control set %q", got, names)
	}
	for _, name := range names {
		want, err := os.ReadFile(filepath.Join(control.out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(empty.out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s from an empty cluster differs from the control render", name)
		}
	}
}

// TestClusterRenderSbxClusterRefusesRecordFlags: --record-flags with --cluster
// gt-sbx is refused by name before the town, the engine or any write, so a gt-sbx
// run never reads as recorded when nothing was written.
func TestClusterRenderSbxClusterRefusesRecordFlags(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	d := clusterRenderDeps{
		findTown: func() (string, error) {
			t.Error("the gt-sbx render looked up the town before it refused --record-flags")
			return "", errors.New("planted town")
		},
		renderSbx: func(context.Context, sbx.Inputs) (sbx.Set, error) {
			t.Error("the engine ran before it refused --record-flags")
			return sbx.Set{}, errors.New("planted render")
		},
		writeFile: func(string, []byte) error {
			t.Error("a gt-sbx run with --record-flags wrote a manifest")
			return errors.New("planted write")
		},
		saveRecord: func(string, clusterRenderFlags) error {
			t.Error("a gt-sbx run with --record-flags wrote the recorded flags")
			return errors.New("planted save")
		},
		record: clusterRenderRecordOptions{record: true},
		out:    io.Discard,
	}
	f := clusterRenderFlags{cluster: clusterSbx, out: out, sbxManifests: "m", sbxChart: "c", sbxValues: "v"}
	err := runClusterRender(d, f)
	if err == nil {
		t.Fatalf("runClusterRender(--cluster %s --record-flags) = nil, want a refusal naming --record-flags", clusterSbx)
	}
	if !strings.Contains(err.Error(), "--record-flags") {
		t.Errorf("error %q does not name --record-flags", err)
	}
	if names := dirNames(t, out); names != nil {
		t.Errorf("files in %s = %q, want none", out, names)
	}
}

// TestTownClusterRenderControlClusterMatchesDefault: --cluster control writes the
// same bytes as a render that names no cluster, and never reaches the gt-sbx engine.
func TestTownClusterRenderControlClusterMatchesDefault(t *testing.T) {
	t.Parallel()
	town := clusterRenderTestTown(t)
	control := clusterRenderFixture(t)
	control.cluster = clusterControl
	def := control
	def.cluster = ""
	def.out = filepath.Join(t.TempDir(), "default")
	for _, f := range []clusterRenderFlags{control, def} {
		d := clusterRenderDepsAt(t, town, io.Discard)
		d.renderSbx = func(context.Context, sbx.Inputs) (sbx.Set, error) {
			t.Error("the control render reached the gt-sbx engine")
			return sbx.Set{}, errors.New("planted render")
		}
		if err := runClusterRender(d, f); err != nil {
			t.Fatalf("runClusterRender(--cluster %q): %v", f.cluster, err)
		}
	}
	names := dirNames(t, control.out)
	if len(names) == 0 {
		t.Fatal("the control render wrote no file, so there is nothing to compare")
	}
	if got := dirNames(t, def.out); !slices.Equal(got, names) {
		t.Fatalf("a render with no cluster wrote %q, want the control set %q", got, names)
	}
	for _, name := range names {
		want, err := os.ReadFile(filepath.Join(control.out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(def.out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s from a render with no cluster differs from --cluster %s", name, clusterControl)
		}
	}
}
