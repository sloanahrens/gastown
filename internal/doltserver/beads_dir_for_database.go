package doltserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	configpkg "github.com/steveyegge/gastown/internal/config"
)

// ErrNoBeadsDir is the not-found error of BeadsDirForDatabase: no searched root
// names the database, and no metadata.json among them was present but unreadable.
// An unreadable metadata.json is not this error: it may be the one that names
// the database.
var ErrNoBeadsDir = errors.New("no beads directory")

// BeadsDirForDatabase returns the beads directory whose metadata.json names
// dbName as its dolt_database.
func BeadsDirForDatabase(townRoot, dbName string, extraRoots ...string) (string, error) {
	if townRoot == "" || dbName == "" {
		return "", fmt.Errorf("beads dir for database: town root and database name are required")
	}
	// The inverse of DatabaseForBeadsDir: the caller found a database on the
	// server and must hand the write to bd, which selects its database by
	// beads dir. The town's own workspaces are searched first; extraRoots
	// follow, for a workspace outside the town such as ~/.claude (gt-33etw).
	//
	// A metadata.json that is present but unreadable reads as naming no
	// database, so the search goes on past it: a match in a later root still
	// wins. Only a search that finds nothing reports the unreadable file, and
	// it does so as an error other than ErrNoBeadsDir, so no caller reads a
	// broken workspace as an absent one.
	var unreadable error
	for _, dir := range append(candidateBeadsDirs(townRoot), extraRootBeadsDirs(extraRoots)...) {
		db := DatabaseForBeadsDir(dir)
		if db == dbName {
			return dir, nil
		}
		if db == "" && unreadable == nil {
			_, err := configpkg.BeadsFileDatabaseErr(filepath.Join(dir, configpkg.BeadsMetadataFile))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				unreadable = err
			}
		}
	}
	if unreadable != nil {
		return "", fmt.Errorf("beads dir for database %q: %w", dbName, unreadable)
	}
	searched := append([]string{townRoot}, extraRoots...)
	return "", fmt.Errorf("%w in %s names database %q in its metadata.json",
		ErrNoBeadsDir, strings.Join(searched, ", "), dbName)
}

// extraRootBeadsDirs are the beads directories an extra root can hold: a
// workspace keeps its beads in .beads, and a root named directly as a beads
// directory carries metadata.json itself.
func extraRootBeadsDirs(extraRoots []string) []string {
	dirs := make([]string, 0, 2*len(extraRoots))
	for _, root := range extraRoots {
		if root == "" {
			continue
		}
		dirs = append(dirs, filepath.Join(root, ".beads"), root)
	}
	return dirs
}

func candidateBeadsDirs(townRoot string) []string {
	dirs := []string{filepath.Join(townRoot, ".beads")}

	if rigs, err := registeredRigs(townRoot); err == nil {
		names := make([]string, 0, len(rigs))
		for name := range rigs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if dir := FindRigBeadsDir(townRoot, name); dir != "" {
				dirs = append(dirs, dir)
			}
		}
	}

	if data, err := os.ReadFile(filepath.Join(townRoot, ".beads", "routes.jsonl")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			var route struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &route) != nil || route.Path == "" || route.Path == "." {
				continue
			}
			dirs = append(dirs, filepath.Join(townRoot, route.Path, ".beads"))
		}
	}
	return dirs
}
