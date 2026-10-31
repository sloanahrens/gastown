package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejoresync"
)

// Cutover is one cutover run: the options it was parsed with, the git it reads and
// writes through, and where it logs.
type Cutover struct {
	Opts *Options
	// Git runs one git argv and returns its stdout; nil runs the git binary, each
	// call bounded as forgejoresync bounds its own.
	Git forgejoresync.Runner
	// Log writes one line to the run's stderr; nil writes nothing.
	Log func(string)
	// MkdirTemp and RemoveAll make and remove the scratch directory the import's
	// repository sits in; nil means os.MkdirTemp and os.RemoveAll.
	MkdirTemp func(dir, pattern string) (string, error)
	RemoveAll func(path string) error
	// ProvisionRun is step 3's provisioning run; nil runs forgejoprov.Run in this
	// process. A test replaces it to count the call and to force its exit code.
	ProvisionRun ProvisionFunc
	// ProvisionEnv is what that run reads: the environment the admin token resolves
	// from, the token file reader and the Forgejo transport. Provision replaces its
	// Log with the run's own, so a progress line reaches the operator the way every
	// other step's lines do.
	ProvisionEnv forgejoprov.Env
}

// errRefsMissing is the final comparison's refusal: Forgejo does not hold every
// GitHub ref after the push, so a later mirror sync would prune GitHub.
var errRefsMissing = errors.New("Forgejo is missing GitHub refs after the import")

// forgejoRefspecs fetch Forgejo's refs into the scratch repository under
// refs/forgejo/*, beside GitHub's refs/*. The import pushes ref by ref, so the
// push can never send this side: fetching both into one repository is what lets
// the plan read one sha against the other.
var forgejoRefspecs = []string{"+refs/heads/*:refs/forgejo/heads/*", "+refs/tags/*:refs/forgejo/tags/*"}

// ImportRefs is step 2b: it imports every GitHub branch and tag Forgejo lacks or
// is behind on, and stops on a ref whose two sides diverged unless the run accepts
// skipping it.
//
// The push is plain — no '+', no --force, no --prune — because a rig already cut
// over has Forgejo ahead of a frozen GitHub, and a forced push would discard every
// commit landed since (gt-fn9e6.51).
//
// Every URL this run prints goes through printedURL first, and every error it
// returns is scrubbed on the way out: an operator-supplied remote can carry a
// credential in its userinfo or its query, and the log and the refusal are both
// read and pasted into a bead. The scrub runs once, in the deferred redact below,
// because a git error prints the URL it was given wherever the call was made.
func (c *Cutover) ImportRefs() (err error) {
	defer func() { err = c.redact(err) }()
	o := c.Opts
	c.logf("importing every GitHub ref through %s without force", printedURL(o.AdminGitURL))

	githubBefore, err := c.listRefs(o.GitHubURL)
	if err != nil {
		return err
	}
	forgejoBefore, err := c.listRefs(o.AdminGitURL)
	if err != nil {
		return err
	}

	scratch, err := c.makeScratch()
	if err != nil {
		return err
	}
	defer c.removeScratch(scratch)
	if err := c.fetchBoth(scratch); err != nil {
		return err
	}

	plan, err := Plan(githubBefore, forgejoBefore, ExecRefGit{Dir: scratch, Run: c.git()})
	if err != nil {
		return err
	}
	specs, diverged := c.importSpecs(plan)
	if len(diverged) > 0 && !o.AcceptDiverged {
		c.logf("ERROR: %d ref(s) have diverged between GitHub and Forgejo:", len(diverged))
		for _, ref := range diverged {
			c.logf("  %s", ref)
		}
		return errors.New("reconcile the diverged refs, or re-run with --accept-diverged to skip them")
	}

	if len(specs) == 0 {
		c.logf("no GitHub ref needs importing")
	} else {
		c.logf("+ git -C %s push --quiet -- %s %s", scratch, printedURL(o.AdminGitURL), strings.Join(specs, " "))
		if !o.DryRun {
			if err := pushRefs(c.git(), scratch, o.AdminGitURL, specs); err != nil {
				return err
			}
		}
	}
	// A dry run stops here: the refs it would import were printed, and listing
	// both sides again would only say the push never happened.
	if o.DryRun {
		return nil
	}

	return c.checkEveryRefArrived()
}

// importSpecs logs the plan's line for every ref and returns the refspecs to push
// and the refs that diverged. A refspec is the plain ref-to-itself form: the plan
// already settled that a push of it is a creation or a fast-forward.
func (c *Cutover) importSpecs(plan []Decision) (specs, diverged []string) {
	for _, d := range plan {
		c.logf("%s", d.Line)
		switch d.Action {
		case ActionPush, ActionFastForward:
			specs = append(specs, d.Ref+":"+d.Ref)
		case ActionDiverged:
			diverged = append(diverged, d.Ref)
		}
	}
	return specs, diverged
}

// checkEveryRefArrived re-lists both sides and refuses a Forgejo copy that lacks a
// GitHub ref name. A mirror has no branch filter and prunes, so a name Forgejo
// lacks is deleted on GitHub at the next sync.
func (c *Cutover) checkEveryRefArrived() error {
	github, err := c.listRefs(c.Opts.GitHubURL)
	if err != nil {
		return err
	}
	forgejo, err := c.listRefs(c.Opts.AdminGitURL)
	if err != nil {
		return err
	}
	if missing := missingNames(github, forgejo); len(missing) > 0 {
		c.logf("ERROR: Forgejo is missing GitHub refs after the import; the mirror would prune them")
		for _, name := range missing {
			c.logf("%s", name)
		}
		return errRefsMissing
	}
	c.logf("imported every GitHub ref into %s (%d refs there)", c.Opts.Repo, len(forgejo))
	return nil
}

// missingNames is the refs github holds and forgejo does not, in ref-name order.
func missingNames(github, forgejo []forgejoresync.Ref) []string {
	have := make(map[string]bool, len(forgejo))
	for _, ref := range forgejo {
		have[ref.Name] = true
	}
	var missing []string
	for _, ref := range github {
		if !have[ref.Name] {
			missing = append(missing, ref.Name)
		}
	}
	return missing
}

// listRefs logs and reads one side's branch and tag listing, sorted by ref name.
func (c *Cutover) listRefs(url string) ([]forgejoresync.Ref, error) {
	c.logf("+ git ls-remote --heads --tags --refs %s", printedURL(url))
	out, err := forgejoresync.ExecGit{Run: c.git()}.LsRemote(url)
	if err != nil {
		return nil, fmt.Errorf("could not list the refs at %s: %w", printedURL(url), err)
	}
	refs, err := forgejoresync.ParseRefs(out)
	if err != nil {
		return nil, fmt.Errorf("could not read the ref listing at %s: %w", printedURL(url), err)
	}
	return refs, nil
}

// fetchBoth fills the scratch repository with both sides' objects, so the plan can
// ask one side's sha against the other's. A dry run fetches too: these writes stay
// inside the scratch directory, and the plan is unreadable without them.
func (c *Cutover) fetchBoth(scratch string) error {
	git := forgejoresync.ExecGit{Run: c.git()}
	c.logf("+ git init --bare --quiet %s", scratch)
	if err := git.InitBare(scratch); err != nil {
		return fmt.Errorf("could not make the scratch repository %s: %w", scratch, err)
	}
	for _, side := range []struct {
		url      string
		refspecs []string
	}{
		{url: c.Opts.AdminGitURL, refspecs: forgejoRefspecs},
		{url: c.Opts.GitHubURL, refspecs: forgejoresync.Refspecs},
	} {
		c.logf("+ git -C %s fetch --quiet -- %s %s", scratch, printedURL(side.url), strings.Join(side.refspecs, " "))
		if err := git.Fetch(scratch, side.url, side.refspecs...); err != nil {
			return fmt.Errorf("could not fetch %s into the scratch repository: %w", printedURL(side.url), err)
		}
	}
	return nil
}

// pushRefs sends specs from the scratch repository to url as a plain push, so a
// ref Forgejo is ahead on cannot move. The cutover installs no signal handler, so
// the push runs under the background context (gt-csjwi).
//
// Its error is returned as git wrote it: ImportRefs is its only caller, and the
// scrub there is what takes the URL out.
func pushRefs(git forgejoresync.Runner, scratch, url string, specs []string) error {
	argv := append([]string{"git", "-C", scratch, "push", "--quiet", "--", url}, specs...)
	if _, err := git(context.Background(), argv); err != nil {
		return fmt.Errorf("the push of %d ref(s) to the admin base failed: %w", len(specs), err)
	}
	return nil
}

// redactErr returns err with each URL, and each credential those URLs carry, taken
// out of its text. It is called once, at the boundary of ImportRefs: a git error
// prints the URL it was given, whichever call made it, and the boundary is the one
// place both URLs are in scope.
func redactErr(err error, urls ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := scrubURLs(msg, urls)
	if clean == msg {
		return err
	}
	return &redactedError{msg: clean, err: err}
}

// scrubURLs replaces each URL in msg with its printed form, then takes the
// credentials those URLs carry out of every URL-shaped word of the message.
func scrubURLs(msg string, urls []string) string {
	clean := msg
	// The whole URL goes first: a credential taken out on its own would stop the
	// URL that holds it from matching.
	for _, raw := range urls {
		if raw != "" {
			clean = strings.ReplaceAll(clean, raw, printedURL(raw))
		}
	}
	forms := credentialForms(urls)
	if len(forms) == 0 {
		return clean
	}
	hosts := urlHosts(urls)
	var b strings.Builder
	b.Grow(len(clean))
	start := 0
	for i := 0; i <= len(clean); i++ {
		if i < len(clean) && !isSpaceByte(clean[i]) {
			continue
		}
		b.WriteString(scrubWord(clean[start:i], forms, hosts))
		if i < len(clean) {
			b.WriteByte(clean[i])
		}
		start = i + 1
	}
	return b.String()
}

// scrubWord takes every credential form out of one word, when the word is a URL.
// A credential is matched only inside a URL word, so a one-letter query value
// cannot rewrite the rest of the message.
func scrubWord(word string, forms, hosts []string) string {
	if !looksLikeURL(word, hosts) {
		return word
	}
	for _, form := range forms {
		word = strings.ReplaceAll(word, form, "[redacted]")
	}
	return word
}

// looksLikeURL reports whether one word of a message is a URL or an abbreviation of
// one: either it names a scheme, or it carries a host this run knows, since git can
// print a URL without its scheme.
func looksLikeURL(word string, hosts []string) bool {
	if strings.Contains(word, "://") {
		return true
	}
	for _, host := range hosts {
		if host != "" && strings.Contains(word, host) {
			return true
		}
	}
	return false
}

// isSpaceByte reports whether b is the ASCII whitespace a message separates words
// with. A multibyte character is never one, so its bytes stay in their word.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// printedURL is how a URL reads in the run's log and its errors: without its
// userinfo, its query and its fragment, since each can carry a credential as the
// other can. A URL that does not parse reads "[redacted]", as no part of it can be
// told apart from a credential. A URL written without a scheme is taken apart as
// text, because url.Parse cannot tell its userinfo from its path.
func printedURL(raw string) string {
	if !strings.Contains(raw, "://") {
		return writtenURLWithoutCredentials(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[redacted]"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// writtenURLWithoutCredentials drops a scheme-less URL's userinfo, query and
// fragment, which its text alone marks. Text that has none of those parts is
// returned as it is.
func writtenURLWithoutCredentials(raw string) string {
	rest := raw
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	authority := rest
	if i := strings.IndexByte(authority, '/'); i >= 0 {
		authority = authority[:i]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		return rest[i+1:]
	}
	return rest
}

// credentialForms are every string a URL can print as a credential: its userinfo,
// its password and each query value, as the URL writes them and percent-decoded.
// git echoes a URL as written, so the written form is the one an error text holds,
// and the decoded form is the one anything else compares.
func credentialForms(urls []string) []string {
	var forms []string
	seen := make(map[string]bool)
	add := func(form string) {
		if form == "" || seen[form] {
			return
		}
		seen[form] = true
		forms = append(forms, form)
	}
	addBoth := func(form string, unescape func(string) (string, error)) {
		add(form)
		if decoded, err := unescape(form); err == nil {
			add(decoded)
		}
	}
	for _, raw := range urls {
		userinfo, _, query := urlTextParts(raw)
		for _, pair := range strings.FieldsFunc(query, func(r rune) bool { return r == '&' || r == ';' }) {
			if _, value, ok := strings.Cut(pair, "="); ok {
				addBoth(value, url.QueryUnescape)
			}
		}
		add(userinfo)
		if _, password, ok := strings.Cut(userinfo, ":"); ok {
			addBoth(password, url.PathUnescape)
		}
	}
	return forms
}

// urlHosts are the hosts the run's URLs name, as their text writes them, so a
// credential search finds a URL printed without its scheme.
func urlHosts(urls []string) []string {
	var hosts []string
	for _, raw := range urls {
		if _, host, _ := urlTextParts(raw); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// urlTextParts splits a URL, with or without a scheme, into the userinfo and the
// host its text writes and the query it carries. Text parsing is what tells a
// scheme-less URL's userinfo from its path, which url.Parse cannot.
func urlTextParts(raw string) (userinfo, host, query string) {
	rest := raw
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		query, rest = rest[i+1:], rest[:i]
	}
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		userinfo, rest = rest[:i], rest[i+1:]
	}
	return userinfo, rest, query
}

// redactedError is an error whose text has its credentials taken out. The error it
// was made from is deliberately not reachable by unwrapping, since its text is the
// leak this type exists to close. Is and As keep errors.Is and errors.As matching
// through it.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Is(target error) bool { return errors.Is(e.err, target) }

func (e *redactedError) As(target any) bool { return errors.As(e.err, target) }

// redact is redactErr over the two git URLs this run reads and pushes through.
func (c *Cutover) redact(err error) error {
	return redactErr(err, c.Opts.GitHubURL, c.Opts.AdminGitURL)
}

// makeScratch creates the scratch directory and returns the bare repository inside
// it, as the script's $WORK/import.git.
func (c *Cutover) makeScratch() (string, error) {
	mkdir := c.MkdirTemp
	if mkdir == nil {
		mkdir = os.MkdirTemp
	}
	work, err := mkdir("", "forgejo-cutover.")
	if err != nil {
		return "", fmt.Errorf("could not make the scratch directory: %w", err)
	}
	return filepath.Join(work, "import.git"), nil
}

// removeScratch removes the scratch directory makeScratch returned the repository
// of. A directory left behind is litter, not a failed step.
func (c *Cutover) removeScratch(scratch string) {
	rm := c.RemoveAll
	if rm == nil {
		rm = os.RemoveAll
	}
	_ = rm(filepath.Dir(scratch))
}

// git is the runner the cutover's git calls go through.
func (c *Cutover) git() forgejoresync.Runner {
	if c.Git != nil {
		return c.Git
	}
	return forgejoresync.RunExec
}

// logf writes one line of the run's log, when the run has one.
func (c *Cutover) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}
