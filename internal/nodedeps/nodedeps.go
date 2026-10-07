// Package nodedeps compares a tree's installed node_modules against its
// package-lock.json, without running npm.
//
// The comparison exists because a worktree cut before a lockfile change
// landed answers lint with the versions its setup installed: its lint passes
// where the landing gate, which installs from the lockfile, fails (gt-wd12s).
// Only an npm tree is compared — one whose lockfile is pnpm-lock.yaml or
// yarn.lock carries no package-lock.json to compare — and everything here is
// a report for a caller to phrase, never a verdict.
package nodedeps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Mismatch is one package whose installed copy disagrees with what the tree's
// package-lock.json pins for it.
type Mismatch struct {
	// Path is the lockfile's own key for the package: where it is installed,
	// relative to the tree (node_modules/foo, node_modules/a/node_modules/b,
	// packages/web/node_modules/bar).
	Path string
	// Installed is the version the package's own package.json reports, and ""
	// when nothing is installed at Path (Missing).
	Installed string
	// Locked is the version the lockfile pins for Path.
	Locked string
	// Missing is true when the lockfile requires Path at the tree's own
	// node_modules and nothing installed there reports a version.
	Missing bool
}

// Result is one comparison of a tree's installed packages against its
// lockfile: what disagrees, and what the comparison has no verdict on.
type Result struct {
	// Mismatches are the packages that disagree with the lockfile, sorted by
	// Path.
	Mismatches []Mismatch
	// NotCompared are the lockfile paths whose package.json could not be read
	// or carries no version, sorted: no verdict was reached on them.
	NotCompared []string
}

// Check compares dir's installed node_modules against dir's package-lock.json
// and reports what disagrees; a tree with no package-lock.json or no
// node_modules has nothing installed to disagree and reports nothing, and a
// non-nil error means the comparison did not happen rather than that the tree
// agrees.
func Check(dir string) (Result, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading package-lock.json: %w", err)
	}
	st, err := os.Stat(filepath.Join(dir, "node_modules"))
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("checking node_modules: %w", err)
	}
	if !st.IsDir() {
		// A file named node_modules holds no installed package to compare.
		return Result{}, nil
	}
	pins, err := lockedPins(data)
	if err != nil {
		return Result{}, err
	}
	paths := make([]string, 0, len(pins))
	for p := range pins {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var res Result
	for _, p := range paths {
		rel := filepath.FromSlash(p)
		if !filepath.IsLocal(rel) {
			// A path that escapes the tree names no installed package, and
			// the lockfile is repo content: read nothing outside dir.
			continue
		}
		pin := pins[p]
		installed, err := installedVersion(filepath.Join(dir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Nothing is installed at Path. The pin says whether a fresh
			// install would put one there.
			if pin.required() && isTopLevelInstall(p) {
				res.Mismatches = append(res.Mismatches, Mismatch{Path: p, Locked: pin.Version, Missing: true})
			}
		case err != nil:
			res.NotCompared = append(res.NotCompared, p)
		case installed == "":
			res.NotCompared = append(res.NotCompared, p)
		case installed != pin.Version:
			res.Mismatches = append(res.Mismatches, Mismatch{Path: p, Installed: installed, Locked: pin.Version})
		}
	}
	return res, nil
}

// isTopLevelInstall reports whether a lockfile path is a package at a tree's
// own node_modules (node_modules/foo), not a nested duplicate and not a
// workspace's private install. Absence is reported only there: a nested
// duplicate can vanish when npm dedupes, while a fresh install always writes
// the tree's own copy.
func isTopLevelInstall(p string) bool {
	rest, ok := strings.CutPrefix(p, "node_modules/")
	return ok && !strings.Contains(rest, "/node_modules/")
}

// installedVersion is the version the package installed at pkgDir reports, or
// "" when its package.json carries none: a package with no version is nothing
// this comparison can call stale.
func installedVersion(pkgDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", err
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", fmt.Errorf("parsing %s: %w", filepath.Join(pkgDir, "package.json"), err)
	}
	if !isVersion(pkg.Version) {
		return "", nil
	}
	return pkg.Version, nil
}

// pin is one lockfile entry: the version it pins and what npm is free to
// leave out of an install.
type pin struct {
	Version  string
	Dev      bool `json:"dev"`
	Optional bool `json:"optional"`
	Peer     bool `json:"peer"`
	Bundled  bool `json:"inBundle"`
	Link     bool `json:"link"`
}

// required reports whether npm installs this pin whatever the install's own
// flags are. A dev, optional or peer package is one npm may omit on purpose,
// a bundled one ships inside its parent, and a link is a symlink to a
// workspace directory rather than an installed package.
func (p pin) required() bool {
	return !p.Dev && !p.Optional && !p.Peer && !p.Bundled && !p.Link
}

// lockedPins maps each install path a lockfile names to its pin: from the
// "packages" map of a lockfileVersion 2 or 3 lockfile, or, for the
// lockfileVersion 1 form that has no such map, from the installed tree its
// "dependencies" record.
func lockedPins(data []byte) (map[string]pin, error) {
	var lf struct {
		Packages map[string]pin `json:"packages"`
		// A lockfileVersion 1 dependency records "bundled" where 2 and 3
		// record "inBundle".
		Dependencies map[string]lockDepV1 `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parsing package-lock.json: %w", err)
	}
	pins := map[string]pin{}
	if len(lf.Packages) > 0 {
		for path, p := range lf.Packages {
			if !isInstallPath(path) || !isVersion(p.Version) {
				continue
			}
			pins[path] = p
		}
		return pins, nil
	}
	collectV1(lf.Dependencies, "", pins)
	return pins, nil
}

// isInstallPath reports whether a lockfile key names a package installed under
// a node_modules directory — the tree's own (node_modules/foo) or a
// workspace's (packages/web/node_modules/bar). A key naming a workspace or the
// root is not one.
func isInstallPath(p string) bool {
	segments := strings.Split(p, "/")
	return len(segments) > 1 && slices.Contains(segments[:len(segments)-1], "node_modules")
}

// lockDepV1 is one node of a lockfileVersion 1 lockfile's dependency tree.
type lockDepV1 struct {
	Version      string               `json:"version"`
	Dev          bool                 `json:"dev"`
	Optional     bool                 `json:"optional"`
	Bundled      bool                 `json:"bundled"`
	Dependencies map[string]lockDepV1 `json:"dependencies"`
}

// collectV1 records the tree a lockfileVersion 1 lockfile describes: each
// dependency sits at node_modules/<name> below its own parent's path.
func collectV1(deps map[string]lockDepV1, at string, pins map[string]pin) {
	for name, dep := range deps {
		dir := "node_modules/" + name
		if at != "" {
			dir = at + "/node_modules/" + name
		}
		if isVersion(dep.Version) {
			pins[dir] = pin{Version: dep.Version, Dev: dep.Dev, Optional: dep.Optional, Bundled: dep.Bundled}
		}
		collectV1(dep.Dependencies, dir, pins)
	}
}

// isVersion reports whether v is a version this comparison can compare: a pin
// like "7.1.1". A "file:", "link:" or git spec is not one — the package
// installed for it is free to report a version no lockfile field equals.
func isVersion(v string) bool {
	return v != "" && v[0] >= '0' && v[0] <= '9'
}
