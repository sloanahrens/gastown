package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
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
