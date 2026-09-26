// Package cmd — polecat-side pre-work duplicate check (gt-csng).
//
// The third detection point in the same-defect double-dispatch trilogy,
// alongside the sling-time content dedupe (sling_duplicate.go, gt-mcq) and the
// post-hoc patch-id equivalence check. Where the sling-time check matches bead
// text against bead text, this one routes through FILES, not vocabulary: it
// compares the paths named in the polecat's hooked bead against recent
// origin/main history. Two beads describing one defect share no keywords —
// that is the whole vantage-point mechanism — but they always share files.
//
// It runs at gt prime, BEFORE the session is spent, needs no index, and costs
// two local git commands — a ref check that origin/main exists, then one day of
// its history for the bead's paths. Both go through runPrimeExternalCommand, so
// they share prime's external-tool deadline and process group; a git that hangs
// is abandoned rather than waited out. Output is deliberately terse (a few
// lines); a prime payload is already long, so the check degrades to silence on
// any failure rather than growing it.

package cmd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// dupesRecentCommits bounds how many recent commits per file are quoted in the
// warning. Three is enough to see the shape of the change that just landed;
// more would spend prime budget for no signal.
const dupesRecentCommits = 3

// dupesRecentFiles bounds how many of the bead's named paths are checked and
// reported. A bead that names a dozen paths is describing an area, not a
// defect; three is where the warning stops reading as a diagnosis.
const dupesRecentFiles = 3

var dupesCommitLineRe = regexp.MustCompile(`^([0-9a-f]{7,40})\t(.*)$`)

type dupesCommit struct {
	Hash    string
	Subject string
	Files   map[string]bool
}

func (c dupesCommit) touches(files []string) bool {
	for _, f := range files {
		if c.Files[f] {
			return true
		}
	}
	return false
}

// dupesWarn reports whether a recent commit of a shared file also mentions a
// test name the bead names — the shape of the instance this check exists for
// (opal's bead named cmd/gt/hermetic_main_test.go, and pearl's 279a4bd that
// fixed the defect had already landed on main when opal started).
func (c dupesCommit) dupesWarn(tests []string, files []string) bool {
	if !c.touches(files) {
		return false
	}
	subj := c.Subject
	for _, t := range tests {
		t = strings.TrimRight(t, "_")
		if t != "" && strings.Contains(subj, t) {
			return true
		}
	}
	return false
}

// checkHookedPathDupes runs the polecat-side pre-work duplicate check for a
// fresh session (skips continuation mode, where the continuation directive
// replaces the autonomous block this warning rides, and dry-run, where no
// subprocess may run). Every failure path returns without printing: the check
// must never block or bloat a prime.
func checkHookedPathDupes(ctx RoleContext, hookedBead *beads.Issue) {
	if primeContinuationMode || primeDryRun {
		return
	}
	if ctx.Role != RolePolecat || hookedBead == nil || hookedBead.ID == "" {
		return
	}

	refs := extractContentRefs(hookedBead.Title, hookedBead.Description, hookedBead.Design, hookedBead.Notes)
	if len(refs.Files) == 0 {
		return
	}
	files := refs.Files
	if len(files) > dupesRecentFiles {
		files = files[:dupesRecentFiles]
	}

	commits, err := dupesRecentLog(ctx.WorkDir, files)
	if err != nil {
		return
	}

	var hits []dupesCommit
	for _, c := range commits {
		if c.touches(files) {
			hits = append(hits, c)
			if len(hits) >= dupesRecentCommits {
				break
			}
		}
	}
	if len(hits) == 0 {
		return
	}

	shared := make([]string, 0, len(hits))
	for _, c := range hits {
		for _, f := range files {
			if c.Files[f] && !listContains(shared, f) {
				shared = append(shared, f)
			}
		}
	}

	var b strings.Builder
	warning := style.Warning.Render("⚠")
	fmt.Fprintf(&b, "%s Your bead names %d path(s) touched on origin/main in the last day — check before duplicating this work.\n", warning, len(shared))
	for _, f := range shared {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	for _, c := range hits {
		stronger := ""
		if c.dupesWarn(refs.Tests, files) {
			stronger = " — commit mentions a test name your bead names"
		}
		fmt.Fprintf(&b, "  %s %s%s\n", c.Hash, c.Subject, stronger)
	}
	fmt.Fprintln(&b, "  If the fix above already covers this bead, close it no-changes; otherwise proceed and build on it.")
	fmt.Println(b.String())
}

// dupesRecentLog reads a day of origin/main history naming the given paths.
// The day cap keeps this O(churn) for any worktree, fresh or stale; the cap
// is the point, not the exactness. The commands run in workDir as Dir, the
// polecat's own worktree — no getGitRoot call, which resolves relative to the
// gt binary's cwd and would read the wrong repo.
//
// Both commands go through runPrimeExternalCommand, which is what keeps this
// check from outliving its welcome: prime's external-tool deadline bounds each
// one and its process group makes a canceled git killable, exactly as for the
// bd and mail injections. Any error — workDir outside a repo, no origin/main,
// git failing or hitting the deadline — is returned so the caller can degrade
// to silence.
func dupesRecentLog(workDir string, files []string) ([]dupesCommit, error) {
	if _, _, err := runPrimeExternalCommand(workDir, "git", "rev-parse", "--verify", "--quiet", "origin/main"); err != nil {
		return nil, fmt.Errorf("origin/main: %w", err)
	}

	args := []string{"log", "origin/main", "--since=1 day", "--name-only", "--pretty=format:%h%x09%s", "--"}
	args = append(args, files...)
	stdout, _, err := runPrimeExternalCommand(workDir, "git", args...)
	if err != nil {
		return nil, err
	}
	out := stdout.String()

	var commits []dupesCommit
	var cur *dupesCommit
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := dupesCommitLineRe.FindStringSubmatch(line); m != nil {
			if cur != nil {
				commits = append(commits, *cur)
			}
			cur = &dupesCommit{Hash: m[1], Subject: m[2], Files: map[string]bool{}}
			continue
		}
		if cur != nil && line != "" {
			cur.Files[normalizeFileTokenPath(line)] = true
		}
	}
	if cur != nil {
		commits = append(commits, *cur)
	}
	return commits, nil
}

// normalizeFileTokenPath canonicalizes a path straight from git log
// --name-only (it may be absolute-looking or "./"-prefixed by quoting rules)
// to the same repo-relative form normalizeFileToken produces for prose
// tokens, so both sides of the comparison share one spelling.
func normalizeFileTokenPath(p string) string {
	segments := strings.Split(strings.TrimSpace(p), "/")
	for len(segments) > 1 && (segments[0] == "." || segments[0] == ".." || segments[0] == "") {
		segments = segments[1:]
	}
	for len(segments) > 1 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	return strings.Join(segments, "/")
}

func listContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
