package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/jobledger"
)

// nodeImagePruneRunID is a real-shaped update run id: a stamp whose T a Job name
// cannot carry, which is why the name comes from nodePruneJobName.
const nodeImagePruneRunID = "20261104T200648Z-e41a9ded"

// nodeImagePruneStart is the run's start time handed to nodeImagePruneOnce.
var nodeImagePruneStart = time.Date(2026, 11, 4, 20, 6, 48, 0, time.UTC)

// nodeImagePruneKubeList is a listing the prune can take spares from.
func nodeImagePruneKubeList() []byte {
	return []byte(jsonList(jsonDeployment("gt-daemon", digestN("a"))))
}

// TestNodeImagePruneOnceLogsTheDryRun: a dry run logs the summary line first and
// then each would-remove line it planned, one logger call per line; an applied run
// logs only its summary line.
func TestNodeImagePruneOnceLogsTheDryRun(t *testing.T) {
	t.Parallel()
	// A prefix on the rig's logger marks each call, not each newline in a
	// message: a call carrying several lines shows the prefix once, as the
	// daemon's timestamp does, so four prefixed lines mean four calls.
	t.Run("dry run", func(t *testing.T) {
		t.Parallel()
		r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
		var logs bytes.Buffer
		r.d.logger = log.New(&logs, "daemon: ", 0)
		kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodePruneLogDryThree)}

		require.NoError(t, r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, false, nodeImagePruneStart))

		want := []string{
			"daemon: node_image_prune: dry run planned 3 images",
			"daemon: node-prune: would remove sha256:aaa gastown-polecat created 2026-11-20T03:00:00Z",
			"daemon: node-prune: would remove sha256:bbb gastown-daemon created 2026-11-20T03:01:00Z",
			"daemon: node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z",
		}
		assert.Equal(t, want, strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n"))
	})
	t.Run("apply run", func(t *testing.T) {
		t.Parallel()
		r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
		var logs bytes.Buffer
		r.d.logger = log.New(&logs, "daemon: ", 0)
		kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodeImagePruneLogApplied)}

		require.NoError(t, r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, true, nodeImagePruneStart))

		assert.Equal(t, "daemon: node_image_prune: applied removed 7 failed 2\n", logs.String(),
			"an applied run logs its summary line and no would-remove line")
	})
}

// TestNodeImagePruneOnceUsesTheRunningPolecatImage: the applied Job is named as
// nodePruneJobName names it for a real-shaped run id, runs the running values
// file's Polecat reference -- not the daemon's -- and keeps three. A town with no
// values file applies nothing.
func TestNodeImagePruneOnceUsesTheRunningPolecatImage(t *testing.T) {
	t.Parallel()
	t.Run("applies the running polecat reference", func(t *testing.T) {
		t.Parallel()
		r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
		kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodePruneLogDry)}

		require.NoError(t, r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, false, nodeImagePruneStart))

		var doc nodePruneJobDoc
		require.NoError(t, json.Unmarshal(kube.sent, &doc), "the applied manifest does not parse")
		assert.Regexp(t, `^gt-node-prune-[a-z0-9]{12}$`, doc.Metadata.Name)
		assert.Equal(t, "gt-node-prune-20261104t200", doc.Metadata.Name)
		_, polecat := runningRefs()
		assert.Equal(t, polecat, doc.Spec.Template.Spec.Containers[0].Image)
		assert.Equal(t, []string{"3"}, nodePruneFlagValues(doc.Spec.Template.Spec.Containers[0].Command, "--keep"))
	})
	t.Run("applies nothing without a values file", func(t *testing.T) {
		t.Parallel()
		r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
		require.NoError(t, os.Remove(imagevalues.PathIn(filepath.Join(r.town, "gastown"))))
		kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodePruneLogDry)}

		err := r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, false, nodeImagePruneStart)

		require.Error(t, err)
		assert.ErrorContains(t, err, "no running polecat image")
		assert.Empty(t, kube.calls, "a run with no values file must not touch the cluster")
		assert.Nil(t, kube.sent, "a run with no values file must apply nothing")
	})
}

// nodePruneLogDryThree is a dry run's log of three would-remove lines, as the
// helper writes it.
const nodePruneLogDryThree = "node-prune: would remove sha256:aaa gastown-polecat created 2026-11-20T03:00:00Z\n" +
	"node-prune: would remove sha256:bbb gastown-daemon created 2026-11-20T03:01:00Z\n" +
	"node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z\n" +
	"node-prune: dry run planned 3 images\n"

// TestNodeImagePruneOnceDryRow is the bead's box: a dry run appends one ok row
// of job node_image_prune with the report's planned count and no removals.
func TestNodeImagePruneOnceDryRow(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	ended := nodeImagePruneStart.Add(time.Minute)
	r.d.clock = clockwork.NewFakeClockAt(ended)
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodePruneLogDryThree)}

	require.NoError(t, r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, false, nodeImagePruneStart))

	ledger, err := jobledger.NewLedger(r.town)
	require.NoError(t, err)
	runs, err := ledger.Tail(nodeImagePruneJob, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1, "a dry run appends one row")
	assert.Equal(t, jobledger.Run{
		Job:       nodeImagePruneJob,
		StartedAt: nodeImagePruneStart,
		EndedAt:   ended,
		Outcome:   "ok",
		Stats:     map[string]int64{"planned": 3, "removed": 0, "failed": 0},
	}, runs[0])
}

// TestNodeImagePruneOnceDryRowLedgerRefused: a ledger that cannot record the row
// leaves the dry run's result unchanged and logs the failure once: the ledger
// records the run, it does not decide it.
func TestNodeImagePruneOnceDryRowLedgerRefused(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	// A regular file where the ledger's directory belongs refuses every append.
	require.NoError(t, os.MkdirAll(filepath.Join(r.town, ".runtime"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(r.town, ".runtime", "jobs"), []byte("not a directory"), 0o644))
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodePruneLogDryThree)}

	require.NoError(t, r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, false, nodeImagePruneStart))

	// The dry run logs its report lines as well; the refused ledger adds exactly one.
	assert.Equal(t, 1, strings.Count(r.logs.String(), "job ledger"), "the refused ledger logs one line")
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "job ledger") {
			assert.Contains(t, line, "node_image_prune")
		}
	}
}

// TestNodeImagePruneRecordLedgerFailsToOpen: a ledger the town root cannot open
// leaves the run's result unchanged and logs the failure once.
func TestNodeImagePruneRecordLedgerFailsToOpen(t *testing.T) {
	t.Parallel()
	d, logs, _, _ := newUpdateTriggerTestDaemon(t, nil)
	// jobledger.NewLedger refuses a town root of nothing, so the ledger is never
	// opened and no append is attempted.
	d.config.TownRoot = ""

	d.nodeImagePruneRecord(jobledger.Run{Job: nodeImagePruneJob})

	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	require.Len(t, lines, 1, "the ledger that cannot be opened logs one line")
	assert.Contains(t, lines[0], "node_image_prune")
	assert.Contains(t, lines[0], "job ledger")
}

// TestNodeImagePruneRecordLedgerFailsToAppend: an Append that fails leaves the
// run's result unchanged and logs the failure once.
func TestNodeImagePruneRecordLedgerFailsToAppend(t *testing.T) {
	t.Parallel()
	d, logs, _, _ := newUpdateTriggerTestDaemon(t, nil)
	// A regular file where the ledger's directory belongs refuses every append.
	require.NoError(t, os.MkdirAll(filepath.Join(d.config.TownRoot, ".runtime"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d.config.TownRoot, ".runtime", "jobs"), []byte("not a directory"), 0o644))

	d.nodeImagePruneRecord(jobledger.Run{Job: nodeImagePruneJob})

	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	require.Len(t, lines, 1, "a failing Append logs one line")
	assert.Contains(t, lines[0], "node_image_prune")
	assert.Contains(t, lines[0], "job ledger")
}

// nodeImagePruneLogApplied is an applied run's log as the helper writes it: 7
// removed lines, 2 that failed and the summary.
var nodeImagePruneLogApplied = strings.Repeat("node-prune: removed sha256:a\n", 7) +
	strings.Repeat("node-prune: cannot remove sha256:b: exit status 1\n", 2) +
	"node-prune: applied removed 7 failed 2\n"

// lastNodeImagePruneRun reads the newest node image prune row from the town's job ledger.
func lastNodeImagePruneRun(t *testing.T, town string) (jobledger.Run, bool) {
	t.Helper()
	ledger, err := jobledger.NewLedger(town)
	require.NoError(t, err)
	run, ok, err := ledger.Last(nodeImagePruneJob)
	require.NoError(t, err)
	return run, ok
}

// TestNodeImagePruneOnceApplyRecords: an applied run logs the summary line of
// its report and appends one ledger row carrying the report's figures.
func TestNodeImagePruneOnceApplyRecords(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodeImagePruneLogApplied)}

	require.NoError(t, r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, true, nodeImagePruneStart))

	assert.Contains(t, r.logs.String(), "node_image_prune: applied removed 7 failed 2\n")
	run, ok := lastNodeImagePruneRun(t, r.town)
	require.True(t, ok, "the apply run appended no ledger row")
	assert.Equal(t, nodeImagePruneJob, run.Job)
	assert.Equal(t, "ok", run.Outcome)
	assert.Equal(t, nodeImagePruneStart, run.StartedAt)
	assert.Equal(t, updateTriggerTestTime, run.EndedAt)
	assert.Equal(t, map[string]int64{"planned": 9, "removed": 7, "failed": 2}, run.Stats)
}

// TestNodeImagePruneOnceApplyRecordsError: a Delete that fails after a good log
// leaves the apply done: its figures are logged and recorded, and the error is
// logged and returned beside them.
func TestNodeImagePruneOnceApplyRecordsError(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodeImagePruneLogApplied), fail: "Delete"}

	err := r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, true, nodeImagePruneStart)

	require.Error(t, err)
	assert.ErrorContains(t, err, "delete job")
	assert.Contains(t, r.logs.String(), "node_image_prune: applied removed 7 failed 2\n")
	assert.Contains(t, r.logs.String(), "Delete refused")
	run, ok := lastNodeImagePruneRun(t, r.town)
	require.True(t, ok, "the apply run appended no ledger row")
	assert.Equal(t, map[string]int64{"planned": 9, "removed": 7, "failed": 2}, run.Stats)
}

// TestNodeImagePruneOnceApplyZeroRemovalDeleteFailureRecords: an apply that removed
// nothing still ran, so a Job delete that fails after its log was read leaves its
// summary line logged and its row recorded, beside the error.
func TestNodeImagePruneOnceApplyZeroRemovalDeleteFailureRecords(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte("node-prune: applied removed 0 failed 0\n"), fail: "Delete"}

	err := r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, true, nodeImagePruneStart)

	require.Error(t, err)
	assert.ErrorContains(t, err, "delete job")
	assert.Contains(t, r.logs.String(), "node_image_prune: applied removed 0 failed 0\n")
	run, ok := lastNodeImagePruneRun(t, r.town)
	require.True(t, ok, "the zero-removal apply appended no ledger row")
	assert.Equal(t, map[string]int64{"planned": 0, "removed": 0, "failed": 0}, run.Stats)
}

// TestNodeImagePruneOnceApplyMalformedLogRecordsNothing: a log that does not parse
// gives no report to record even when the Job delete fails too: no summary line, no
// row, and the error returned.
func TestNodeImagePruneOnceApplyMalformedLogRecordsNothing(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte("node-prune: applied removed 7 failed 2"), fail: "Delete"}

	err := r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, true, nodeImagePruneStart)

	require.Error(t, err)
	assert.NotContains(t, r.logs.String(), "node_image_prune: applied")
	_, ok := lastNodeImagePruneRun(t, r.town)
	assert.False(t, ok, "a run whose log did not parse appended a ledger row")
}

// TestNodeImagePruneOnceApplyRowNotRecorded: an append the ledger refuses logs
// one line, and the applied run's result stands.
func TestNodeImagePruneOnceApplyRowNotRecorded(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	// A plain file where the ledger's directory belongs makes the append fail.
	require.NoError(t, os.MkdirAll(filepath.Join(r.town, ".runtime"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(r.town, ".runtime", "jobs"), nil, 0o644))
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodeImagePruneLogApplied)}

	require.NoError(t, r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, true, nodeImagePruneStart))

	assert.Contains(t, r.logs.String(), "node_image_prune: applied removed 7 failed 2\n")
	assert.Contains(t, r.logs.String(), "the job ledger could not record the run")
}

// TestNodeImagePruneOnceApplyFailedRunRecordsNothing: a run that fails before
// its log gives a report appends no figures and returns the error.
func TestNodeImagePruneOnceApplyFailedRunRecordsNothing(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodeImagePruneLogApplied), fail: "Apply"}

	err := r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, true, nodeImagePruneStart)

	require.Error(t, err)
	assert.ErrorContains(t, err, "apply job")
	assert.NotContains(t, r.logs.String(), "node_image_prune: applied")
	_, ok := lastNodeImagePruneRun(t, r.town)
	assert.False(t, ok, "a failed run appended a ledger row")
}

// TestNodeImagePruneOnceRefuses: an id nodePruneJobName refuses and a town with no
// running polecat image both return an error and touch no cluster; a runner error is
// returned after the Job was applied and deleted. Each is one row.
func TestNodeImagePruneOnceRefuses(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name      string
		runID     string
		noImage   bool
		fail      string
		wantErr   string
		wantCalls string
	}{
		{name: "short run id", runID: "20261104T20", wantErr: "does not give a lower-case DNS-1123 Job name"},
		{name: "no running polecat image", runID: nodeImagePruneRunID, noImage: true, wantErr: "no running polecat image"},
		{name: "runner error", runID: nodeImagePruneRunID, fail: "Apply", wantErr: "apply job gt-node-prune-20261104t200: Apply refused", wantCalls: "ListJSON Apply Delete"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			if row.noImage {
				require.NoError(t, os.Remove(imagevalues.PathIn(filepath.Join(r.town, "gastown"))))
			}
			kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(nodePruneLogDry), fail: row.fail}

			err := r.d.nodeImagePruneOnce(context.Background(), kube, row.runID, false, nodeImagePruneStart)

			require.Error(t, err)
			assert.ErrorContains(t, err, row.wantErr)
			assert.Equal(t, row.wantCalls, strings.Join(kube.calls, " "))
		})
	}
}

// nodeImagePruneRowJob is the job the row beads (gt-08qnmf, gt-i8zplo,
// gt-eg0ji0) record a node prune run under.
const nodeImagePruneRowJob = "node_image_prune"

// TestNodeImagePruneOnceAppendsNoRowOfItsOwn: a dry run appends exactly the one
// node_image_prune row bead gt-08qnmf owns and an apply of removed 1 failed 1
// exactly the one bead gt-i8zplo owns; neither appends a second. A runner error
// appends none, because that row is bead gt-eg0ji0's, and this test holds the
// line until it lands. The runner error is returned.
func TestNodeImagePruneOnceAppendsNoRowOfItsOwn(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name, logs, fail, wantErr string
		apply                     bool
		wantRows                  int
	}{
		{name: "dry run", logs: nodePruneLogDry, wantRows: 1},
		{name: "apply removed 1 failed 1", apply: true, logs: "node-prune: removed sha256:bbb gastown-polecat\n" +
			"node-prune: applied removed 1 failed 1\n", wantRows: 1},
		{name: "runner error", logs: nodePruneLogDry, fail: "Apply", wantErr: "Apply refused"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			kube := &fakeKube{list: nodeImagePruneKubeList(), logs: []byte(row.logs), fail: row.fail}

			err := r.d.nodeImagePruneOnce(context.Background(), kube, nodeImagePruneRunID, row.apply, nodeImagePruneStart)

			if row.wantErr != "" {
				require.ErrorContains(t, err, row.wantErr)
			} else {
				require.NoError(t, err)
			}
			ledger, err := jobledger.NewLedger(r.town)
			require.NoError(t, err)
			runs, err := ledger.Tail(nodeImagePruneRowJob, 10)
			require.NoError(t, err)
			assert.Len(t, runs, row.wantRows, "the job ledger's %s rows after the run", nodeImagePruneRowJob)
		})
	}
}
