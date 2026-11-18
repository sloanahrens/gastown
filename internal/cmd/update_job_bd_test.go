package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// writeBdlessImages writes a values file with the daemon and polecat pair and no bd
// key, the file a `gt update images` run without --bd records, and returns its path.
func writeBdlessImages(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "images.yaml")
	if err := imagevalues.Write(path, imagevalues.Values{Daemon: podSwapDigestRef, Polecat: podPairPolecatRef, Commit: rollbackNewCommit}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
	return path
}

// TestUpdateJobBdPodTargetRefusesImagesWithoutBd holds the refusal: a pod-target run
// whose --images file records no bd key stops before any work, and the error names
// the bd key. Both pod-target flags and a dry run are refused the same way.
func TestUpdateJobBdPodTargetRefusesImagesWithoutBd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "--pod-context", args: []string{"--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
		{name: "--pod-in-cluster", args: []string{"--pod-in-cluster", "--pod-namespace", podSwapNamespace}},
		{name: "--pod-context with --dry-run", args: []string{"--dry-run", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"--images", writeBdlessImages(t)}, tt.args...)
			got, err := runCountedPodCommand(t, false, args...)
			if err == nil {
				t.Fatalf("gt update %v ran, want it refused", args)
			}
			if !strings.Contains(err.Error(), "records no bd key") {
				t.Errorf("error = %q, want it to name the bd key", err.Error())
			}
			if code := exitCodeForError(err); code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if got.townLookups != 0 || got.depsCalls != 0 || got.runs != 0 || got.host.calls != 0 || len(got.kube.calls) != 0 {
				t.Errorf("refused run did work: town lookups %d, deps calls %d, runs %d, host installs %d, kubectl calls %d",
					got.townLookups, got.depsCalls, got.runs, got.host.calls, len(got.kube.calls))
			}
		})
	}
}

// TestUpdateJobBdRehearsalImagesFileUnchanged holds the runs the refusal leaves alone:
// a run with no pod target and an images file with no bd key, the file a rehearsal
// writes, is accepted and the file's bytes are left as they were.
func TestUpdateJobBdRehearsalImagesFileUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantRuns int
	}{
		{name: "run", args: nil, wantRuns: 1},
		{name: "--dry-run", args: []string{"--dry-run"}, wantRuns: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeBdlessImages(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the values file: %v", err)
			}
			got, err := runCountedPodCommand(t, false, append([]string{"--images", path}, tt.args...)...)
			if err != nil {
				t.Fatalf("gt update with no pod target and a bd-less images file was refused: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the values file after the run: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("images file changed under a non-pod run:\nbefore %q\nafter  %q", before, after)
			}
			if got.runs != tt.wantRuns {
				t.Errorf("orchestrator called %d times, want %d", got.runs, tt.wantRuns)
			}
		})
	}
}
