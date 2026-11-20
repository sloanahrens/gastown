package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/update"
)

// newNodeImagePruneDecisionDaemon returns a daemon on a temp town whose update
// Job list is empty and whose disk maintenance runs in mode.
func newNodeImagePruneDecisionDaemon(t *testing.T, mode string) (*Daemon, string) {
	t.Helper()
	town := t.TempDir()
	d := &Daemon{
		config:       &Config{TownRoot: town},
		patrolConfig: diskMaintEnabled("04:30", mode),
		updateJob:    &fakeUpdateJob{},
	}
	return d, town
}

func writeNodeImagePruneDecisionState(t *testing.T, town string, st update.State) {
	t.Helper()
	store := update.Store{Dir: update.Dir(town)}
	if err := store.Write(st); err != nil {
		t.Fatalf("write update state: %v", err)
	}
}

// TestNodeImagePruneDecisionMode drives an ok run with no active Job and an
// unrecorded id through both disk-maintenance modes: the decision is ok either
// way, and only apply mode applies it.
func TestNodeImagePruneDecisionMode(t *testing.T) {
	t.Parallel()
	ended := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	for _, mode := range []string{agentconfig.DiskMaintenanceModeDryRun, agentconfig.DiskMaintenanceModeApply} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			d, town := newNodeImagePruneDecisionDaemon(t, mode)
			writeNodeImagePruneDecisionState(t, town, update.State{
				RunID: "run-2", Outcome: update.OutcomeOK, EndedAt: ended,
			})

			runID, apply, ok, why := d.nodeImagePruneDecision()
			if !ok {
				t.Fatalf("nodeImagePruneDecision() refused an ok run: %s", why)
			}
			if runID != "run-2" {
				t.Errorf("run id = %q, want %q", runID, "run-2")
			}
			if want := mode == agentconfig.DiskMaintenanceModeApply; apply != want {
				t.Errorf("apply = %v in mode %q, want %v", apply, mode, want)
			}
		})
	}
}

// TestNodeImagePruneDecisionRefuses drives a partial run, an active Job, an
// already pruned id and an unreadable read through the decision: each refuses
// with its reason. A Job whose conditions say Failed or Complete has finished
// and does not hold the prune.
func TestNodeImagePruneDecisionRefuses(t *testing.T) {
	t.Parallel()
	ended := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	okState := update.State{RunID: "run-2", Outcome: update.OutcomeOK, EndedAt: ended}
	failedJob := `{"items":[{"metadata":{"name":"gt-update-fe633c9ea2fb"},"status":{"failed":3,"conditions":[{"type":"Failed","status":"True"}]}}]}`
	completeJob := `{"items":[{"metadata":{"name":"gt-update-done1"},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}]}`
	tests := []struct {
		name       string
		state      update.State
		corrupt    bool
		lastPruned string
		lastRunDir bool
		active     []string
		listErr    error
		listing    string
		mode       string
		wantOK     bool
		wantWhy    string
	}{
		{"a partial run refuses", update.State{RunID: "run-2", Outcome: update.OutcomePartial, EndedAt: ended}, false, "", false, nil, nil, "", "", false, "partial"},
		{"an active update Job refuses", okState, false, "", false, []string{"gt-update-inflight"}, nil, "", "", false, "update Job active"},
		{"an already pruned run refuses", okState, false, "run-2", false, nil, nil, "", "", false, "already pruned"},
		{"an unreadable state refuses", okState, true, "", false, nil, nil, "", "", false, "the update state could not be read"},
		{"an unreadable last-run record refuses", okState, false, "", true, nil, nil, "", "", false, "reading the node image prune last-run record"},
		{"an unreadable Job list refuses before the gate", okState, false, "", false, nil, errors.New("kubectl get jobs exited 1"), "", "", false, "update Job list could not be read: kubectl get jobs exited 1"},
		{"an invalid mode refuses", okState, false, "", false, nil, nil, "", "sometimes", false, "patrols.disk_maintenance.mode"},
		{"a Job with a Failed condition does not hold", okState, false, "", false, nil, nil, failedJob, "", true, ""},
		{"a Job with a Complete condition does not hold", okState, false, "", false, nil, nil, completeJob, "", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mode := tt.mode
			if mode == "" {
				mode = agentconfig.DiskMaintenanceModeApply
			}
			d, town := newNodeImagePruneDecisionDaemon(t, mode)
			if tt.corrupt {
				store := update.Store{Dir: update.Dir(town)}
				path := store.Path()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("create the update state directory: %v", err)
				}
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatalf("write a corrupt update state: %v", err)
				}
			} else {
				writeNodeImagePruneDecisionState(t, town, tt.state)
			}
			if tt.lastPruned != "" {
				if err := writeNodeImagePruneLast(town, tt.lastPruned); err != nil {
					t.Fatalf("record the last pruned run: %v", err)
				}
			}
			if tt.lastRunDir {
				if err := os.MkdirAll(filepath.Join(constants.TownRuntimePath(town), nodeImagePruneLastFile), 0o755); err != nil {
					t.Fatalf("create a directory where the last-run record belongs: %v", err)
				}
			}
			switch {
			case tt.listErr != nil:
				d.updateJob = &fakeUpdateJob{listErr: tt.listErr}
			case tt.listing != "":
				d.updateJob = kubectlUpdateJob{dir: update.Dir(town), run: (&kubectlJobsFake{listing: tt.listing}).run}
			case tt.active != nil:
				d.updateJob = &fakeUpdateJob{active: tt.active}
			}

			_, apply, ok, why := d.nodeImagePruneDecision()
			if ok != tt.wantOK {
				t.Fatalf("nodeImagePruneDecision() ok = %v, want %v (why %q)", ok, tt.wantOK, why)
			}
			if ok {
				if !apply {
					t.Error("an ok decision in apply mode did not apply")
				}
				return
			}
			if apply {
				t.Error("a refused decision applied the prune")
			}
			if !strings.Contains(why, tt.wantWhy) {
				t.Errorf("why = %q, want it to contain %q", why, tt.wantWhy)
			}
		})
	}
}
