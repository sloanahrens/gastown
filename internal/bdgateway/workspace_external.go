package bdgateway

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// externalProjectsConfig is the part of a .beads/config.yaml this check reads.
type externalProjectsConfig struct {
	ExternalProjects map[string]string `yaml:"external_projects"`
}

// checkExternalProjects refuses a tree in which a configured external project
// does not resolve to a .beads/ directory in the tree. Every .beads/config.yaml
// among files, at any depth, names its external_projects; a relative path
// resolves from the parent of that config's .beads directory, as bd resolves
// it. An absolute path is skipped, since it cannot be checked against the tree.
// A path that climbs above the tree's root is refused. A config that does not
// parse, or has no external_projects, is not an error here.
func checkExternalProjects(files map[string]string) error {
	var errs []error
	for _, configFile := range slices.Sorted(maps.Keys(files)) {
		if path.Base(configFile) != "config.yaml" || path.Base(path.Dir(configFile)) != ".beads" {
			continue
		}
		var cfg externalProjectsConfig
		if err := yaml.Unmarshal([]byte(files[configFile]), &cfg); err != nil {
			continue
		}
		parent := path.Dir(path.Dir(configFile))
		for _, name := range slices.Sorted(maps.Keys(cfg.ExternalProjects)) {
			value := cfg.ExternalProjects[name]
			if err := checkExternalProject(files, configFile, parent, name, value); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// checkExternalProject checks one project named by configFile, whose .beads
// sits under parent. It returns an error when the project's path is empty,
// climbs above the tree's root, or has no file under its .beads/ in files.
func checkExternalProject(files map[string]string, configFile, parent, name, value string) error {
	if path.IsAbs(value) {
		return nil
	}
	if value == "" {
		return fmt.Errorf("%s: external project %q has an empty path", configFile, name)
	}
	resolved := path.Join(parent, value)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%s: external project %q path %q climbs above the tree root", configFile, name, value)
	}
	prefix := path.Join(resolved, ".beads") + "/"
	for file := range files {
		if strings.HasPrefix(file, prefix) {
			return nil
		}
	}
	return fmt.Errorf("%s: external project %q resolves to %q, which has no .beads/ in the tree", configFile, name, resolved)
}
