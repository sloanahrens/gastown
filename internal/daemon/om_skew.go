package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The om-skew check compares the installed om with the om rig's origin/main
// at daemon start and then hourly. gastown feeds om new flags and new gate
// directives, and a landing is reviewed by whichever om is installed, so an
// om installed before a flag landed reviews against the older binary's
// reading — the om-cix, om-rym and om-c05 incidents, where merges landed
// unreviewed or under a drifted gate.
//
// It reports and nothing else: no path here blocks, rejects or delays a
// landing, the readings run on their own goroutine, and a reading that cannot
// be taken is not an alarm but a log line.
const (
	// omSkewAlertKey is the stable identity of the one condition this check
	// owns. Every escalation goes out under it, so gt escalate records a
	// persisting skew as a repeat on the bead the key already names rather
	// than one bead an hour (gt-vwry).
	omSkewAlertKey = "om-skew"
	// omSkewSource is the escalation source: om reviews landings, so the
	// landing worker owns this condition.
	omSkewSource = "landing-worker"

	// omSkewInterval is the gap between checks after the first. An hour is
	// the shortest wait a landing-affecting skew is worth re-reading at; the
	// first check runs as the monitor starts, so an operator who just
	// installed om does not wait one out.
	omSkewInterval = time.Hour

	// omSkewVersionTimeout bounds one `<omPath> version` call and
	// omSkewRemoteTimeout one remote read. Both are short enough that a hung
	// check cannot outlive the interval in which the next one is due.
	omSkewVersionTimeout = 20 * time.Second
	omSkewRemoteTimeout  = 20 * time.Second

	// omSkewRigWorktree is the om rig's canonical worktree, the only place
	// the daemon reads om's origin url from (never a fetch: an om build runs
	// in that worktree, and a probe must not move its refs).
	omSkewRigWorktree = "om/mayor/rig"
	// omSkewRemoteRef is the branch the installed om is meant to come from.
	omSkewRemoteRef = "refs/heads/main"
)

// omSkewNonBinaryPaths are the paths whose shape says om's build does not read
// them: a commit that touches only these leaves the binary as it was, so the
// installed sha differing from origin/main over them is not a skew. A path in
// none of them (Go source, go.mod and go.sum, the build and install scripts,
// anything unknown) can change the binary and keeps the alert. A name ending in
// "/" is a directory prefix; any other is an exact path.
//
// Shape is not the whole answer: a //go:embed directive carries a file listed
// here into the binary whatever it looks like, so omSkewNoEmbedReaches checks
// every name on this list against the om source's directives before the list
// dismisses a skew (gt-8n9hj).
var omSkewNonBinaryPaths = []string{".om.json", "docs/", "README.md", "CHANGELOG.md", "LICENSE", ".gitignore"}

// omSkewMonitor is the check's state: the start guard, the open alert, and
// the last unreadable reading logged.
type omSkewMonitor struct {
	start sync.Once

	mu sync.Mutex
	// raised is true while an om-skew alert is open. It is what keeps the
	// hourly check from re-raising a condition the operator already holds.
	raised bool
	// settled is true once this process has closed any om-skew alert a
	// previous process left open. raised lives only in memory, so a fresh
	// daemon starts unraised whatever the town holds; the first matching
	// reading therefore clears once, and settled keeps every later one from
	// repeating it.
	settled bool
	// lastReadErr is the error last logged, so an offline town logs once per
	// change rather than once an hour.
	lastReadErr string
	// lastCloneErr is the message last logged about the clone failing to
	// answer a question past the two shas (the diff, the embed directives). It
	// is lastReadErr's suppression for those: a clone that cannot answer
	// repeats the same line every hour otherwise, and the log stops being a
	// record of what changed (gt-8n9hj).
	lastCloneErr string
}

// startOMSkewMonitor starts the check, once per daemon, from the landing
// worker manager (lagging a patrol is a per-daemon condition, not a per-rig
// one). It returns at once, so the manager's pass loop never waits on a
// reading.
func (d *Daemon) startOMSkewMonitor() {
	if d.ctx == nil {
		return
	}
	d.omSkew.start.Do(func() { go d.runOMSkewMonitor() })
}

// runOMSkewMonitor checks at start, then hourly, until the daemon stops.
func (d *Daemon) runOMSkewMonitor() {
	d.omSkewOnce()
	ticker := d.clk().NewTicker(omSkewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.Chan():
			d.omSkewOnce()
		}
	}
}

// omSkewOnce runs one check. It is the landing worker's check — om reviews
// landings — so it stands down while that patrol is off or its review is
// disabled, leaving the alert state as it found it.
func (d *Daemon) omSkewOnce() {
	if d.config == nil || !d.isPatrolActive("landing_worker") || !landingReviewEnabled(d.patrolConfig) {
		return
	}
	d.omSkewJudge(d.omSkewRead(d.config.TownRoot))
}

// omSkewReading is one comparison: the installed om's sha and the om rig's
// origin/main sha, or the error that stopped one of them being taken.
type omSkewReading struct {
	installed string
	remote    string
	// binaryUnchanged is true when the two shas differ but every file between
	// them is in omSkewNonBinaryPaths. It is false whenever that could not be
	// established: the alert fails toward alarming.
	binaryUnchanged bool
	err             error
}

// omSkewRead takes both readings, the installed side first: an om that cannot
// describe itself is a local condition, and a network read for a comparison
// that cannot happen is a wasted timeout.
func (d *Daemon) omSkewRead(townRoot string) omSkewReading {
	omPath := d.omSkewPath()
	installed, err := d.omSkewInstalledSHA(omPath)
	if err != nil {
		return omSkewReading{err: fmt.Errorf("the installed om at %s: %w", omPath, err)}
	}
	remote, err := d.omSkewRemoteSHA(townRoot)
	if err != nil {
		return omSkewReading{err: fmt.Errorf("the om rig's origin/main: %w", err)}
	}
	r := omSkewReading{installed: installed, remote: remote}
	if installed != remote {
		r.binaryUnchanged = d.omSkewBinaryUnchanged(townRoot, installed, remote)
	}
	return r
}

// omSkewBinaryUnchanged reports whether no file between the installed om and
// origin/main can change the binary. The diff runs in the rebuild_om job's own
// clone, read-only: never in om's canonical worktree, which an om build runs
// in. When the clone cannot answer (missing, unreadable, or without either
// sha) the answer is false, so the alert still fires.
func (d *Daemon) omSkewBinaryUnchanged(townRoot, installed, remote string) bool {
	unchanged, err := d.omSkewNoFileCanChangeTheBinary(townRoot, installed, remote)
	d.omSkewNoteCloneErr(err)
	return err == nil && unchanged
}

// omSkewNoFileCanChangeTheBinary answers the question in full: what the clone
// reports differs, and whether an embed directive reaches an exempt path among
// it. err is a question the clone could not answer, which leaves the caller
// alarming.
func (d *Daemon) omSkewNoFileCanChangeTheBinary(townRoot, installed, remote string) (bool, error) {
	changed, err := d.omSkewChangedPaths(townRoot, installed, remote)
	if err != nil {
		return false, fmt.Errorf("cannot list the files between %s and %s: %w",
			shortOMSHA(installed), shortOMSHA(remote), err)
	}
	for _, path := range changed {
		if !omSkewNonBinaryPath(path) {
			return false, nil
		}
	}
	return d.omSkewNoEmbedReaches(townRoot, remote, changed)
}

// omSkewNoteCloneErr records the clone's answer to the questions a reading asks
// of it. err nil is a clone that answered, which clears the suppression; an
// error is logged once per distinct message, since an hourly check over a clone
// that stays unreadable would otherwise write the same line an hour (gt-8n9hj).
func (d *Daemon) omSkewNoteCloneErr(err error) {
	m := &d.omSkew
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastCloneErr = ""
		return
	}
	msg := err.Error()
	if msg == m.lastCloneErr {
		return
	}
	m.lastCloneErr = msg
	d.logger.Printf("om_skew: %s; treating the skew as real", msg)
}

// omSkewNonBinaryPath reports whether path is in omSkewNonBinaryPaths.
func omSkewNonBinaryPath(path string) bool {
	for _, p := range omSkewNonBinaryPaths {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(path, p) {
				return true
			}
		} else if path == p {
			return true
		}
	}
	return false
}

// omSkewEmbedRule is one //go:embed pattern and the directory of the Go file
// naming it. A pattern is relative to that directory and may not leave it, so
// only a file at or under the directory can be carried in.
type omSkewEmbedRule struct {
	dir     string // slash-separated, "" at the tree root
	pattern string // as written, with any all: prefix removed
}

// omSkewNoEmbedReaches reports whether the om source at rev's //go:embed
// directives leave every one of paths out of the binary, and a tree whose
// directives cannot be read is an error rather than a yes: an allowlisted path
// is only dismissed once the tree has been asked about it.
func (d *Daemon) omSkewNoEmbedReaches(townRoot, rev string, paths []string) (bool, error) {
	if len(paths) == 0 {
		return true, nil
	}
	rules, err := d.omSkewEmbedRules(townRoot, rev)
	if err != nil {
		return false, fmt.Errorf("cannot read the om source's embed directives: %w", err)
	}
	for _, p := range paths {
		for _, r := range rules {
			if omSkewEmbedRuleCovers(r, p) {
				return false, nil
			}
		}
	}
	return true, nil
}

// omSkewEmbedRules reads the //go:embed directives the om source at rev
// carries. A test source is compiled into no om, so *_test.go names nothing;
// every other directive counts, and a rule this reads too broadly makes the
// alert fire rather than a skew be dismissed.
func (d *Daemon) omSkewEmbedRules(townRoot, rev string) ([]omSkewEmbedRule, error) {
	if err := omSkewCommitRevs(rev); err != nil {
		return nil, err
	}
	src, err := omSkewCloneDir(townRoot)
	if err != nil {
		return nil, err
	}
	// -n prints each match's file and line number, which is how a pattern
	// learns the directory it is relative to. A tree carrying no directive is
	// git grep's exit 1, an answer and not a failure.
	out, err := d.omSkewGitNoMatch("-C", src, "grep", "-n", "-F", "-e", "//go:embed", rev, "--", "*.go")
	if err != nil {
		return nil, err
	}
	return parseOMSkewEmbedRules(rev, out)
}

// parseOMSkewEmbedRules reads the answer to the search above. Searching a
// revision prefixes every line with that revision as it was written —
// "<rev>:<file>:<line>:<content>" — and the prefix has to come off before the
// fields are read: with it left on, the sha is taken for the file name and no
// directive is ever recognized, so the allowlist dismisses the skew a directive
// says is real (gt-8n9hj). A non-empty line without the prefix is an output
// shape this does not know, and a shape it cannot read alarms rather than
// dismisses.
func parseOMSkewEmbedRules(rev, out string) ([]omSkewEmbedRule, error) {
	prefix := rev + ":"
	var rules []omSkewEmbedRule
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			return nil, fmt.Errorf("git grep printed %q, which does not carry the revision %s it searched", line, rev)
		}
		// The line number sits between the file and the directive. All three
		// fields are required and the middle one is that number: a line whose
		// fields this cannot tell apart (a path carrying a colon) alarms rather
		// than passing the directive over.
		fields := strings.SplitN(rest, ":", 3)
		if len(fields) != 3 || !omSkewLineNumber(fields[1]) {
			return nil, fmt.Errorf("git grep printed %q, which carries no <file>:<line>:<content>", line)
		}
		file, content := fields[0], fields[2]
		patterns, ok := strings.CutPrefix(strings.TrimSpace(content), "//go:embed")
		// A line the search matched but this prefix does not confirm (a comment
		// about go:embed, or "//go:embedding") names no pattern.
		if !ok || (patterns != "" && patterns[0] != ' ' && patterns[0] != '\t') || strings.HasSuffix(file, "_test.go") {
			continue
		}
		dir := path.Dir(file)
		if dir == "." {
			dir = ""
		}
		for _, p := range omSkewEmbedPatterns(patterns) {
			rules = append(rules, omSkewEmbedRule{dir: dir, pattern: p})
		}
	}
	return rules, nil
}

// omSkewLineNumber reports whether s is the line number git grep -n prints.
func omSkewLineNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// omSkewEmbedPatterns returns one directive's patterns with any all: prefix
// removed. A pattern may be quoted and hold a space, so the split is not on
// whitespace alone.
func omSkewEmbedPatterns(rest string) []string {
	var out []string
	for rest = strings.TrimLeft(rest, " \t\r"); rest != ""; rest = strings.TrimLeft(rest, " \t\r") {
		var p string
		if q := rest[0]; q == '"' || q == '`' {
			end := strings.IndexByte(rest[1:], q)
			if end < 0 {
				break // an unterminated quote names no pattern
			}
			p, rest = rest[1:1+end], rest[2+end:]
		} else if i := strings.IndexAny(rest, " \t\r"); i < 0 {
			p, rest = rest, ""
		} else {
			p, rest = rest[:i], rest[i:]
		}
		if p = strings.TrimPrefix(p, "all:"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// omSkewEmbedRuleCovers reports whether r can carry p into the binary. go:embed
// matches a pattern against the package's own tree and a pattern that names a
// directory carries its whole subtree, so p is covered when the pattern matches
// it or any directory above it. A directory pattern's own exclusions (a name
// beginning with "." or "_", embedded only under all:) are not applied: they
// cost an alert, never a dismissed skew.
func omSkewEmbedRuleCovers(r omSkewEmbedRule, p string) bool {
	rel := p
	if r.dir != "" {
		var ok bool
		if rel, ok = strings.CutPrefix(p, r.dir+"/"); !ok {
			return false
		}
	}
	for dir := rel; dir != ""; {
		if matched, err := path.Match(r.pattern, dir); err == nil && matched {
			return true
		}
		i := strings.LastIndexByte(dir, '/')
		if i < 0 {
			break
		}
		dir = dir[:i]
	}
	return false
}

// omSkewCloneDir is the rebuild_om job's own clone, the only checkout this
// check reads: an om build runs in the rig's worktree, which a probe must not
// touch.
func omSkewCloneDir(townRoot string) (string, error) {
	src := filepath.Join(townRoot, filepath.FromSlash(rebuildOMSrcDir))
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("the rebuild_om clone: %w", err)
	}
	return src, nil
}

// omSkewCommitRevs holds each revision to a commit id before it reaches git's
// argv, so a value that looks like an option is refused rather than read as
// one. Every function here that hands git a revision calls this first: the
// guard covers the calls, not one call (gt-8n9hj).
func omSkewCommitRevs(revs ...string) error {
	for _, rev := range revs {
		if !isCommitSHA(rev) {
			return fmt.Errorf("%q is not a commit id to hand git", rev)
		}
	}
	return nil
}

// omSkewChangedPaths lists the files that differ between the installed om's
// commit and origin/main's, from the rebuild_om clone. Renames are not
// detected, so a file moved out of Go source is listed under both names.
func (d *Daemon) omSkewChangedPaths(townRoot, installed, remote string) ([]string, error) {
	src, err := omSkewCloneDir(townRoot)
	if err != nil {
		return nil, err
	}
	// Both revisions reach git's argv, so each is held to a commit id: a value
	// that looks like an option would otherwise be read as one, and the diff
	// deciding the alert would be a diff of something else. The "--" marks the
	// same boundary from git's side (gt-8n9hj).
	if err := omSkewCommitRevs(installed, remote); err != nil {
		return nil, err
	}
	out, err := d.omSkewGit("-C", src, "diff", "--name-only", "-z", "--no-renames", installed, remote, "--")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// omSkewJudge applies one reading to the alert state: a mismatch raises
// om-skew once, an equal pair, or a differing one whose files cannot change
// the binary, clears an open one (one a previous daemon process left open
// too: this process's first such reading clears once), and a reading that could not
// be taken does neither — an unreadable om or an unreachable remote is not
// evidence that a skew ended.
func (d *Daemon) omSkewJudge(r omSkewReading) {
	m := &d.omSkew
	m.mu.Lock()
	defer m.mu.Unlock()

	if r.err != nil {
		msg := r.err.Error()
		if msg == m.lastReadErr {
			return
		}
		m.lastReadErr = msg
		d.logger.Printf("om_skew: %s; leaving the alert state as it is", msg)
		return
	}
	m.lastReadErr = ""

	if r.installed == r.remote || r.binaryUnchanged {
		if !m.raised && m.settled {
			return
		}
		if err := d.clearAlertsErr("the installed om is current with the om rig's origin/main: nothing between them changes the binary", omSkewAlertKey); err != nil {
			// The clear never reached the town: stay raised so the next check
			// retries it, rather than leaving an alert nothing will close.
			d.logger.Printf("om_skew: clearing the alert did not reach the town; retrying next check: %v", err)
			return
		}
		m.raised = false
		m.settled = true
		return
	}

	if m.raised {
		return
	}
	m.raised = true
	if err := d.escalateAlertSeverity("MEDIUM", omSkewAlertKey, omSkewSource, omSkewMessage(r.installed, r.remote)); err != nil {
		// The escalation never reached the town: drop back to unraised so the
		// next check retries rather than record an alert nobody received.
		d.logger.Printf("om_skew: the escalation did not reach the town; retrying next check: %v", err)
		m.raised = false
	}
}

// omSkewMessage names both shas short: the operator reads them against `om
// version` and a git log, and the alert is a pointer to a condition, not a
// log line.
func omSkewMessage(installed, remote string) string {
	return fmt.Sprintf("the installed om is %s but the om rig's origin/main is %s: landings run the older binary against the flags and gates of the newer one; install om from %s to clear this",
		shortOMSHA(installed), shortOMSHA(remote), omSkewRigWorktree)
}

// shortOMSHA is the first 8 characters of a commit sha, for a message a human
// matches by eye.
func shortOMSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// omSkewPath is the om the landing worker reviews with (resolveOMPath): the
// configured om, else om on the daemon's PATH, else the go install location.
func (d *Daemon) omSkewPath() string {
	configured := ""
	if cfg := landingWorkerConfig(d.patrolConfig); cfg != nil {
		configured = cfg.OMPath
	}
	home, _ := os.UserHomeDir()
	return resolveOMPath(configured, exec.LookPath, home)
}

// omSkewInstalledSHA reads the installed om's version. om prints one
// "om version <sha>" line; output without it is a reading that could not be
// taken, which is not a skew.
func (d *Daemon) omSkewInstalledSHA(omPath string) (string, error) {
	if d.omVersionFn != nil {
		return d.omVersionFn(omPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), omSkewVersionTimeout)
	defer cancel()
	out, err := d.combinedOutput(exec.CommandContext(ctx, omPath, "version"))
	if err != nil {
		return "", fmt.Errorf("%s version: %w", omPath, err)
	}
	sha, err := parseOMVersion(string(out))
	if err != nil {
		return "", fmt.Errorf("%s version: %w", omPath, err)
	}
	return sha, nil
}

// parseOMVersion returns the sha on om's "om version <sha>" line. The line
// may carry trailing text, so the sha is its first field.
func parseOMVersion(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "om version ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		if !isCommitSHA(fields[0]) {
			break
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("no \"om version <sha>\" line in %q", strings.TrimSpace(out))
}

// omSkewRemoteSHA reads the om rig's origin/main tip: the url from the rig's
// own origin remote, the tip from an ls-remote against that url. The read is
// never a fetch — the om rig's worktree is what an om build runs in, and a
// probe must not move its refs.
func (d *Daemon) omSkewRemoteSHA(townRoot string) (string, error) {
	if d.omHeadFn != nil {
		return d.omHeadFn(townRoot)
	}
	rigPath := filepath.Join(townRoot, filepath.FromSlash(omSkewRigWorktree))
	url, err := d.omSkewGit("-C", rigPath, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("git -C %s remote get-url origin: no url", rigPath)
	}
	out, err := d.omSkewGit("ls-remote", url, omSkewRemoteRef)
	if err != nil {
		return "", err
	}
	sha := parseLSRemoteMain(out)
	if sha == "" {
		return "", fmt.Errorf("git ls-remote %s %s: no tip", url, omSkewRemoteRef)
	}
	return sha, nil
}

// omSkewGitRun runs one read-only git call, bounded by omSkewRemoteTimeout,
// and returns its trimmed stdout with the command's own error: the status
// omSkewGitNoMatch reads is read where exec produces it, not off a wrapped
// error another function builds (gt-8n9hj).
func (d *Daemon) omSkewGitRun(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), omSkewRemoteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	// A background probe must never sit on a credential prompt: the daemon has
	// no terminal to answer one, so the timeout would spend itself waiting.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := d.combinedOutput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// omSkewGit is omSkewGitRun for the calls whose non-zero exit is a failure,
// named in the error so a log line says which read it was.
func (d *Daemon) omSkewGit(args ...string) (string, error) {
	out, err := d.omSkewGitRun(args...)
	return out, omSkewGitErr(args, err)
}

// omSkewGitNoMatch is omSkewGitRun for the calls whose exit 1 is an answer: git
// grep found no line, which is a tree carrying no directive rather than a
// question the clone could not answer.
func (d *Daemon) omSkewGitNoMatch(args ...string) (string, error) {
	out, err := d.omSkewGitRun(args...)
	var coded interface{ ExitCode() int }
	if err != nil && errors.As(err, &coded) && coded.ExitCode() == 1 {
		return "", nil
	}
	return out, omSkewGitErr(args, err)
}

// omSkewGitErr names the call in a failure, and is nil for a call that
// succeeded.
func omSkewGitErr(args []string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// parseLSRemoteMain returns the sha ls-remote printed for refs/heads/main,
// and "" when it printed none. The ref is matched whole, so a branch named
// refs/heads/main-old is not the tip.
func parseLSRemoteMain(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != omSkewRemoteRef {
			continue
		}
		// A first field that is not a commit id is a malformed answer, not a
		// tip: returning it would raise a skew against garbage.
		if !isCommitSHA(fields[0]) {
			return ""
		}
		return fields[0]
	}
	return ""
}

// isCommitSHA reports whether s is hex and long enough to be a git object id.
func isCommitSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
