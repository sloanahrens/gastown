package forgejoprobe

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
)

// gatePath is where a rig's gate workflow lives in the copy's tree.
const gatePath = ".forgejo/workflows/gate.yml"

// userinfo matches the user:secret@ of a URL, which git can print in its output.
var userinfo = regexp.MustCompile(`(://)[^/\s@]+@`)

// tokenSegment matches a path segment long enough to be a credential: a forge
// can carry its token as a path segment of the repository URL.
var tokenSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{20,}$`)

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
// a message before the message reaches an error. git prints the URL in more
// spellings than it was given in, so every spelling is replaced, longest first:
// the URL, the host and path without its scheme, the password and any
// token-looking path segment. The user:password@ that git prints is replaced
// whatever the URL was.
func redact(msg, rawURL string) string {
	for _, r := range redactions(rawURL) {
		msg = strings.ReplaceAll(msg, r.text, r.with)
	}
	return userinfo.ReplaceAllString(msg, "${1}<redacted>@")
}

// redaction is one text to replace in a message and what it becomes.
type redaction struct{ text, with string }

// redactions lists what redact replaces for rawURL, longest text first, so the
// whole URL is replaced before any part of it.
func redactions(rawURL string) []redaction {
	const placeholder, secret = "<repository URL>", "<redacted>"
	var out []redaction
	seen := map[string]bool{}
	add := func(text, with string) {
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		out = append(out, redaction{text, with})
	}
	for _, f := range urlSpellings(rawURL) {
		add(f, placeholder)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		// Without the parts of the URL, only its spellings can be replaced.
		return sortRedactions(out)
	}
	for _, f := range urlSpellings(u.Host + u.Path) {
		add(f, placeholder)
	}
	// A username alone is not a secret, and replacing it would mangle any
	// message that spells it; the user:password@ form is caught by userinfo.
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			add(u.User.String(), secret)
			add(pw, secret)
		}
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if tokenSegment.MatchString(seg) {
			add(seg, secret)
		}
	}
	return sortRedactions(out)
}

// urlSpellings are the forms one URL is printed in: as given, without a trailing
// slash, without .git, and without either.
func urlSpellings(s string) []string {
	noSlash := strings.TrimSuffix(s, "/")
	return []string{s, noSlash, strings.TrimSuffix(noSlash, ".git"), strings.TrimSuffix(s, ".git")}
}

// sortRedactions orders rs longest text first.
func sortRedactions(rs []redaction) []redaction {
	slices.SortStableFunc(rs, func(a, b redaction) int { return len(b.text) - len(a.text) })
	return rs
}

// hasTokenSegment reports whether a URL path carries a token-looking segment.
func hasTokenSegment(path string) bool {
	return slices.ContainsFunc(strings.Split(path, "/"), tokenSegment.MatchString)
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
