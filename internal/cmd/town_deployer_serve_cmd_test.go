package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// TestTownDeployerServeCommand: serve is registered under gt town deployer, takes no
// arguments, marks four flags required, and hands the run their values (gt-p1ej8c).
func TestTownDeployerServeCommand(t *testing.T) { //testpolicy:allow parallel — it drives the process-wide serve command and the serveSteps seam init registers once
	found, _, err := townDeployerCmd.Find([]string{"serve"})
	if err != nil || found != townDeployerServeCmd || found.Use != "serve" {
		t.Fatalf("gt town deployer Find(serve) = %v, %v; want the serve command", found, err)
	}
	if err := found.Args(found, []string{"extra"}); err == nil {
		t.Error("serve accepted an argument, want a command that takes none")
	}
	for _, name := range []string{"as-user", "inbox", "answers", "state"} {
		f := found.Flags().Lookup(name)
		if f == nil || !slices.Equal(f.Annotations[cobra.BashCompOneRequiredFlag], []string{"true"}) {
			t.Errorf("--%s = %v, want a flag marked required", name, f)
		}
	}
	// The run is handed the flags' values: the request answers and --state/done takes it.
	inbox, answers, state := newServeTestDirs(t)
	writeServeRequest(t, inbox, serveOlderID)
	steps := &serveFakeSteps{}
	serveSteps = steps //testpolicy:allow no-global-swap — serveSteps is the production steps seam gt-1afrbd fills, and the run reads the steps only from it
	var out bytes.Buffer
	found.SetOut(&out)
	t.Cleanup(func() {
		serveSteps = nil //testpolicy:allow no-global-swap — restores the seam set above
		found.SetOut(nil)
		found.Flags().VisitAll(func(f *pflag.Flag) { _ = f.Value.Set(f.DefValue) })
	})
	if err := found.Flags().Parse([]string{"--once", "--as-user", "deployer", "--inbox", inbox, "--answers", answers, "--state", state}); err != nil {
		t.Fatalf("parsing the flags: %v", err)
	}
	if err := found.RunE(found, nil); err != nil {
		t.Fatalf("gt town deployer serve = %v", err)
	}
	if steps.calls != 1 {
		t.Errorf("the steps planned %d times, want 1: --once reached the run", steps.calls)
	}
	if got := serveNames(t, filepath.Join(state, "done")); !slices.Equal(got, []string{serveOlderID + ".json"}) {
		t.Errorf("done holds %v, want the request: --state reached the run", got)
	}
	if !strings.Contains(out.String(), "id: "+serveOlderID) {
		t.Errorf("output = %q, want the answered id on the command's output", out.String())
	}
}

// TestTownDeployerServeCommandFlagErrors: a flag read that fails is an error naming that flag.
func TestTownDeployerServeCommandFlagErrors(t *testing.T) {
	t.Parallel()
	names := []string{"once", "inbox", "answers", "state"}
	for i, missing := range names {
		t.Run("reading --"+missing, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, name := range names[:i] {
				if name == "once" {
					cmd.Flags().Bool(name, false, "")
				} else {
					cmd.Flags().String(name, "", "")
				}
			}
			err := runTownDeployerServe(cmd, nil)
			if want := "reading --" + missing; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("runTownDeployerServe = %v, want a refusal naming %q", err, want)
			}
		})
	}
}

// serveReadAnswer decodes the answer published for id.
func serveReadAnswer(t *testing.T, answers, id string) deployerinbox.Answer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(answers, id+".json"))
	if err != nil {
		t.Fatalf("reading the answer for %s: %v", id, err)
	}
	var answer deployerinbox.Answer
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatalf("the answer for %s does not parse: %v\n%s", id, err, data)
	}
	return answer
}

// TestTownDeployerServeRefusesInvalid: an invalid oldest request is answered refused and moved into
// <state>/rejected with no step run, and an inbox that cannot be read is an error, never an empty run.
// A refusal that cannot finish leaves the request in the inbox, so it is never lost.
func TestTownDeployerServeRefusesInvalid(t *testing.T) {
	t.Parallel()

	invalid := []struct {
		name    string
		content string
	}{
		{"not JSON", "not json"},
		{"an unknown key", `{"id":"` + serveOlderID + `","kind":"plan","plan":"` + servePlan + `","extra":"x"}`},
		{"a kind it does not handle", `{"id":"` + serveOlderID + `","kind":"destroy","plan":"` + servePlan + `"}`},
	}
	for _, tc := range invalid {
		t.Run("moves "+tc.name+" to rejected with a refused answer and runs no step", func(t *testing.T) {
			t.Parallel()
			inbox, answers, state := newServeTestDirs(t)
			rejected := filepath.Join(state, "rejected")
			if err := os.MkdirAll(rejected, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(inbox, serveOlderID+".json"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			steps := &serveFakeSteps{}
			env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: steps, now: serveClock, out: &bytes.Buffer{}}

			err := runTownDeployerServeIn(context.Background(), env)

			if err == nil || !strings.Contains(err.Error(), "is not valid, so it is refused") {
				t.Fatalf("runTownDeployerServeIn = %v, want a refusal of the invalid request", err)
			}
			if steps.calls != 0 {
				t.Errorf("the steps planned %d times for an invalid request, want 0", steps.calls)
			}
			if got := serveNames(t, inbox); len(got) != 0 {
				t.Errorf("the inbox holds %v, want the invalid request moved out", got)
			}
			if got := serveNames(t, rejected); !slices.Equal(got, []string{serveOlderID + ".json"}) {
				t.Errorf("rejected holds %v, want the invalid request", got)
			}
			if got := serveNames(t, answers); !slices.Equal(got, []string{serveOlderID + ".json"}) {
				t.Errorf("answers holds %v, want exactly the refusal", got)
			}
			answer := serveReadAnswer(t, answers, serveOlderID)
			if answer.ID != serveOlderID || answer.Status != deployerinbox.StatusRefused || answer.PlanSHA256 != "" {
				t.Errorf("the answer is %q %q with plan %q, want %q refused with no plan", answer.ID, answer.Status, answer.PlanSHA256, serveOlderID)
			}
			if answer.Detail != detailInvalidRequest {
				t.Errorf("the answer detail is %q, want the fixed %q", answer.Detail, detailInvalidRequest)
			}
			if !answer.At.Equal(serveClock()) {
				t.Errorf("the answer is stamped %s, want the injected clock's %s", answer.At, serveClock())
			}
		})
	}

	t.Run("an invalid request whose answer is already published is moved to rejected and not answered again", func(t *testing.T) {
		t.Parallel()
		inbox, answers, state := newServeTestDirs(t)
		rejected := filepath.Join(state, "rejected")
		if err := os.MkdirAll(rejected, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inbox, serveOlderID+".json"), []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		answerPath := writeServeAnswer(t, answers, serveOlderID)
		before, err := os.ReadFile(answerPath)
		if err != nil {
			t.Fatal(err)
		}
		steps := &serveFakeSteps{}
		env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: steps, now: serveClock, out: &bytes.Buffer{}}

		if err := runTownDeployerServeIn(context.Background(), env); err == nil || !strings.Contains(err.Error(), "is not valid") {
			t.Fatalf("runTownDeployerServeIn = %v, want a refusal of the invalid request", err)
		}
		if got := serveNames(t, rejected); !slices.Equal(got, []string{serveOlderID + ".json"}) {
			t.Errorf("rejected holds %v, want the invalid request", got)
		}
		if got := serveNames(t, inbox); len(got) != 0 {
			t.Errorf("the inbox holds %v, want it empty", got)
		}
		after, err := os.ReadFile(answerPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Errorf("the earlier answer changed: %v, %q to %q", err, before, after)
		}
	})

	// Each row is a refusal that happens before the request leaves the inbox or a step runs.
	refusals := []struct {
		name    string
		prepare func(t *testing.T, env *townDeployerServeEnv)
		wantErr string
	}{
		{"rejected is missing", func(t *testing.T, env *townDeployerServeEnv) {
			if err := os.Remove(filepath.Join(env.state, "rejected")); err != nil {
				t.Fatal(err)
			}
		}, "to move an invalid request into"},
		{"rejected is a file", func(t *testing.T, env *townDeployerServeEnv) {
			rejected := filepath.Join(env.state, "rejected")
			if err := os.Remove(rejected); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rejected, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "to be a directory to move an invalid request into"},
		{"the answer cannot be written", func(t *testing.T, env *townDeployerServeEnv) {
			env.answers = filepath.Join(filepath.Dir(env.answers), "no-such-answers")
		}, "writing the refusal of the invalid request"},
		{"the answer's existence cannot be checked", func(t *testing.T, env *townDeployerServeEnv) {
			// A path under a regular file: Stat answers ENOTDIR, which is not "no answer yet".
			blocker := filepath.Join(filepath.Dir(env.answers), "answers-file")
			if err := os.WriteFile(blocker, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			env.answers = filepath.Join(blocker, "answers")
		}, "already has an answer"},
		{"the request cannot be moved", func(t *testing.T, env *townDeployerServeEnv) {
			// The name it moves to is a non-empty directory already.
			target := filepath.Join(env.state, "rejected", serveOlderID+".json")
			if err := os.MkdirAll(filepath.Join(target, "x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "moving the invalid request"},
	}
	for _, tc := range refusals {
		t.Run("refuses when "+tc.name+" and leaves the request in the inbox", func(t *testing.T) {
			t.Parallel()
			inbox, answers, state := newServeTestDirs(t)
			if err := os.MkdirAll(filepath.Join(state, "rejected"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(inbox, serveOlderID+".json"), []byte("not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			steps := &serveFakeSteps{}
			env := townDeployerServeEnv{once: true, inbox: inbox, answers: answers, state: state, steps: steps, now: serveClock, out: &bytes.Buffer{}}
			tc.prepare(t, &env)

			err := runTownDeployerServeIn(context.Background(), env)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runTownDeployerServeIn = %v, want an error containing %q", err, tc.wantErr)
			}
			if steps.calls != 0 {
				t.Errorf("the steps planned %d times on a refused run, want 0", steps.calls)
			}
			if got := serveNames(t, inbox); !slices.Equal(got, []string{serveOlderID + ".json"}) {
				t.Errorf("the inbox holds %v, want the invalid request left in place", got)
			}
		})
	}

	t.Run("an inbox that cannot be read is an error, not an empty run", func(t *testing.T) {
		t.Parallel()
		_, answers, state := newServeTestDirs(t)
		if err := os.MkdirAll(filepath.Join(state, "rejected"), 0o755); err != nil {
			t.Fatal(err)
		}
		steps := &serveFakeSteps{}
		var out bytes.Buffer
		env := townDeployerServeEnv{once: true, inbox: filepath.Join(state, "no-such-inbox"), answers: answers, state: state, steps: steps, now: serveClock, out: &out}

		err := runTownDeployerServeIn(context.Background(), env)

		if err == nil || !strings.Contains(err.Error(), "reading the inbox") {
			t.Fatalf("runTownDeployerServeIn = %v, want an error naming the unreadable inbox", err)
		}
		if out.Len() != 0 {
			t.Errorf("the command printed %q, want nothing: an unreadable inbox is not a run with no request", out.String())
		}
		if steps.calls != 0 || len(serveNames(t, answers)) != 0 {
			t.Errorf("the steps planned %d times and answers %v, want nothing", steps.calls, serveNames(t, answers))
		}
	})
}
