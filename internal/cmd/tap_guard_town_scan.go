package cmd

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/workspace"
)

// This file extends the dangerous-command guard's unbounded-scan rule
// (matchesUnboundedScan, tap_guard_dangerous.go) from the host's broad
// filesystem roots to the Gas Town tree itself (gt-6e2l).
//
// The incident: a dog session ran `grep -R "record-run" -n /Users/sloan/gt`
// — a recursive scan of the town root, which holds every rig, every
// .repo.git, and every polecat/crew worktree. The existing scanRootDenylist
// covered / and $HOME but not the town, so the command executed: >64 MB of
// output persisted and a 15-minute load average of 17. The cost is not the
// grep itself — it is that the tree below the town root is tens of gigabytes
// of git objects and duplicate checkouts, so walking it pegs the host for
// minutes.
//
// The town is hazardous at two levels:
//
//   - the town root itself, and any path directly under it. A rig root is
//     the town's biggest single tree (that rig's bare repo plus all of its
//     worktrees), and the town's other level-1 directories are either large
//     (.dolt-data, logs) or not worth a recursive scan. Rigs are recognized
//     structurally rather than by reading mayor/rigs.json, which a guard
//     running on every Bash tool call should not depend on — and which is
//     missing or stale often enough to matter (the live-fire probe for this
//     bead ran against a town with no rigs.json at all);
//   - a rig's aggregate directories — polecats/, crew/, refinery/,
//     witness/, mayor/. One level deeper, but each holds *many* checkouts
//     rather than one, so scanning "the rig's polecats/" walks every polecat
//     worktree on the host: the town-root hazard at a smaller scale.
//
// Anything deeper than that is inside a single repository or a single
// worktree — a bounded subtree, and scanning it is an ordinary thing to do.
// The boundary is deliberate: grep -rn foo ~/gt/<rig>/polecats/<name>/<repo>
// (one checkout) is allowed; grep -r foo ~/gt/<rig>/polecats (all of them)
// is not.

// rigAggregateDirs lists the rig subdirectories that hold more than one
// checkout, so a recursive scan rooted at one of them walks many repos at
// once (every polecat worktree, every crew workspace). Derived from
// rig.AgentDirs — the canonical rig layout (internal/rig/types.go) — by
// taking each entry's first path component, so a layout change there (a new
// aggregate directory) reaches this guard without a second list to keep in
// sync.
//
// A rig's deeper clone directories — mayor/rig, refinery/rig, and a single
// polecats/<name>/<repo> — are each one repository and stay scannable.
func rigAggregateDirs() map[string]bool {
	dirs := make(map[string]bool, len(rig.AgentDirs))
	for _, d := range rig.AgentDirs {
		first, _, _ := strings.Cut(d, "/")
		if first != "" {
			dirs[first] = true
		}
	}
	return dirs
}

// bareRepoDir is the name of the bare repository a rig keeps its worktrees
// off of (~/gt/<rig>/.repo.git). Never useful to grep, and large enough that
// walking it is a hazard on its own, so it is denied at any depth.
const bareRepoDir = ".repo.git"

// currentTownRoot resolves the town root for scan-guard purposes, or "" when
// this guard is not running inside a Gas Town workspace (a standalone repo
// checkout, a crew session outside the town, ...). An empty result means the
// guard falls back to its host-root rule alone, exactly as before.
//
// Resolution goes through internal/workspace — the same walk-up-from-cwd plus
// GT_TOWN_ROOT/GT_ROOT fallback every other gt command uses — which also
// means a test harness running with GT_TEST_FORBIDDEN_TOWN_ROOT set never
// picks up the operator's live town.
//
// Cost: this runs on every Bash tool call, so it is a handful of stat(2)
// calls up the directory tree — no readdir, no rigs.json parse. That is
// noise beside the Go process spawn the hook already pays for.
func currentTownRoot() string {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return ""
	}
	return townRoot
}

// townScanHazard reports whether a recursive scan rooted at scanRoot would
// walk a town-aggregate tree (see this file's header for the two levels),
// returning a short label for the block banner — "the town root", "a rig
// root", "a rig's worktree dir", "a .repo.git bare repo" — or "" when
// scanning scanRoot is fine. The labels are sized to fit the banner's
// 53-character reason field alongside the longest tool name ("grep").
//
// scanRoot must already be an absolute, cleaned path (see scanRootPath).
// townRoot may be "", meaning "no town context": nothing is a hazard.
func townScanHazard(scanRoot, townRoot string) string {
	if scanRoot == "" || townRoot == "" {
		return ""
	}
	// Compared case-insensitively on purpose: the operator's host is macOS,
	// whose default filesystem is case-insensitive, so /Users/sloan/GT and
	// /Users/sloan/gt name the same tree and a case-sensitive compare would
	// leave an easy way around this rule. On a case-sensitive filesystem the
	// only effect is a redundant block for a path differing from a hook or
	// aggregate-directory name in case alone, which is the safe direction to
	// err — and matches how scanRootDenylist already folds case for the
	// host roots.
	town := strings.ToLower(filepath.Clean(townRoot))
	root := strings.ToLower(filepath.Clean(scanRoot))

	if root == town {
		return "the town root"
	}

	rel, err := filepath.Rel(town, root)
	if err != nil || !isWithinRel(rel) {
		return "" // outside the town — only the host-root rule applies
	}

	if filepath.Base(root) == bareRepoDir {
		return "a " + bareRepoDir + " bare repo"
	}

	// rel is root's path below the town root: "<rig>" at level 1,
	// "<rig>/polecats" at level 2, and so on.
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 1 {
		return "a rig root"
	}
	if len(parts) == 2 && rigAggregateDirs()[filepath.Base(root)] {
		return "a rig's worktree dir"
	}
	return ""
}

// isWithinRel reports whether a filepath.Rel result names a path strictly
// inside the reference directory — not "..", not an "../..." escape, and not
// the reference directory itself (callers check that case separately).
func isWithinRel(rel string) bool {
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel) && rel != "." && rel != ""
}

// scanRootPath resolves a scan-root argument token to the absolute path it
// names, so it can be compared against the town tree: a leading ~ (or
// $HOME / ${HOME}, bare or with a path) expands to the home directory, and a
// relative path resolves against the guard's working directory — the
// session's cwd, which is also what the town-root lookup above walks up
// from.
//
// Returns "" when the token names nothing this guard can resolve: a flag
// ("-name", "-maxdepth"), or a token still containing "$" after shell
// variable assignments were resolved (an environment variable this process
// cannot see). Guessing a value there is how a guard starts blocking
// innocent commands, so an unresolvable token never blocks.
//
// A relative token counts as a path only when it names an existing
// directory. Most relative arguments to a scan are not paths at all — "TODO"
// is the search pattern in `grep -rn TODO /var/log`, "-name" is a flag, and
// ".env" is a pattern in `grep -rn .env src/` — so resolving them against cwd
// would let a pattern that happens to collide with a real entry classify the
// command as a scan of that entry. That is not hypothetical: a guard whose
// cwd is the town root would block `grep -rn .env /var/log` the moment the
// town has a .env file, which it does. Absolute tokens are left alone
// (a path spelled in full is unambiguous, and `find ~/gt/* -name x` must
// still classify), and glob tokens are exempt from the existence check
// because they cannot be stat'd — the shell turns them into many roots
// before the walker ever runs, which is exactly the case worth blocking.
// Existence is one stat(2), never a walk.
// isHomeDirScanRoot reports whether a resolved scan root IS the home
// directory. scanRootDenylist catches the shell spellings (~, $HOME, /Users),
// but an agent that writes the expanded path (/Users/me) has named the same
// root; a dog did exactly that on 2026-09-18 (`find /Users/sloan -maxdepth 3
// -name .git`) and walked Documents/Desktop/Music, firing macOS privacy
// prompts at the operator. Only the exact home path is denied — /Users/me/
// project stays bounded, consistent with the rest of the denylist.
func isHomeDirScanRoot(resolved string) bool {
	if resolved == "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	// Case-insensitive filesystems (macOS default) and a $HOME that runs
	// through a symlink (/var -> /private/var) both name the same directory
	// under a different spelling; compare the canonical forms.
	canon := func(p string) string {
		p = filepath.Clean(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return p
	}
	return strings.EqualFold(canon(resolved), canon(home))
}

func scanRootPath(token string) string {
	if token == "" || strings.HasPrefix(token, "-") {
		return ""
	}

	p, ok := expandHomePath(token)
	// An unknown home directory, or a variable this process cannot resolve,
	// leaves a literal "$..." behind — both unresolvable.
	if !ok || p == "" || strings.Contains(p, "$") {
		return ""
	}

	if !filepath.IsAbs(p) {
		if !hasGlobMeta(p) {
			st, err := os.Stat(p)
			if err != nil || !st.IsDir() {
				return ""
			}
		}
		wd, err := os.Getwd()
		if err != nil {
			return "" // a deleted worktree: unresolvable, never "the root"
		}
		p = filepath.Join(wd, p)
	}
	return filepath.Clean(p)
}

// hasGlobMeta reports whether a token carries shell glob syntax, in which
// case it names a set of paths rather than one and cannot be stat'd.
func hasGlobMeta(token string) bool {
	return strings.ContainsAny(token, "*?[")
}

// scanPatternPath is scanRootPath for the argument a scan tool reads as its
// search pattern, which is not a path merely because a directory of that name
// exists at cwd (gt-yts7).
//
// A bare relative name is the collision that filed gt-yts7: `grep -rn polecats
// ./docs` searches ./docs, but a polecats/ directory at cwd made the pattern
// read as a scan of it. So a bare name resolves to nothing here. Every
// spelling that names a path by its form — absolute, ~/$HOME, a glob, or a
// relative path carrying a separator or a . / .. segment — resolves exactly
// as scanRootPath resolves it, which keeps the denylist, home-directory and
// town-tree rules judging a pattern that is one of their roots.
func scanPatternPath(token string) string {
	if token == "" || strings.HasPrefix(token, "-") {
		return ""
	}
	expanded, ok := expandHomePath(token)
	if !ok || expanded == "" || strings.Contains(expanded, "$") || !isPathSpelling(expanded) {
		return ""
	}
	return scanRootPath(token)
}

// isPathSpelling reports whether token names a path by its form alone: an
// absolute path, a glob, or a relative path with a separator or a . / ..
// segment. A bare relative name is not one — that is the shape a search
// pattern and a cwd-relative directory share.
func isPathSpelling(token string) bool {
	if token == "." || token == ".." {
		return true
	}
	return filepath.IsAbs(token) || hasGlobMeta(token) || strings.ContainsRune(token, '/')
}

// expandHomePath applies the shell's ~ / $HOME / ${HOME} prefix rules to a
// token, returning it unchanged when it names no home-relative path. ok is
// false only when the token *did* reference the home directory but this
// process cannot see it — the caller must not then treat the leftover text
// as a relative path, which is how "~/gt" would silently become "<cwd>/gt".
func expandHomePath(token string) (path string, ok bool) {
	join := func(rest string) (string, bool) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		return filepath.Join(home, rest), true
	}

	switch {
	case token == "~", token == "$HOME", token == "${HOME}":
		return join("")
	case strings.HasPrefix(token, "~/"):
		return join(strings.TrimPrefix(token, "~/"))
	case strings.HasPrefix(token, "$HOME/"):
		return join(strings.TrimPrefix(token, "$HOME/"))
	case strings.HasPrefix(token, "${HOME}/"):
		return join(strings.TrimPrefix(token, "${HOME}/"))
	}
	return token, true
}
