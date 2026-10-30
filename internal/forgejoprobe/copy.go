package forgejoprobe

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
)

// gatePath is where a rig's gate workflow lives in the copy's tree.
const gatePath = ".forgejo/workflows/gate.yml"

// userinfo matches the user:secret@ of a URL, which git can print in its output.
var userinfo = regexp.MustCompile(`(://)[^/\s@]+@`)

// CloneCopy clones the rig's GitHub repository into dest in full. Forgejo
// refuses a shallow push, so no --depth is passed. The -- ends git's options,
// so a URL that starts with a dash is a repository, never an option.
func CloneCopy(g Git, githubURL, dest string) error {
	if _, err := g.Run("", nil, "clone", "--quiet", "--", githubURL, dest); err != nil {
		return fmt.Errorf("git clone failed: %s", redact(err.Error(), githubURL))
	}
	return nil
}

// redact takes the URL as given, and any credential git prints in a URL, out of
// a message before the message reaches an error.
func redact(msg, rawURL string) string {
	if rawURL != "" {
		msg = strings.ReplaceAll(msg, rawURL, "<repository URL>")
	}
	return userinfo.ReplaceAllString(msg, "${1}<redacted>@")
}

// GateIdentity returns the gate's identity and the commit SHA of the ref the
// probe will push, read from the ref rather than the work tree.
//
// The gate file is read from the ref, which is the tree Forgejo runs. A copy
// without the file is refused: Forgejo would then run the repo's
// .github/workflows and nothing would gate. A ref that names no commit is
// refused too, since there is no SHA to push.
func GateIdentity(g Git, dir, mainBranch string) (land.GateWorkflow, string, error) {
	ref := "refs/remotes/origin/" + mainBranch
	doc, err := g.Run(dir, nil, "show", ref+":"+gatePath)
	if err != nil {
		// git show fails for a missing ref and for a missing file alike, so
		// the ref decides which refusal the copy gets.
		if _, refErr := g.Run(dir, nil, "rev-parse", "--verify", ref+"^{commit}"); refErr != nil {
			return land.GateWorkflow{}, "", fmt.Errorf("the copy has no %s", ref)
		}
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
