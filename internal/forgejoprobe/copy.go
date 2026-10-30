package forgejoprobe

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
)

// gatePath is where a rig's gate workflow lives in the copy's tree.
const gatePath = ".forgejo/workflows/gate.yml"

// CloneCopy clones the rig's GitHub repository into dest in full. Forgejo
// refuses a shallow push, so no --depth is passed.
func CloneCopy(g Git, githubURL, dest string) error {
	if _, err := g.Run("", nil, "clone", "--quiet", githubURL, dest); err != nil {
		return fmt.Errorf("git clone %s failed: %w", githubURL, err)
	}
	return nil
}

// GateIdentity reads the gate workflow from the ref the probe will push, which
// is the tree Forgejo runs, and returns the gate's identity with the commit it
// names. A copy without the file is refused: Forgejo would then run the repo's
// .github/workflows and nothing would gate. A ref that names no commit is
// refused too, since there is no SHA to push.
func GateIdentity(g Git, dir, mainBranch string) (land.GateWorkflow, string, error) {
	ref := "refs/remotes/origin/" + mainBranch
	doc, err := g.Run(dir, nil, "show", ref+":"+gatePath)
	if err != nil {
		return land.GateWorkflow{}, "", fmt.Errorf("refusing: the copy of %s's %s has no %s, so Forgejo would run the repo's .github/workflows and nothing would gate", repoLabel(g, dir), mainBranch, gatePath)
	}
	wf, err := land.ParseGateWorkflow([]byte(doc))
	if err != nil {
		return land.GateWorkflow{}, "", err
	}
	sha, err := g.Run(dir, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return land.GateWorkflow{}, "", fmt.Errorf("the copy has no %s", ref)
	}
	return wf, strings.TrimSpace(sha), nil
}

// repoLabel names the copy's repository as OWNER/NAME, from its origin remote,
// for the refusal that names it. It is read only when a refusal is written, so
// an accepted copy costs the two reads GateIdentity makes and no more.
func repoLabel(g Git, dir string) string {
	remote, err := g.Run(dir, nil, "config", "--get", "remote.origin.url")
	if err != nil {
		return "the repository"
	}
	// The scp form git@host:owner/name splits like the URL form once its colon
	// is a slash.
	path := strings.TrimSuffix(strings.TrimSpace(strings.ReplaceAll(remote, ":", "/")), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "the repository"
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}
