package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNodeImagePruneKubeArgv: ListJSON and Apply run kubectl through the exec seam
// with exactly the argv the node-prune run needs, and no --request-timeout or
// --context flag, since the bound is a context deadline (gt-ye543d). A non-zero
// exit is an error naming the verb and the exit status, never kubectl's output.
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
				assert.NotContains(t, err.Error(), leak, "kubectl's output reached the error")
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
				assert.NotContains(t, err.Error(), leak, "kubectl's output reached the error")
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
			name:     "Delete",
			call:     func(k kubectlNodePrune) ([]byte, error) { return nil, k.Delete(context.Background(), job) },
			wantArgs: []string{"-n", "gt", "delete", "job", job, "--ignore-not-found", "--wait=true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgs []string
			run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
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
	}{
		{
			name: "WaitComplete",
			call: func(k kubectlNodePrune) error { return k.WaitComplete(context.Background(), job) },
			want: 1020 * time.Second,
		},
		{
			name: "Logs",
			call: func(k kubectlNodePrune) error {
				_, err := k.Logs(context.Background(), job)
				return err
			},
			want: 120 * time.Second,
		},
		{
			name: "Delete",
			call: func(k kubectlNodePrune) error { return k.Delete(context.Background(), job) },
			want: 120 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got time.Duration
			var hasDeadline bool
			run := func(ctx context.Context, _ string, _ []string, _ string, _ ...string) (string, int, error) {
				var deadline time.Time
				deadline, hasDeadline = ctx.Deadline()
				got = time.Until(deadline)
				return "", 0, nil
			}
			require.NoError(t, tt.call(kubectlNodePrune{dir: t.TempDir(), run: run}))

			require.True(t, hasDeadline, "kubectl ran with no deadline")
			// The seam reads the deadline a moment after it is set; a few seconds of
			// slack keeps the assertion about which bound was chosen.
			assert.InDelta(t, tt.want.Seconds(), got.Seconds(), 5, "the deadline is %s, want about %s", got, tt.want)
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
// an error naming the verb and the exit status, never kubectl's output, and a run
// that fails to start is an error naming the verb.
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
			wantExit: "kubectl wait for the node-prune Job exited 1",
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
				assert.NotContains(t, err.Error(), leak, "kubectl's output reached the error")
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
