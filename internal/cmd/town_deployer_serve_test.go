package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/deployerinbox"
)

const (
	serveOlderID = "20261119T181450Z-0123abcd"
	serveNewerID = "20261119T181451Z-0123abcd"
	servePlan    = "2026-11-19-first"
)

// serveFakeSteps counts the plan steps Handle runs; the plan kind reaches no
// other step, so any other call panics on the nil embedded interface.
type serveFakeSteps struct {
	deployerinbox.Steps
	calls int
}

func (f *serveFakeSteps) Plan(_ context.Context, _ string) (clusterapproval.Plan, error) {
	f.calls++
	return clusterapproval.Plan{Cluster: clusterapproval.ClusterControl, SHA256: strings.Repeat("a1", 32), Phases: []string{"policy"}}, nil
}

func (f *serveFakeSteps) WriteProposal(context.Context, string, []byte, string) error {
	return nil
}

// newServeTestDirs makes the inbox, the answers directory and <state>/done
// under the test's own temporary directory.
func newServeTestDirs(t *testing.T) (inbox, answers, state string) {
	t.Helper()
	root := t.TempDir()
	inbox, answers, state = filepath.Join(root, "inbox"), filepath.Join(root, "answers"), filepath.Join(root, "state")
	for _, dir := range []string{inbox, answers, filepath.Join(state, "done")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("making %s: %v", dir, err)
		}
	}
	return inbox, answers, state
}

func writeServeRequest(t *testing.T, inbox, id string) {
	t.Helper()
	data := `{"id":"` + id + `","kind":"plan","plan":"` + servePlan + `"}`
	if err := os.WriteFile(filepath.Join(inbox, id+".json"), []byte(data), 0o644); err != nil {
		t.Fatalf("writing the request %s: %v", id, err)
	}
}

func serveNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestTownDeployerServeOnce(t *testing.T) {
	t.Parallel()

	t.Run("answers the oldest request, writes its answer and moves it to done", func(t *testing.T) {
		t.Parallel()
		inbox, answers, state := newServeTestDirs(t)
		writeServeRequest(t, inbox, serveOlderID)
		writeServeRequest(t, inbox, serveNewerID)
		steps := &serveFakeSteps{}
		var out bytes.Buffer
		env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: steps, now: serveClock, out: &out}

		if err := runTownDeployerServeIn(context.Background(), env); err != nil {
			t.Fatalf("runTownDeployerServeIn = %v", err)
		}
		if steps.calls != 1 {
			t.Errorf("the steps planned %d times, want 1", steps.calls)
		}
		if got := serveNames(t, inbox); len(got) != 1 || got[0] != serveNewerID+".json" {
			t.Errorf("the inbox holds %v, want only the newer request", got)
		}
		if got := serveNames(t, filepath.Join(state, "done")); len(got) != 1 || got[0] != serveOlderID+".json" {
			t.Errorf("done holds %v, want only the older request", got)
		}
		data, err := os.ReadFile(filepath.Join(answers, serveOlderID+".json"))
		if err != nil {
			t.Fatalf("reading the answer: %v", err)
		}
		var answer deployerinbox.Answer
		if err := json.Unmarshal(data, &answer); err != nil {
			t.Fatalf("the answer does not parse: %v\n%s", err, data)
		}
		if answer.ID != serveOlderID || answer.Status != deployerinbox.StatusProposed {
			t.Errorf("the answer is %q %q, want %q proposed", answer.ID, answer.Status, serveOlderID)
		}
		if !answer.At.Equal(serveClock()) {
			t.Errorf("the answer is stamped %s, want the injected clock's %s", answer.At, serveClock())
		}
		wantOut := "id: " + serveOlderID + "\nstatus: proposed\nanswer: " + filepath.Join(answers, serveOlderID+".json") + "\n"
		if out.String() != wantOut {
			t.Errorf("the command printed %q, want %q", out.String(), wantOut)
		}
	})

	refusals := []struct {
		name    string
		once    bool
		steps   bool
		prepare func(t *testing.T, inbox, state string)
		wantErr string
	}{
		{name: "refuses without --once", once: false, steps: true, wantErr: "needs --once"},
		{name: "refuses with no steps wired", once: true, steps: false, wantErr: "no plan, apply or rollback steps wired"},
		{
			name: "refuses when <state>/done is missing", once: true, steps: true,
			prepare: func(t *testing.T, _, state string) {
				if err := os.Remove(filepath.Join(state, "done")); err != nil {
					t.Fatalf("removing done: %v", err)
				}
			},
			wantErr: "to move a request into",
		},
		{
			name: "refuses when <state>/done is a file", once: true, steps: true,
			prepare: func(t *testing.T, _, state string) {
				done := filepath.Join(state, "done")
				if err := os.Remove(done); err != nil {
					t.Fatalf("removing done: %v", err)
				}
				if err := os.WriteFile(done, nil, 0o644); err != nil {
					t.Fatalf("writing done as a file: %v", err)
				}
			},
			wantErr: "to be a directory",
		},
		{
			name: "refuses an invalid oldest request and leaves it in the inbox", once: true, steps: true,
			prepare: func(t *testing.T, inbox, _ string) {
				if err := os.WriteFile(filepath.Join(inbox, serveOlderID+".json"), []byte("not json"), 0o644); err != nil {
					t.Fatalf("writing the invalid request: %v", err)
				}
			},
			wantErr: "is not valid",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inbox, answers, state := newServeTestDirs(t)
			writeServeRequest(t, inbox, serveOlderID)
			if tc.prepare != nil {
				tc.prepare(t, inbox, state)
			}
			steps := &serveFakeSteps{}
			env := townDeployerServeEnv{once: tc.once, inbox: inbox, answers: answers, state: state, now: serveClock, out: &bytes.Buffer{}}
			if tc.steps {
				env.steps = steps
			}

			err := runTownDeployerServeIn(context.Background(), env)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runTownDeployerServeIn = %v, want an error containing %q", err, tc.wantErr)
			}
			if steps.calls != 0 {
				t.Errorf("the steps planned %d times on a refused run, want 0", steps.calls)
			}
			if got := serveNames(t, answers); len(got) != 0 {
				t.Errorf("a refused run wrote answers %v, want none", got)
			}
		})
	}

	t.Run("skips a dotfile and a directory ahead of a valid request and leaves them alone", func(t *testing.T) {
		t.Parallel()
		inbox, answers, state := newServeTestDirs(t)
		if err := os.WriteFile(filepath.Join(inbox, ".DS_Store"), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A directory that sorts before the request and even carries a request's name.
		if err := os.Mkdir(filepath.Join(inbox, "00000000T000000Z-00000000.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeServeRequest(t, inbox, serveOlderID)
		steps := &serveFakeSteps{}
		env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: steps, now: serveClock, out: &bytes.Buffer{}}

		if err := runTownDeployerServeIn(context.Background(), env); err != nil {
			t.Fatalf("runTownDeployerServeIn = %v, want the valid request answered", err)
		}
		if steps.calls != 1 {
			t.Errorf("the steps planned %d times, want 1", steps.calls)
		}
		if got := serveNames(t, inbox); !slices.Equal(got, []string{".DS_Store", "00000000T000000Z-00000000.json"}) {
			t.Errorf("the inbox holds %v, want the dotfile and the directory untouched", got)
		}
		if got := serveNames(t, filepath.Join(state, "done")); !slices.Equal(got, []string{serveOlderID + ".json"}) {
			t.Errorf("done holds %v, want only the valid request", got)
		}
	})
	t.Run("an inbox of nothing that is a request says so and answers nothing", func(t *testing.T) {
		t.Parallel()
		inbox, answers, state := newServeTestDirs(t)
		if err := os.WriteFile(filepath.Join(inbox, ".DS_Store"), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
		steps := &serveFakeSteps{}
		var out bytes.Buffer
		env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: steps, now: serveClock, out: &out}

		if err := runTownDeployerServeIn(context.Background(), env); err != nil {
			t.Fatalf("runTownDeployerServeIn = %v, want nil for an inbox with no request", err)
		}
		if out.String() != "deployer: no request in the inbox\n" {
			t.Errorf("output = %q, want the no-request line, so a caller tells nothing-to-answer from a silent success", out.String())
		}
		if steps.calls != 0 || len(serveNames(t, answers)) != 0 {
			t.Errorf("the steps planned %d times and answers %v, want nothing", steps.calls, serveNames(t, answers))
		}
	})
	t.Run("answers nothing from an empty inbox", func(t *testing.T) {
		t.Parallel()
		inbox, answers, state := newServeTestDirs(t)
		steps := &serveFakeSteps{}
		env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: steps, now: serveClock, out: &bytes.Buffer{}}

		if err := runTownDeployerServeIn(context.Background(), env); err != nil {
			t.Fatalf("runTownDeployerServeIn = %v, want nil for an empty inbox", err)
		}
		if steps.calls != 0 {
			t.Errorf("the steps planned %d times for an empty inbox, want 0", steps.calls)
		}
	})
}

// serveFailingWriter is an output that cannot be written.
type serveFailingWriter struct{}

func (serveFailingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// TestTownDeployerServeOnceFails: each step of the run that can fail is an error naming its own
// cause: an inbox that cannot be read, a request that cannot be read, an answer that cannot be
// written, a request that cannot be moved, and a status line that cannot be printed (gt-fqsb68).
func TestTownDeployerServeOnceFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, env *townDeployerServeEnv)
		wantErr string
	}{
		{"an inbox that cannot be read", func(t *testing.T, env *townDeployerServeEnv) {
			env.inbox = filepath.Join(filepath.Dir(env.inbox), "no-such-inbox")
		}, "reading the inbox"},
		{"a request that cannot be read", func(t *testing.T, env *townDeployerServeEnv) {
			env.readFile = func(string) ([]byte, error) { return nil, errors.New("disk gone") }
		}, "reading the request"},
		{"an answer that cannot be written", func(t *testing.T, env *townDeployerServeEnv) {
			env.answers = filepath.Join(filepath.Dir(env.answers), "no-such-answers", "deeper")
		}, "writing the answer to the request"},
		{"a request that cannot be moved", func(t *testing.T, env *townDeployerServeEnv) {
			// The name it moves to is a non-empty directory already.
			target := filepath.Join(env.state, "done", serveOlderID+".json")
			if err := os.MkdirAll(filepath.Join(target, "x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "moving the request"},
		{"a no-request line that cannot be printed", func(t *testing.T, env *townDeployerServeEnv) {
			if err := os.Remove(filepath.Join(env.inbox, serveOlderID+".json")); err != nil {
				t.Fatal(err)
			}
			env.out = serveFailingWriter{}
		}, "printing the no-request line"},
		{"a status line that cannot be printed", func(t *testing.T, env *townDeployerServeEnv) {
			env.out = serveFailingWriter{}
		}, "is answered, but printing its status failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inbox, answers, state := newServeTestDirs(t)
			writeServeRequest(t, inbox, serveOlderID)
			env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: &serveFakeSteps{}, now: serveClock, out: &bytes.Buffer{}}
			tc.prepare(t, &env)

			err := runTownDeployerServeIn(context.Background(), env)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("runTownDeployerServeIn = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func serveClock() time.Time {
	return time.Date(2026, 11, 19, 18, 15, 0, 0, time.UTC)
}
