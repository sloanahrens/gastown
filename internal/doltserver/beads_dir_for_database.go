package doltserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BeadsDirForDatabase returns the beads directory whose metadata.json names
// dbName as its dolt_database: the town's .beads, a rig in mayor/rigs.json,
// or a path in the town's routes.jsonl, in that order. It is the inverse of
// DatabaseForBeadsDir, for callers that find a database on the server and
// must hand the write to bd, which selects its database by beads dir.
func BeadsDirForDatabase(townRoot, dbName string) (string, error) {
	if townRoot == "" || dbName == "" {
		return "", fmt.Errorf("beads dir for database: town root and database name are required")
	}
	for _, dir := range candidateBeadsDirs(townRoot) {
		if DatabaseForBeadsDir(dir) == dbName {
			return dir, nil
		}
	}
	return "", fmt.Errorf("no beads directory in %s names database %q in its metadata.json", townRoot, dbName)
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
