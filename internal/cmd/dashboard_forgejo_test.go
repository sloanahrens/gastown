package cmd

import (
	"errors"
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

// remoteReader is the injected rig remote reader. A rig maps to the remote its
// setting names, or is unreadable; a rig in neither names no remote. It records
// the rigs it was asked about, in the order asked.
type remoteReader struct {
	remotes    map[string]string
	unreadable map[string]bool
	asked      []string
}

func (r *remoteReader) read(_, rig string) (string, error) {
	r.asked = append(r.asked, rig)
	if r.unreadable[rig] {
		return "", errors.New("setting unreadable")
	}
	return r.remotes[rig], nil
}

// logRecorder collects what the dashboard's logf was given, one entry per call.
type logRecorder struct{ lines []string }

func (l *logRecorder) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
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
			reader := &remoteReader{remotes: map[string]string{"beads": tc.remote}}
			rec := &logRecorder{}

			assert.Equal(t, tc.want, dashboardForgejoBase(town, reader.read, rec.logf))
			assert.Empty(t, rec.lines, "one host across the rigs is not a choice to log")
		})
	}
}

// With no rig naming a remote the base is empty, so the client keeps the
// built-in root and today's behaviour is unchanged.
func TestDashboardForgejoBaseNoRemoteKeepsDefault(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "mango")
	reader := &remoteReader{}
	rec := &logRecorder{}

	assert.Empty(t, dashboardForgejoBase(town, reader.read, rec.logf))
	assert.Equal(t, []string{"beads", "mango"}, reader.asked, "every rig is asked")
	assert.Empty(t, rec.lines)
}

// A registry the town cannot read leaves the built-in root in place and says
// so, rather than reading as a town with no rigs.
func TestDashboardForgejoBaseUnreadableRegistry(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	reader := &remoteReader{}
	rec := &logRecorder{}

	assert.Empty(t, dashboardForgejoBase(town, reader.read, rec.logf))
	assert.Len(t, rec.lines, 1, "the unreadable registry is named once")
}

// A rig whose Forgejo setting cannot be read does not stop the walk: the next
// rig with a readable remote supplies the root, and the unreadable rig is named.
// A rig that fails after the root is found is named too, and the root stays.
func TestDashboardForgejoBaseLaterRigFailureKeepsRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		remotes    map[string]string
		unreadable map[string]bool
		want       string
		named      string
	}{
		{
			name:       "a root found before the unreadable rig is kept",
			remotes:    map[string]string{"beads": "http://forgejo:3000/owner/beads.git"},
			unreadable: map[string]bool{"mango": true},
			want:       "http://forgejo:3000/api/v1",
			named:      "rig mango",
		},
		{
			name:       "an unreadable rig before the readable one is passed over",
			remotes:    map[string]string{"mango": "http://forgejo:3000/owner/mango.git"},
			unreadable: map[string]bool{"beads": true},
			want:       "http://forgejo:3000/api/v1",
			named:      "rig beads",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "beads", "mango")
			reader := &remoteReader{remotes: tc.remotes, unreadable: tc.unreadable}
			rec := &logRecorder{}

			assert.Equal(t, tc.want, dashboardForgejoBase(town, reader.read, rec.logf))
			if assert.Len(t, rec.lines, 1, "the failure is logged exactly once") {
				assert.Contains(t, rec.lines[0], tc.named)
			}
		})
	}
}

// A remote that names no host cannot give a root, so its rig is skipped and
// named; the next rig with a host supplies the root.
func TestDashboardForgejoBaseSkipsRemoteWithoutHost(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "mango")
	reader := &remoteReader{remotes: map[string]string{
		"beads": "forgejo:3000/owner/beads.git",
		"mango": "http://forgejo:3000/owner/mango.git",
	}}
	rec := &logRecorder{}

	assert.Equal(t, "http://forgejo:3000/api/v1", dashboardForgejoBase(town, reader.read, rec.logf))
	if assert.Len(t, rec.lines, 1) {
		assert.Contains(t, rec.lines[0], "rig beads", "the skipped rig is named")
	}
}

// Two rigs on different hosts: the first rig in rig-name order sets the root,
// and the choice is logged once. The panes are not split by host.
func TestDashboardForgejoBaseMixedHosts(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads", "mango")
	reader := &remoteReader{remotes: map[string]string{
		"beads": "http://forgejo:3000/owner/beads.git",
		"mango": "http://other:3000/owner/mango.git",
	}}
	rec := &logRecorder{}

	assert.Equal(t, "http://forgejo:3000/api/v1", dashboardForgejoBase(town, reader.read, rec.logf))
	if assert.Len(t, rec.lines, 1, "the choice is logged exactly once") {
		assert.Contains(t, rec.lines[0], "http://other:3000/api/v1", "the log names the host not read")
	}
}

// The production remote reader gives the remote a rig's setting names, nothing
// for a rig that names none, and an error for a setting it cannot read, so the
// walk can name that rig rather than read it as absent.
func TestRigForgejoRemoteReadsTheSetting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		write   func(t *testing.T, town string)
		want    string
		wantErr bool
	}{
		{
			name:  "a rig with no block names no remote",
			write: func(t *testing.T, town string) {},
		},
		{
			name: "a rig with a block names its remote",
			write: func(t *testing.T, town string) {
				writeRigForgejo(t, town, "beads", "http://forgejo:3000/owner/beads.git")
			},
			want: "http://forgejo:3000/owner/beads.git",
		},
		{
			name: "an unreadable setting is an error",
			write: func(t *testing.T, town string) {
				if err := os.WriteFile(filepath.Join(town, "beads", "config.json"), []byte("{not json"), 0644); err != nil {
					t.Fatalf("write beads config.json: %v", err)
				}
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "beads")
			tc.write(t, town)

			remote, err := rigForgejoRemote(town, "beads")
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, remote)
		})
	}
}

// The root comes from the injected reader alone: the function reads each rig's
// remote only through reader, so it starts no process of its own. (A PATH
// override would show the same, but a unit test may not set the environment.)
func TestDashboardForgejoBaseSpawnsNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "beads")
	reader := &remoteReader{remotes: map[string]string{"beads": "http://forgejo:3000/owner/beads.git"}}
	rec := &logRecorder{}

	assert.Equal(t, "http://forgejo:3000/api/v1", dashboardForgejoBase(town, reader.read, rec.logf))
	assert.Equal(t, []string{"beads"}, reader.asked, "the root comes from the injected reader")
	assert.Empty(t, rec.lines)
}
