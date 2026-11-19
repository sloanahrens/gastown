package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// The fixed seams a request test runs with: a UTC second and four random
// bytes, so the id is the same on every run.
const (
	requestTestID   = "20261119T181450Z-0123abcd"
	requestTestPlan = "2026-11-19-first"
)

func requestTestClock() time.Time {
	return time.Date(2026, 11, 19, 18, 14, 50, 0, time.UTC)
}

// requestTestRandom reads the bytes that spell the id's random half.
func requestTestRandom() io.Reader {
	return bytes.NewReader([]byte{0x01, 0x23, 0xab, 0xcd})
}

// newRequestTestEnv is the command a test runs: a fixed clock and random
// source, outside a pod, writing into inbox through the real filesystem.
func newRequestTestEnv(inbox string, out io.Writer) townDeployerRequestEnv {
	return townDeployerRequestEnv{
		kind:    "plan",
		plan:    requestTestPlan,
		inbox:   inbox,
		getenv:  func(string) string { return "" },
		now:     requestTestClock,
		random:  requestTestRandom(),
		marshal: json.Marshal,
		create:  createRequestFile,
		chmod:   os.Chmod,
		remove:  os.Remove,
		out:     out,
	}
}

// newRequestTestInbox makes the inbox directory under the test's own
// temporary directory and returns both paths.
func newRequestTestInbox(t *testing.T) (root, inbox string) {
	t.Helper()
	root = t.TempDir()
	inbox = filepath.Join(root, "inbox")
	if err := os.Mkdir(inbox, 0o755); err != nil {
		t.Fatalf("making the inbox %s: %v", inbox, err)
	}
	return root, inbox
}

// inboxNames lists the files the inbox holds.
func inboxNames(t *testing.T, inbox string) []string {
	t.Helper()
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatalf("reading the inbox %s: %v", inbox, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestTownDeployerRequestWrites(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"plan", "apply", "rollback"} {
		t.Run("writes the "+kind+" request", func(t *testing.T) {
			t.Parallel()
			root, inbox := newRequestTestInbox(t)
			var out bytes.Buffer
			env := newRequestTestEnv(inbox, &out)
			env.kind = kind
			// The process umask cuts OpenFile's create mode, so the mode the
			// command chmods to is recorded here: this assertion is what fails
			// when the chmod call goes.
			var modes []fs.FileMode
			env.chmod = func(name string, mode fs.FileMode) error {
				modes = append(modes, mode)
				return os.Chmod(name, mode)
			}

			if err := runTownDeployerRequestIn(env); err != nil {
				t.Fatalf("runTownDeployerRequestIn = %v", err)
			}
			if len(modes) != 1 || modes[0] != 0o644 {
				t.Errorf("the request was chmodded %v, want one call with 0644", modes)
			}

			if names := inboxNames(t, inbox); len(names) != 1 || names[0] != requestTestID+".json" {
				t.Fatalf("the inbox holds %v, want the one file %s.json", names, requestTestID)
			}
			path := filepath.Join(inbox, requestTestID+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the request %s: %v", path, err)
			}
			got, err := deployerinbox.ParseRequest(requestTestID+".json", data)
			if err != nil {
				t.Fatalf("the written request does not parse: %v\n%s", err, data)
			}
			want := deployerinbox.Request{ID: requestTestID, Kind: kind, Plan: requestTestPlan}
			if got != want {
				t.Errorf("the written request is %+v, want %+v", got, want)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stating the request %s: %v", path, err)
			}
			if got := info.Mode().Perm(); got != 0o644 {
				t.Errorf("the request holds mode %04o, want 0644", got)
			}

			wantOut := "id: " + requestTestID + "\nanswer: " + filepath.Join(root, "answers", requestTestID+".json") + "\n"
			if out.String() != wantOut {
				t.Errorf("the command printed %q, want %q", out.String(), wantOut)
			}
		})
	}
}

// failingRequestFile is a real request file whose write or close fails after
// the file exists, so a test drives the failure and the removal that follows.
type failingRequestFile struct {
	*os.File
	writeErr error
	closeErr error
}

func (f *failingRequestFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(p)
}

func (f *failingRequestFile) Close() error {
	closeErr := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return closeErr
}

// createThenFail makes the real request file and hands back f wrapped around
// it, so the failure happens against a file that exists.
func createThenFail(f func(*os.File) *failingRequestFile) func(string, fs.FileMode) (io.WriteCloser, error) {
	return func(name string, perm fs.FileMode) (io.WriteCloser, error) {
		file, err := createRequestFile(name, perm)
		if err != nil {
			return nil, err
		}
		return f(file.(*os.File)), nil
	}
}

func TestTownDeployerRequestRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// edit adjusts the command before the run; inbox is created and empty
		// unless existing is set.
		edit func(env *townDeployerRequestEnv, inbox string)
		// existing is written at inbox/<requestTestID>.json first.
		existing string
		want     string
		// left is what the inbox holds after the run.
		left []string
	}{
		{
			name: "inside a pod",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.getenv = func(string) string { return "10.0.0.1" }
			},
			want: "refuses inside a pod",
		},
		{
			name: "no --inbox",
			edit: func(env *townDeployerRequestEnv, _ string) { env.inbox = "" },
			want: "needs --inbox",
		},
		{
			name: "the random digits cannot be read",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.random = strings.NewReader("ab")
			},
			want: "random digits",
		},
		{
			name: "the request cannot be encoded",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal refused") }
			},
			want: "encoding the plan request",
		},
		{
			name: "a kind the inbox does not take",
			edit: func(env *townDeployerRequestEnv, _ string) { env.kind = "delete" },
			want: "not plan, apply or rollback",
		},
		{
			name: "a plan name that is a path",
			edit: func(env *townDeployerRequestEnv, _ string) { env.plan = "../control" },
			want: "not a plan directory name",
		},
		{
			name: "no plan name",
			edit: func(env *townDeployerRequestEnv, _ string) { env.plan = "" },
			want: "not a plan directory name",
		},
		{
			name:     "an existing request",
			existing: "original",
			want:     "writing the request",
			left:     []string{requestTestID + ".json"},
		},
		{
			name: "the request cannot be created",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.create = func(string, fs.FileMode) (io.WriteCloser, error) {
					return nil, errors.New("permission denied")
				}
			},
			want: "writing the request",
		},
		{
			name: "the mode cannot be set",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.chmod = func(string, fs.FileMode) error { return errors.New("chmod refused") }
			},
			want: "setting the mode of the request",
		},
		{
			name: "the mode cannot be set and the file cannot be closed",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.chmod = func(string, fs.FileMode) error { return errors.New("chmod refused") }
				env.create = createThenFail(func(f *os.File) *failingRequestFile {
					return &failingRequestFile{File: f, closeErr: errors.New("close refused")}
				})
			},
			want: "close refused",
		},
		{
			name: "the request cannot be written",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.create = createThenFail(func(f *os.File) *failingRequestFile {
					return &failingRequestFile{File: f, writeErr: errors.New("write refused")}
				})
			},
			want: "write refused",
		},
		{
			name: "the request cannot be written and the file cannot be closed",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.create = createThenFail(func(f *os.File) *failingRequestFile {
					return &failingRequestFile{File: f, writeErr: errors.New("write refused"), closeErr: errors.New("close refused")}
				})
			},
			want: "close refused",
		},
		{
			name: "the request cannot be closed",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.create = createThenFail(func(f *os.File) *failingRequestFile {
					return &failingRequestFile{File: f, closeErr: errors.New("close refused")}
				})
			},
			want: "closing the request",
		},
		{
			name: "the incomplete request cannot be removed",
			edit: func(env *townDeployerRequestEnv, _ string) {
				env.create = createThenFail(func(f *os.File) *failingRequestFile {
					return &failingRequestFile{File: f, writeErr: errors.New("write refused")}
				})
				env.remove = func(string) error { return errors.New("remove refused") }
			},
			want: "removing the incomplete request",
			left: []string{requestTestID + ".json"},
		},
		{
			name: "the id and answers path cannot be printed",
			edit: func(env *townDeployerRequestEnv, _ string) { env.out = failingDeployerWriter{} },
			want: "printing its id and answers path",
			left: []string{requestTestID + ".json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, inbox := newRequestTestInbox(t)
			env := newRequestTestEnv(inbox, &bytes.Buffer{})
			if tc.existing != "" {
				path := filepath.Join(inbox, requestTestID+".json")
				if err := os.WriteFile(path, []byte(tc.existing), 0o644); err != nil {
					t.Fatalf("writing the existing request %s: %v", path, err)
				}
			}
			if tc.edit != nil {
				tc.edit(&env, inbox)
			}

			err := runTownDeployerRequestIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownDeployerRequestIn = %v, want a refusal naming %q", err, tc.want)
			}

			names := inboxNames(t, inbox)
			if strings.Join(names, ",") != strings.Join(tc.left, ",") {
				t.Fatalf("the inbox holds %v after the refusal, want %v", names, tc.left)
			}
			if tc.existing != "" {
				data, err := os.ReadFile(filepath.Join(inbox, requestTestID+".json"))
				if err != nil {
					t.Fatalf("reading the existing request: %v", err)
				}
				if string(data) != tc.existing {
					t.Errorf("the existing request holds %q, want its original %q", data, tc.existing)
				}
			}
		})
	}
}

// TestTownDeployerRequestFlagReadsRefuseAnUndefinedFlag keeps every flag read
// in runTownDeployerRequest covered, so a flag that stops being registered
// fails here.
func TestTownDeployerRequestFlagReadsRefuseAnUndefinedFlag(t *testing.T) {
	t.Parallel()
	flags := []string{"kind", "plan", "inbox"}
	for i := range flags {
		defined := flags[:i]
		want := "reading --" + flags[i]
		t.Run(want, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, name := range defined {
				cmd.Flags().String(name, "", "")
			}
			err := runTownDeployerRequest(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("runTownDeployerRequest = %v, want a refusal naming %q", err, want)
			}
		})
	}
}

// TestTownDeployerRequestFlagsRegistered proves the command the binary runs
// holds every flag the run function reads.
func TestTownDeployerRequestFlagsRegistered(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"kind", "plan", "inbox"} {
		if townDeployerRequestCmd.Flags().Lookup(name) == nil {
			t.Errorf("gt town deployer request has no --%s flag", name)
		}
	}
	if found := lookupCommand(rootCmd, []string{"town", "deployer", "request"}); found != townDeployerRequestCmd {
		t.Errorf("gt town deployer request resolves to %v, want the request command", found)
	}
}
