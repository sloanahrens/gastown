//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// readRigOMJSON against a real rig layout: a bare .repo.git whose origin/master
// holds .om.json and whose origin/main holds a different one. The unit tests
// pin the branch and remote rule through a fake; this pins that the production
// wrapper opens the rig's .repo.git and reads the tip through real git
// (gt-8yk8n).
func TestIntegrationReadRigOMJSONReadsTheConfiguredBranchOfTheBareRepo(t *testing.T) {
	t.Parallel()
	town := omRigTown(t, omRigCfg("master", ""))
	bare := filepath.Join(town, omPanelRig, ".repo.git")
	work := filepath.Join(t.TempDir(), "work")
	runGitCmd(t, "", "init", "--bare", bare)
	runGitCmd(t, "", "init", work)
	runGitCmd(t, work, "config", "user.email", "test@example.com")
	runGitCmd(t, work, "config", "user.name", "Test User")
	commitOM := func(branch, body string) {
		t.Helper()
		runGitCmd(t, work, "checkout", "-q", "-B", branch)
		if err := os.WriteFile(filepath.Join(work, ".om.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitCmd(t, work, "add", ".om.json")
		runGitCmd(t, work, "commit", "-q", "-m", "om on "+branch)
		runGitCmd(t, work, "push", "-q", bare, branch+":refs/remotes/origin/"+branch)
	}
	commitOM("main", `{"threshold":0.1}`)
	commitOM("master", `{"threshold":0.8}`)

	data, found, err := readRigOMJSON(town, omPanelRig)
	if err != nil || !found || string(data) != `{"threshold":0.8}` {
		t.Errorf("read = %q found=%v err=%v; want master's file", data, found, err)
	}

	// A rig whose target branch is not in the repository is an error, not a rig
	// without a file.
	if err := os.WriteFile(filepath.Join(town, omPanelRig, "config.json"), []byte(omRigCfg("trunk", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, found, err = readRigOMJSON(town, omPanelRig); err == nil || found {
		t.Errorf("read of a missing branch = %q found=%v err=%v; want an error", data, found, err)
	}
}
