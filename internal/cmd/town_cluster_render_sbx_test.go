package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
