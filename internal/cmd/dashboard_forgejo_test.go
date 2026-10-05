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
	writeRigForgejo(t, town, "beads", "http://forgejo:3000/sloan/beads.git")
	writeRigForgejo(t, town, "mango", "http://forgejo:3000/sloan/mango.git")
	writeRigForgejo(t, town, "om", "http://forgejo:3000/sloan/organic-mechanic.git")
	// hm is not cut over, and a remote that names no repository contributes
	// nothing rather than a name the API would 404 on.
	writeRigForgejo(t, town, "hm", "http://forgejo:3000/not-a-repo")

	assert.Equal(t,
		[]string{"sloan/beads", "sloan/mango", "sloan/organic-mechanic"},
		rigForgejoRepos(town),
		"rig-name order, every rig with a forgejo remote, none of the rest")
}
