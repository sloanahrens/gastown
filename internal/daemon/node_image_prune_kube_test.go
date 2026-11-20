package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
					if len(args) == 3 {
						gotManifest, _ = os.ReadFile(args[2])
					}
					return tt.out, tt.exit, tt.runErr
				}
				err := kubectlNodePrune{dir: dir, run: run}.Apply(context.Background(), []byte(manifest))

				require.Len(t, gotArgs, 3)
				assert.Equal(t, []string{"create", "-f"}, gotArgs[:2])
				assert.Equal(t, filepath.Join(dir, nodePruneManifestName([]byte(manifest))), gotArgs[2], "the manifest was not written under the update directory by its name")
				assert.Equal(t, manifest, string(gotManifest), "kubectl did not read the rendered manifest")
				assert.True(t, hasDeadline, "kubectl ran with no deadline")
				_, statErr := os.Stat(gotArgs[2])
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
