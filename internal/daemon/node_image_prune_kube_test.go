package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitConditionArgv returns the condition a wait argv asks kubectl for, or "".
func waitConditionArgv(args []string) string {
	for _, arg := range args {
		if condition, ok := strings.CutPrefix(arg, "--for=condition="); ok {
			return condition
		}
	}
	return ""
}

// TestNodeImagePruneKubeArgv: ListJSON and Apply run kubectl through the exec seam
// with exactly the argv the node-prune run needs, and no --request-timeout or
// --context flag, since the bound is a context deadline (gt-ye543d). A non-zero
// exit is an error naming the verb, the exit status and the head of kubectl's
// answer (gt-d4zhkq).
func TestNodeImagePruneKubeArgv(t *testing.T) {
	t.Parallel()
	const manifest = "kind: Job\nsecret: none\n"
	const leak = "secret-token"

	t.Run("ListJSON", func(t *testing.T) {
		tests := []struct {
			name    string
			out     string
			exit    int
			runErr  error
			wantOut string
			wantMsg string
		}{
			{name: "listed", out: `{"items":[]}`, wantOut: `{"items":[]}`},
			{name: "non-zero exit", out: leak, exit: 1, wantMsg: "kubectl get of the gt namespace exited 1"},
			{name: "run error", runErr: errors.New("exec: kubectl not found"), wantMsg: "running kubectl get"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var gotArgs []string
				var hasDeadline bool
				run := func(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
					gotArgs = args
					_, hasDeadline = ctx.Deadline()
					return tt.out, tt.exit, tt.runErr
				}
				got, err := kubectlNodePrune{dir: t.TempDir(), run: run}.ListJSON(context.Background())

				assert.Equal(t, []string{"-n", "gt", "get", "job,deploy,pod", "-o", "json"}, gotArgs)
				assert.True(t, hasDeadline, "kubectl ran with no deadline")
				assert.Equal(t, tt.wantOut, string(got))
				if tt.wantMsg == "" {
					assert.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantMsg)
				if tt.exit != 0 {
					assert.Contains(t, err.Error(), tt.out, "the error did not carry the head of kubectl's answer")
				}
			})
		}
	})

	t.Run("Apply", func(t *testing.T) {
		tests := []struct {
			name    string
			out     string
			exit    int
			runErr  error
			wantMsg string
		}{
			{name: "applied", out: "job created"},
			{name: "non-zero exit", out: leak, exit: 1, wantMsg: "kubectl create of the node-prune Job exited 1"},
			{name: "run error", runErr: errors.New("exec: kubectl not found"), wantMsg: "running kubectl create"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				dir := t.TempDir()
				var gotArgs []string
				var gotManifest []byte
				var hasDeadline bool
				run := func(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
					gotArgs = args
					_, hasDeadline = ctx.Deadline()
					if len(args) == 5 {
						gotManifest, _ = os.ReadFile(args[4])
					}
					return tt.out, tt.exit, tt.runErr
				}
				err := kubectlNodePrune{dir: dir, run: run}.Apply(context.Background(), []byte(manifest))

				require.Len(t, gotArgs, 5)
				assert.Equal(t, []string{"-n", "gt", "create", "-f"}, gotArgs[:4])
				assert.Equal(t, filepath.Join(dir, nodePruneManifestName([]byte(manifest))), gotArgs[4], "the manifest was not written under the update directory by its name")
				assert.Equal(t, manifest, string(gotManifest), "kubectl did not read the rendered manifest")
				assert.True(t, hasDeadline, "kubectl ran with no deadline")
				_, statErr := os.Stat(gotArgs[4])
				assert.ErrorIs(t, statErr, os.ErrNotExist, "the manifest was left behind after the apply")
				if tt.wantMsg == "" {
					assert.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantMsg)
				if tt.exit != 0 {
					assert.Contains(t, err.Error(), tt.out, "the error did not carry the head of kubectl's answer")
				}
			})
		}
	})

	t.Run("Apply refuses a manifest path it cannot write", func(t *testing.T) {
		// A directory already holds the manifest's name, so the write fails whoever
		// runs the test, and kubectl must not run.
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, nodePruneManifestName([]byte(manifest))), 0o700))
		run := func(context.Context, string, []string, string, ...string) (string, int, error) {
			t.Error("kubectl ran for a manifest that was not written")
			return "", 0, nil
		}
		err := kubectlNodePrune{dir: dir, run: run}.Apply(context.Background(), []byte(manifest))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "write the node-prune manifest")
	})

	t.Run("Apply refuses an unwritable update directory", func(t *testing.T) {
		// The update directory is a regular file, so the manifest cannot be written
		// under it and kubectl must not run.
		dir := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(dir, []byte("x"), 0o600))
		run := func(context.Context, string, []string, string, ...string) (string, int, error) {
			t.Error("kubectl ran for a manifest that was not written")
			return "", 0, nil
		}
		err := kubectlNodePrune{dir: dir, run: run}.Apply(context.Background(), []byte(manifest))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the update directory")
	})
}

// TestNodeImagePruneKubeApplyNamesTheNamespace: Apply names the gt namespace with
// -n before create and -f, so a manifest that lost its namespace still creates the
// Job in the gt namespace and not in default (gt-rvasli).
func TestNodeImagePruneKubeApplyNamesTheNamespace(t *testing.T) {
	t.Parallel()
	manifest := []byte("kind: Job\n")

	dir := t.TempDir()
	var gotArgs []string
	run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
		gotArgs = args
		return "", 0, nil
	}
	err := kubectlNodePrune{dir: dir, run: run}.Apply(context.Background(), manifest)

	require.NoError(t, err)
	require.Len(t, gotArgs, 5)
	assert.Equal(t, []string{"-n", nodeImagePruneNamespace, "create", "-f"}, gotArgs[:4])
	assert.Equal(t, filepath.Join(dir, nodePruneManifestName(manifest)), gotArgs[4])
}

// TestNodeImagePruneKubeWaitArgv: WaitComplete, Logs and Delete run kubectl through
// the exec seam with exactly the argv the node-prune run needs, and no
// --request-timeout or --context flag (gt-ye543d). Logs returns kubectl's stdout
// untrimmed, because the parser needs the log's final newline.
func TestNodeImagePruneKubeWaitArgv(t *testing.T) {
	t.Parallel()
	const job = "node-prune-abc123"

	tests := []struct {
		name     string
		call     func(k kubectlNodePrune) ([]byte, error)
		out      string
		wantArgs []string
		wantOut  string
	}{
		{
			name:     "WaitComplete",
			call:     func(k kubectlNodePrune) ([]byte, error) { return nil, k.WaitComplete(context.Background(), job) },
			wantArgs: []string{"-n", "gt", "wait", "--for=condition=complete", "job/" + job, "--timeout=960s"},
		},
		{
			name:     "Logs",
			call:     func(k kubectlNodePrune) ([]byte, error) { return k.Logs(context.Background(), job) },
			out:      "pruned 3 images\n",
			wantArgs: []string{"-n", "gt", "logs", "job/" + job},
			wantOut:  "pruned 3 images\n",
		},
		{
			name:     "Logs keeps the bytes untouched",
			call:     func(k kubectlNodePrune) ([]byte, error) { return k.Logs(context.Background(), job) },
			out:      "a\nb\n",
			wantArgs: []string{"-n", "gt", "logs", "job/" + job},
			wantOut:  "a\nb\n",
		},
		{
			name:     "Delete",
			call:     func(k kubectlNodePrune) ([]byte, error) { return nil, k.Delete(context.Background(), job) },
			wantArgs: []string{"-n", "gt", "delete", "job", job, "--ignore-not-found", "--wait=true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgs []string
			run := func(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
				if waitConditionArgv(args) == "failed" {
					// The complete wait is the argv under test; the failed wait
					// ends when WaitComplete cancels it.
					<-ctx.Done()
					return "", -1, ctx.Err()
				}
				gotArgs = args
				return tt.out, 0, nil
			}
			got, err := tt.call(kubectlNodePrune{dir: t.TempDir(), run: run})

			require.NoError(t, err)
			assert.Equal(t, tt.wantArgs, gotArgs)
			assert.Equal(t, tt.wantOut, string(got))
		})
	}
}

// TestNodeImagePruneKubeWaitIsBounded: WaitComplete, Logs and Delete each hand the
// exec seam a context carrying a deadline of its own, so a hung kubectl cannot hold
// the heartbeat. WaitComplete gets the Job's 900s deadline plus kubectl's 960s
// timeout plus a 60s margin; Logs and Delete get 120s each (gt-eh9pvy).
func TestNodeImagePruneKubeWaitIsBounded(t *testing.T) {
	t.Parallel()
	const job = "node-prune-abc123"

	tests := []struct {
		name string
		call func(k kubectlNodePrune) error
		want time.Duration
		// calls is how many kubectl calls the seam takes: WaitComplete runs the
		// complete and failed waits at once.
		calls int
	}{
		{
			name:  "WaitComplete",
			call:  func(k kubectlNodePrune) error { return k.WaitComplete(context.Background(), job) },
			want:  1020 * time.Second,
			calls: 2,
		},
		{
			name: "Logs",
			call: func(k kubectlNodePrune) error {
				_, err := k.Logs(context.Background(), job)
				return err
			},
			want:  120 * time.Second,
			calls: 1,
		},
		{
			name:  "Delete",
			call:  func(k kubectlNodePrune) error { return k.Delete(context.Background(), job) },
			want:  120 * time.Second,
			calls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type call struct {
				limit time.Duration
				ok    bool
			}
			// The fake records its deadline as each wait starts, and WaitComplete
			// returns only after both end, so a buffered channel keeps it race-free.
			calls := make(chan call, tt.calls)
			run := func(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
				deadline, ok := ctx.Deadline()
				calls <- call{limit: time.Until(deadline), ok: ok}
				if waitConditionArgv(args) == "failed" {
					<-ctx.Done()
					return "", -1, ctx.Err()
				}
				return "", 0, nil
			}
			require.NoError(t, tt.call(kubectlNodePrune{dir: t.TempDir(), run: run}))

			for i := 0; i < tt.calls; i++ {
				select {
				case got := <-calls:
					require.True(t, got.ok, "kubectl ran with no deadline")
					// The seam reads the deadline a moment after it is set; a few
					// seconds of slack keeps the assertion about which bound was chosen.
					assert.InDelta(t, tt.want.Seconds(), got.limit.Seconds(), 5, "the deadline is %s, want about %s", got.limit, tt.want)
				default:
					t.Fatalf("kubectl ran %d times, want %d", i, tt.calls)
				}
			}
		})
	}
}

// TestNodeImagePruneKubeWaitDeadlineError: a call that outlives its deadline is an
// error naming the verb and the deadline for WaitComplete, Logs and Delete, whether
// the exec reports context.DeadlineExceeded or, as update.ExecRun does for a process
// it killed, some other error (gt-eh9pvy).
func TestNodeImagePruneKubeWaitDeadlineError(t *testing.T) {
	t.Parallel()
	const job = "node-prune-abc123"

	tests := []struct {
		name     string
		call     func(k kubectlNodePrune, ctx context.Context) error
		wantVerb string
		wantBnd  string
	}{
		{
			name:     "WaitComplete",
			call:     func(k kubectlNodePrune, ctx context.Context) error { return k.WaitComplete(ctx, job) },
			wantVerb: "kubectl wait",
			wantBnd:  "1020s",
		},
		{
			name: "Logs",
			call: func(k kubectlNodePrune, ctx context.Context) error {
				_, err := k.Logs(ctx, job)
				return err
			},
			wantVerb: "kubectl logs",
			wantBnd:  "120s",
		},
		{
			name:     "Delete",
			call:     func(k kubectlNodePrune, ctx context.Context) error { return k.Delete(ctx, job) },
			wantVerb: "kubectl delete",
			wantBnd:  "120s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("exec reports the deadline", func(t *testing.T) {
				run := func(context.Context, string, []string, string, ...string) (string, int, error) {
					return "", -1, context.DeadlineExceeded
				}
				err := tt.call(kubectlNodePrune{dir: t.TempDir(), run: run}, context.Background())

				require.Error(t, err)
				assert.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Contains(t, err.Error(), tt.wantVerb)
				assert.Contains(t, err.Error(), tt.wantBnd)
			})
			t.Run("deadline fires, exec reports a kill", func(t *testing.T) {
				// update.ExecRun reports the process it killed as its own error, not
				// ctx.Err(), so the bound's state is what names the deadline.
				expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				run := func(context.Context, string, []string, string, ...string) (string, int, error) {
					return "", -1, errors.New("signal: killed")
				}
				err := tt.call(kubectlNodePrune{dir: t.TempDir(), run: run}, expired)

				require.Error(t, err)
				assert.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Contains(t, err.Error(), tt.wantVerb)
				assert.Contains(t, err.Error(), tt.wantBnd)
			})
		})
	}
}

// TestNodeImagePruneKubeWaitErrors: a non-zero exit of WaitComplete, Logs or Delete is
// an error naming the verb, the exit status and the head of kubectl's answer
// (gt-d4zhkq), and a run that fails to start is an error naming the verb.
func TestNodeImagePruneKubeWaitErrors(t *testing.T) {
	t.Parallel()
	const job = "node-prune-abc123"
	const leak = "secret-token"

	verbs := []struct {
		name     string
		call     func(k kubectlNodePrune) error
		wantExit string
		wantRun  string
	}{
		{
			name:     "WaitComplete",
			call:     func(k kubectlNodePrune) error { return k.WaitComplete(context.Background(), job) },
			wantExit: "complete: exited 1",
			wantRun:  "running kubectl wait",
		},
		{
			name: "Logs",
			call: func(k kubectlNodePrune) error {
				_, err := k.Logs(context.Background(), job)
				return err
			},
			wantExit: "kubectl logs of the node-prune Job exited 1",
			wantRun:  "running kubectl logs",
		},
		{
			name:     "Delete",
			call:     func(k kubectlNodePrune) error { return k.Delete(context.Background(), job) },
			wantExit: "kubectl delete of the node-prune Job exited 1",
			wantRun:  "running kubectl delete",
		},
	}
	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			t.Run("non-zero exit", func(t *testing.T) {
				run := func(context.Context, string, []string, string, ...string) (string, int, error) {
					return leak, 1, nil
				}
				err := v.call(kubectlNodePrune{dir: t.TempDir(), run: run})

				require.Error(t, err)
				assert.Contains(t, err.Error(), v.wantExit)
				assert.Contains(t, err.Error(), leak, "the error did not carry the head of kubectl's answer")
			})
			t.Run("run error", func(t *testing.T) {
				run := func(context.Context, string, []string, string, ...string) (string, int, error) {
					return "", 0, errors.New("exec: kubectl not found")
				}
				err := v.call(kubectlNodePrune{dir: t.TempDir(), run: run})

				require.Error(t, err)
				assert.Contains(t, err.Error(), v.wantRun)
			})
		})
	}
}

// TestNodeImagePruneKubeDeleteNotFoundIsSuccess: kubectl delete exits 0 with no output
// for a Job that does not exist under --ignore-not-found, so Delete returns nil and an
// Apply refusal adds no delete error of its own (gt-ej64ut, gt-lmpnfw).
func TestNodeImagePruneKubeDeleteNotFoundIsSuccess(t *testing.T) {
	t.Parallel()
	const job = "node-prune-abc123"

	t.Run("Job that does not exist", func(t *testing.T) {
		var gotArgs []string
		run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
			gotArgs = args
			return "", 0, nil
		}
		err := kubectlNodePrune{dir: t.TempDir(), run: run}.Delete(context.Background(), job)

		require.NoError(t, err)
		assert.Contains(t, gotArgs, "--ignore-not-found", "kubectl delete fails on a missing Job without --ignore-not-found")
	})

	t.Run("non-zero exit", func(t *testing.T) {
		run := func(context.Context, string, []string, string, ...string) (string, int, error) {
			return "", 1, nil
		}
		err := kubectlNodePrune{dir: t.TempDir(), run: run}.Delete(context.Background(), job)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "kubectl delete of the node-prune Job exited 1")
	})
}

// TestNodeImagePruneKubeListJSONToleratesWarnings: kubectl prints a deprecation
// warning on stdout and still exits 0, so ListJSON's answer carries lines around
// the JSON list. ListJSON returns exactly the object and nothing else; an answer
// with no JSON line, or a value that does not decode, is an error naming the verb
// (gt-x73zmz).
func TestNodeImagePruneKubeListJSONToleratesWarnings(t *testing.T) {
	t.Parallel()
	const list = `{"kind":"List","items":[]}`
	const warning = "Warning: v1 Pod is deprecated"

	tests := []struct {
		name    string
		out     string
		wantOut string
		wantErr bool
	}{
		{name: "warning before and after", out: warning + "\n" + list + "\nWarning: another\n", wantOut: list},
		{name: "pretty list", out: warning + "\n{\n  \"kind\": \"List\"\n}\n", wantOut: "{\n  \"kind\": \"List\"\n}"},
		{name: "no JSON", out: warning + "\n", wantErr: true},
		{name: "empty answer", out: "", wantErr: true},
		{name: "truncated", out: warning + "\n" + `{"kind":"List","items":[` + "\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := func(context.Context, string, []string, string, ...string) (string, int, error) {
				return tt.out, 0, nil
			}
			got, err := kubectlNodePrune{dir: t.TempDir(), run: run}.ListJSON(context.Background())

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "kubectl get")
				assert.NotContains(t, err.Error(), warning, "kubectl's output reached the error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOut, string(got))
		})
	}
}

// TestNodeImagePruneKubeErrorsCarryTheHead: a non-zero exit carries the verb, the
// exit status and the first 300 bytes of kubectl's answer, newlines turned to spaces
// and "..." where it was cut, so a Forbidden, an AlreadyExists and a policy refusal
// read apart in the log (gt-d4zhkq). Every verb's exit branch asserts the head in its
// own table above.
func TestNodeImagePruneKubeErrorsCarryTheHead(t *testing.T) {
	t.Parallel()
	const refusal = "Error from server (Forbidden): jobs.batch is forbidden\n"
	const tail = "TAILMARK"
	// Exactly 1000 bytes, so the head is the first 300 and the tail is past it.
	answer := refusal + strings.Repeat("x", 1000-len(refusal)-len(tail)) + tail

	const short = "Error\tfrom server (NotFound): jobs.batch \"x\" not found\n"
	tests := []struct {
		name string
		out  string
		want string
		cut  bool
	}{
		{name: "answer past the bound", out: answer, want: strings.ReplaceAll(answer[:300], "\n", " ") + "...", cut: true},
		{name: "answer under the bound", out: short, want: strings.NewReplacer("\n", " ", "\t", " ").Replace(short)},
		// Byte 300 falls inside the hundredth three-byte rune, so the head ends a
		// rune early and stays valid UTF-8.
		{name: "cut inside a rune", out: "a" + strings.Repeat("世", 100), want: "a" + strings.Repeat("世", 99) + "...", cut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := func(context.Context, string, []string, string, ...string) (string, int, error) {
				return tt.out, 1, nil
			}
			err := kubectlNodePrune{dir: t.TempDir(), run: run}.Delete(context.Background(), "node-prune-abc123")

			require.Error(t, err)
			assert.Contains(t, err.Error(), "kubectl delete of the node-prune Job exited 1")
			assert.Contains(t, err.Error(), tt.want)
			if !tt.cut {
				assert.NotContains(t, err.Error(), "...", "an answer under the bound was cut")
			}
			assert.NotContains(t, err.Error(), tail, "the error carries the answer past the 300-byte head")
			assert.NotContains(t, err.Error(), "\n", "a newline reached the log line")
			assert.NotContains(t, err.Error(), "\t", "a tab reached the log line")
			assert.True(t, utf8.ValidString(err.Error()), "the cut left half a rune")
		})
	}
}

// TestNodeImagePruneKubeWaitSeesAFailedJob: a node-prune Job has backoffLimit 0,
// so a helper that fails at once leaves a Failed Job that never becomes complete.
// Waiting only on complete held for kubectl's whole 960 second timeout. WaitComplete
// races a wait on complete and a wait on failed: the first to exit 0 decides, the
// other kubectl is canceled and waited for, and the Failed Job is an error naming
// the Job (gt-25fx5t).
func TestNodeImagePruneKubeWaitSeesAFailedJob(t *testing.T) {
	t.Parallel()
	const job = "node-prune-abc123"

	tests := []struct {
		name       string
		first      string // the condition whose wait exits 0
		wantFailed bool
	}{
		{name: "failed exits 0 first", first: "failed", wantFailed: true},
		{name: "complete exits 0 first", first: "complete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			argvs := map[string][]string{}
			canceled := make(chan string, 1)
			run := func(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
				condition := waitConditionArgv(args)
				mu.Lock()
				argvs[condition] = args
				mu.Unlock()
				if condition == tt.first {
					return "", 0, nil
				}
				<-ctx.Done()
				canceled <- condition
				return "", -1, ctx.Err()
			}

			err := kubectlNodePrune{dir: t.TempDir(), run: run}.WaitComplete(context.Background(), job)

			// WaitComplete returns only after the canceled wait's exec returned,
			// so no goroutine or kubectl is left behind.
			select {
			case condition := <-canceled:
				assert.Equal(t, canceledCondition(tt.first), condition)
			default:
				t.Fatalf("WaitComplete returned with the %s wait still running", canceledCondition(tt.first))
			}

			mu.Lock()
			defer mu.Unlock()
			for _, condition := range []string{"complete", "failed"} {
				assert.Equal(t, []string{"-n", "gt", "wait", "--for=condition=" + condition, "job/" + job, "--timeout=960s"}, argvs[condition])
			}

			if !tt.wantFailed {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the node-prune Job "+job+" failed")
		})
	}

	// The wait that errors need not be the one that reports the Job's condition:
	// the first to exit 0 decides whichever order the two end in.
	t.Run("one wait errors, the other exits 0", func(t *testing.T) {
		cases := []struct {
			name      string
			exitsZero string
			wantNoErr bool
		}{
			{name: "failed exits 0", exitsZero: "failed", wantNoErr: false},
			{name: "complete exits 0", exitsZero: "complete", wantNoErr: true},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
					if waitConditionArgv(args) == c.exitsZero {
						return "", 0, nil
					}
					return "", 1, nil
				}
				err := kubectlNodePrune{dir: t.TempDir(), run: run}.WaitComplete(context.Background(), job)

				if c.wantNoErr {
					assert.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), "the node-prune Job "+job+" failed")
			})
		}
	})
}

// canceledCondition names the wait that is not the first to exit 0.
func canceledCondition(first string) string {
	if first == "failed" {
		return "complete"
	}
	return "failed"
}

// TestNodeImagePruneKubeWaitNamesBothStatuses: when neither wait reports its
// condition, WaitComplete is an error naming both, so a refused wait is never
// read as a clean one (gt-25fx5t).
func TestNodeImagePruneKubeWaitNamesBothStatuses(t *testing.T) {
	t.Parallel()
	const job = "node-prune-abc123"

	run := func(context.Context, string, []string, string, ...string) (string, int, error) {
		return "", 1, nil
	}
	err := kubectlNodePrune{dir: t.TempDir(), run: run}.WaitComplete(context.Background(), job)

	require.Error(t, err)
	assert.Contains(t, err.Error(), job)
	assert.Contains(t, err.Error(), "complete: exited 1")
	assert.Contains(t, err.Error(), "failed: exited 1")
}
