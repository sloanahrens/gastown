package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// writeRigForgejo gives a rig the merge_queue.forgejo block that marks it cut
// over to Forgejo, in the shape rig.ResolveForgejoConfig reads.
func writeRigForgejo(t *testing.T, townRoot, rig, remoteURL string) {
	t.Helper()
	body := fmt.Sprintf(`{"type":"rig","version":1,"name":%q,"merge_queue":{"forgejo":{"remote_url":%q}}}`, rig, remoteURL)
	if err := os.WriteFile(filepath.Join(townRoot, rig, "config.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write %s config.json: %v", rig, err)
	}
}

// The pane's repo list is every rig cut over to Forgejo, not the repos the
// viewer's token happens to see. Reading the token's own list is what let a rig
// the viewer cannot read go missing with nothing on the pane to say so: the
// dashboard never even asked for it (gt-faml5).
func TestRigForgejoReposIsEveryCutOverRig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "hm", "mango", "om")
	writeRigForgejo(t, town, "beads", "http://forgejo:3000/owner/beads.git")
	writeRigForgejo(t, town, "mango", "http://forgejo:3000/owner/mango.git")
	writeRigForgejo(t, town, "om", "http://forgejo:3000/owner/organic-mechanic.git")
	// hm is not cut over, and a remote that names no repository contributes
	// nothing rather than a name the API would 404 on.
	writeRigForgejo(t, town, "hm", "http://forgejo:3000/not-a-repo")

	assert.Equal(t,
		[]string{"owner/beads", "owner/mango", "owner/organic-mechanic"},
		rigForgejoRepos(town),
		"rig-name order, every rig with a forgejo remote, none of the rest")
}

// The Forgejo and deploys panes read the API root the rigs' Forgejo remote
// names, the same root the daemon derives, not the built-in loopback root that
// is the pod's own address in the control cluster (gt-6qc3nw.1).
func TestDashboardForgejoBaseFromRigRemote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{"cluster service host", "http://forgejo:3000/sloan/x.git", "http://forgejo:3000/api/v1"},
		{"loopback remote", "http://127.0.0.1:3000/sloan/x.git", "http://127.0.0.1:3000/api/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "beads")
			writeRigForgejo(t, town, "beads", tc.remote)

			var logged []string
			logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
			assert.Equal(t, tc.want, dashboardForgejoBase(town, logf))
			assert.Empty(t, logged, "one host across the rigs is not a choice to log")
		})
	}
}

// With no rig naming a remote the base is empty, so the client keeps the
// built-in root and today's behaviour is unchanged.
func TestDashboardForgejoBaseNoRemoteKeepsDefault(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "mango")

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	assert.Empty(t, dashboardForgejoBase(town, logf))
	assert.Empty(t, logged)
}

// A registry the town cannot read leaves the built-in root in place and says
// so, rather than reading as a town with no rigs.
func TestDashboardForgejoBaseUnreadableRegistry(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	assert.Empty(t, dashboardForgejoBase(town, logf))
	assert.Len(t, logged, 1, "the unreadable registry is named once")
}

// A rig whose Forgejo setting the town cannot read does not hand the root to
// the next rig: the first rig's remote is unknown, so the panes keep the
// built-in root and the rig is named (the daemon's townForgejoBaseURL rule).
func TestDashboardForgejoBaseUnreadableRigSetting(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "mango")
	if err := os.WriteFile(filepath.Join(town, "beads", "config.json"), []byte("{not json"), 0644); err != nil {
		t.Fatalf("write beads config.json: %v", err)
	}
	writeRigForgejo(t, town, "mango", "http://forgejo:3000/owner/mango.git")

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	assert.Empty(t, dashboardForgejoBase(town, logf))
	if assert.Len(t, logged, 1) {
		assert.Contains(t, logged[0], "rig beads", "the unreadable rig is named")
	}
}

// A remote that names no host cannot give a root, so its rig is skipped and
// named; the next rig with a host supplies the root.
func TestDashboardForgejoBaseSkipsRemoteWithoutHost(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "mango")
	writeRigForgejo(t, town, "beads", "forgejo:3000/owner/beads.git")
	writeRigForgejo(t, town, "mango", "http://forgejo:3000/owner/mango.git")

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	assert.Equal(t, "http://forgejo:3000/api/v1", dashboardForgejoBase(town, logf))
	if assert.Len(t, logged, 1) {
		assert.Contains(t, logged[0], "rig beads", "the skipped rig is named")
	}
}

// Two rigs on different hosts: the first rig in rig-name order sets the root,
// and the choice is logged once. The panes are not split by host.
func TestDashboardForgejoBaseMixedHosts(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "mango")
	writeRigForgejo(t, town, "mango", "http://other:3000/owner/mango.git")
	writeRigForgejo(t, town, "beads", "http://forgejo:3000/owner/beads.git")

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	assert.Equal(t, "http://forgejo:3000/api/v1", dashboardForgejoBase(town, logf))
	if assert.Len(t, logged, 1, "the choice is logged exactly once") {
		assert.Contains(t, logged[0], "http://other:3000/api/v1", "the log names the host not read")
	}
}
