package cmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const rigAddInitArgs = "init --prefix gt --database gastown --server --force --skip-agents"

// fakeRigAddWorkspace returns a rigAddWorkspace whose seams record each call in
// order, so a test asserts what ran and in what order without a bd.real or a
// gateway. The temp directory is the test's own, so nothing touches the host.
func fakeRigAddWorkspace(t *testing.T, gatewayURL string) (rigAddWorkspace, *[]string, string) {
	t.Helper()
	dir := t.TempDir()
	var events []string
	return rigAddWorkspace{
		gatewayURL: gatewayURL,
		push: func(_ context.Context, townRoot string) (string, error) {
			events = append(events, "push "+townRoot)
			return "manifest-sha", nil
		},
		bdReal: func(workDir string, argv []string) error {
			events = append(events, "init in "+workDir+": "+strings.Join(argv, " "))
			return nil
		},
		tempDir: func() (string, error) {
			events = append(events, "tempdir")
			return dir, nil
		},
		removeDir: func(path string) error {
			events = append(events, "remove "+path)
			return nil
		},
	}, &events, dir
}

func TestRigAddWorkspaceInitThenPush(t *testing.T) {
	w, events, dir := fakeRigAddWorkspace(t, "http://gateway.test:7440")

	if err := w.run(context.Background(), "/town", "gastown", "gt"); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	want := []string{
		"tempdir",
		"init in " + dir + ": " + rigAddInitArgs,
		"push /town",
		"remove " + dir,
	}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %q\nwant %q", *events, want)
	}
}

func TestRigAddWorkspaceOff(t *testing.T) {
	w, events, _ := fakeRigAddWorkspace(t, "")

	if err := w.run(context.Background(), "/town", "gastown", "gt"); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if len(*events) != 0 {
		t.Errorf("with no gateway, run made calls %q; want none (no temp workspace, no bd.real, no push)", *events)
	}
}

func TestRigAddWorkspacePushFails(t *testing.T) {
	cause := errors.New("gateway answered 503")
	w, events, dir := fakeRigAddWorkspace(t, "http://gateway.test:7440")
	w.push = func(_ context.Context, _ string) (string, error) {
		*events = append(*events, "push")
		return "", cause
	}

	err := w.run(context.Background(), "/town", "gastown", "gt")
	if err == nil {
		t.Fatal("run() error = nil, want the push failure reported")
	}
	if !errors.Is(err, cause) {
		t.Errorf("run() error = %v, want it to wrap the push cause", err)
	}
	for _, want := range []string{"rig gastown", "pushing the workspace to the bd gateway"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("run() error = %q, want it to name %q", err, want)
		}
	}
	want := []string{"tempdir", "init in " + dir + ": " + rigAddInitArgs, "push", "remove " + dir}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %q\nwant %q: the temp workspace is removed even after a failed push", *events, want)
	}
}

func TestRigAddWorkspaceRefusals(t *testing.T) {
	cause := errors.New("cause")
	tests := []struct {
		name       string
		breakSeam  func(w *rigAddWorkspace)
		wantErr    string
		wantEvents func(dir string) []string
	}{
		{
			name: "temp directory cannot be made",
			breakSeam: func(w *rigAddWorkspace) {
				w.tempDir = func() (string, error) { return "", cause }
			},
			wantErr:    "making the temporary workspace",
			wantEvents: func(string) []string { return nil },
		},
		{
			name: "bd.real init fails, so nothing is pushed",
			breakSeam: func(w *rigAddWorkspace) {
				w.bdReal = func(string, []string) error { return cause }
			},
			wantErr: "bd.real init",
			wantEvents: func(dir string) []string {
				return []string{"tempdir", "remove " + dir}
			},
		},
		{
			name: "temp directory cannot be removed after a good push",
			breakSeam: func(w *rigAddWorkspace) {
				w.removeDir = func(string) error { return cause }
			},
			wantErr: "removing the temporary workspace",
			wantEvents: func(dir string) []string {
				return []string{"tempdir", "init in " + dir + ": " + rigAddInitArgs, "push /town"}
			},
		},
		{
			name: "no pusher wired while a gateway is set",
			breakSeam: func(w *rigAddWorkspace) {
				w.push = nil
			},
			wantErr:    "pusher",
			wantEvents: func(string) []string { return nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, events, dir := fakeRigAddWorkspace(t, "http://gateway.test:7440")
			tt.breakSeam(&w)

			err := w.run(context.Background(), "/town", "gastown", "gt")
			if err == nil {
				t.Fatalf("run() error = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("run() error = %q, want it to contain %q", err, tt.wantErr)
			}
			if want := tt.wantEvents(dir); !reflect.DeepEqual(*events, want) {
				t.Errorf("events = %q\nwant %q", *events, want)
			}
		})
	}
}
