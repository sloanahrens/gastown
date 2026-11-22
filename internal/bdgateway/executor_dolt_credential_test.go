package bdgateway

import (
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDoltCredential writes a credential file with body and returns its path.
func writeDoltCredential(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dolt.cred")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the credential file: %v", err)
	}
	return path
}

// newDoltCredentialExecutor builds the standard test executor with the Dolt
// credential file named. It lists the options itself rather than reusing
// newTestExecutor's, so the option under test is the one set here.
func newDoltCredentialExecutor(t *testing.T, root string, runner processRunner, file string) *Executor {
	t.Helper()
	if r, ok := runner.(*fakeRunner); ok && r.bound == nil {
		r.bound = waitCtx(t)
	}
	e, err := New(Options{
		Binary:             "/opt/bd/bd",
		WorkspaceRoot:      root,
		Home:               "/home/bdgw",
		Path:               "/usr/local/bin:/usr/bin:/bin",
		DoltHost:           "gt-dolt",
		DoltPort:           "3307",
		TrackerURL:         "http://127.0.0.1:7433",
		TrackerTokenFile:   "/run/secrets/tracker.token",
		DoltCredentialFile: file,
		runner:             runner,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestExecutorDoltCredential: a credential file puts the Dolt user and password
// in bd's environment, one variable for each value the file sets, and neither
// value in its argv.
func TestExecutorDoltCredential(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		want   map[string]string
		absent []string
	}{
		{
			name: "user and password",
			body: "user=gateway\npassword=s3cret\n",
			want: map[string]string{"BEADS_DOLT_SERVER_USER": "gateway", "BEADS_DOLT_PASSWORD": "s3cret"},
		},
		{
			name:   "user only",
			body:   "user=gateway\n",
			want:   map[string]string{"BEADS_DOLT_SERVER_USER": "gateway"},
			absent: []string{"BEADS_DOLT_PASSWORD"},
		},
		{
			name:   "password only",
			body:   "password=s3cret\n",
			want:   map[string]string{"BEADS_DOLT_PASSWORD": "s3cret"},
			absent: []string{"BEADS_DOLT_SERVER_USER"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{}
			e := newDoltCredentialExecutor(t, testWorkspace(t), runner, writeDoltCredential(t, tc.body))
			if err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown"}, nil, io.Discard); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			call := runner.only(t)
			env := envMap(call.env)
			for name, want := range tc.want {
				if got := env[name]; got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			for _, name := range tc.absent {
				if _, ok := env[name]; ok {
					t.Errorf("the environment holds %s, want it absent when the file sets no value for it", name)
				}
			}
			for _, value := range tc.want {
				for _, arg := range call.argv {
					if strings.Contains(arg, value) {
						t.Errorf("argv holds %q, which reaches bd only through its environment", value)
					}
				}
			}
		})
	}
}

// TestExecutorDoltCredentialReadEveryCall: the file is read on every call, so a
// credential rewritten between two calls reaches the second one without a new
// executor.
func TestExecutorDoltCredentialReadEveryCall(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	path := writeDoltCredential(t, "user=gateway\npassword=first\n")
	e := newDoltCredentialExecutor(t, testWorkspace(t), runner, path)
	for i, password := range []string{"first", "second"} {
		if err := os.WriteFile(path, []byte("user=gateway\npassword="+password+"\n"), 0o600); err != nil {
			t.Fatalf("writing the credential file: %v", err)
		}
		if err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, io.Discard); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if got := envMap(runner.records()[i].env)["BEADS_DOLT_PASSWORD"]; got != password {
			t.Errorf("call %d carried BEADS_DOLT_PASSWORD = %q, want the file's %q", i+1, got, password)
		}
	}
}

// TestExecutorDoltCredentialAbsent: with no credential file the environment is
// exactly the one the gateway sent before the file existed.
func TestExecutorDoltCredentialAbsent(t *testing.T) {
	t.Parallel()
	root := testWorkspace(t)
	runner := &fakeRunner{}
	e := newDoltCredentialExecutor(t, root, runner, "")
	if err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, io.Discard); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := map[string]string{
		"HOME":                   "/home/bdgw",
		"PATH":                   "/usr/local/bin:/usr/bin:/bin",
		"BEADS_DIR":              filepath.Join(root, "gastown", ".beads"),
		"BEADS_DOLT_SERVER_HOST": "gt-dolt",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"TRACKER_URL":            "http://127.0.0.1:7433",
		"TRACKER_TOKEN_FILE":     "/run/secrets/tracker.token",
		"GT_TOWN_ROOT":           root,
	}
	if got := envMap(runner.only(t).env); !maps.Equal(got, want) {
		t.Errorf("the environment with no credential file = %v, want %v", got, want)
	}
}
