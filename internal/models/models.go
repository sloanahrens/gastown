// Package models is the town's model table, settings/models.json: the profile
// each role runs and what a profile is. gt model reads and writes it (gt-jaagc).
package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// FileName is the table under the town's settings directory.
const FileName = "models.json"

// TablePath is the town's model table.
func TablePath(townRoot string) string { return filepath.Join(townRoot, "settings", FileName) }

// OMReview is the row whose live value is the reviewer's own config, the om
// backend, rather than a role_agents entry.
const OMReview = "om-review"

// Roles are the roles a table may name, in gt model show's order: every key
// role_agents resolves, then the reviewer.
var Roles = []string{OMReview, "crew", "witness", "refinery", "deacon", "mayor", "dog"}

// ErrPolecatRole refuses a polecat entry: a polecat's agent is its seat in
// polecat_pool.seats (gt-mukgk).
var ErrPolecatRole = errors.New("polecat is not a table role: its agent is its seat in polecat_pool.seats (gt-mukgk)")

// ErrDefaultRole refuses default as a role: the default profile is a key of its
// own, set with gt model set-default.
var ErrDefaultRole = errors.New("default is not a role: set the default profile with gt model set-default <profile>")

// Profile is one row of the table's profiles: the agent preset a role runs, and
// the model the reviewer runs for it.
type Profile struct {
	// Preset is the agent preset name written to role_agents and default_agent.
	Preset string `json:"preset,omitempty"`
	// Model is the model the reviewer's backend runs. A profile that names one
	// needs no om_backend: the model is swapped in the live backend, so the
	// operator's command keeps its path and flags.
	Model string `json:"model,omitempty"`
	// OMBackend is the reviewer's whole command, for a profile whose model the
	// live command cannot be given.
	OMBackend []string `json:"om_backend,omitempty"`
}

// Table is settings/models.json.
type Table struct {
	Default  string
	Profiles map[string]Profile
	Roles    map[string]string

	raw    map[string]json.RawMessage
	loaded bool
}

// Load reads the table at path, with builtin as the profiles that ship under the
// file's own. A missing file is an empty table, not an error: a town that never
// wrote one declares nothing. A file that does not parse is an error, because a
// table nobody can read must not pass for an empty one (gt-jaagc).
//
// builtin is the caller's: which profiles ship names the caller's agents, and
// this package is only the table's format (gt-jaagc).
func Load(path string, builtin map[string]Profile) (*Table, error) {
	t := &Table{Profiles: make(map[string]Profile, len(builtin)), Roles: map[string]string{}}
	for name, p := range builtin {
		t.Profiles[name] = p
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t.raw, t.loaded = doc, true
	if raw, ok := doc["default"]; ok {
		if err := json.Unmarshal(raw, &t.Default); err != nil {
			return nil, fmt.Errorf("%s: default: %w", path, err)
		}
	}
	if raw, ok := doc["profiles"]; ok {
		var file map[string]Profile
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, fmt.Errorf("%s: profiles: %w", path, err)
		}
		for name, p := range file {
			t.Profiles[name] = p
		}
	}
	if raw, ok := doc["roles"]; ok {
		if err := json.Unmarshal(raw, &t.Roles); err != nil {
			return nil, fmt.Errorf("%s: roles: %w", path, err)
		}
	}
	// A "roles": null decodes to a nil map, and set-role assigns into it: the
	// table always holds a map (gt-jaagc).
	if t.Roles == nil {
		t.Roles = map[string]string{}
	}
	return t, nil
}

// Loaded reports whether the table's file was there.
func (t *Table) Loaded() bool { return t != nil && t.loaded }

// ProfileOf is the profile name resolves to, the file's entry over the builtin
// of the same name.
func (t *Table) ProfileOf(name string) (Profile, bool) {
	if t == nil {
		return Profile{}, false
	}
	p, ok := t.Profiles[name]
	return p, ok
}

// RoleProfile is the profile role runs, and false when the table names none.
func (t *Table) RoleProfile(role string) (Profile, bool) {
	if t == nil {
		return Profile{}, false
	}
	name, ok := t.Roles[role]
	if !ok || name == "" {
		return Profile{}, false
	}
	return t.ProfileOf(name)
}

// RolePreset is the preset role's profile names, "" when the table names none.
func (t *Table) RolePreset(role string) string {
	p, _ := t.RoleProfile(role)
	return p.Preset
}

// ProfileNames lists the profiles a role may name, sorted, for a refusal.
func (t *Table) ProfileNames() string {
	if t == nil {
		return "none"
	}
	names := make([]string, 0, len(t.Profiles))
	for name := range t.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Save writes the table to path. Every key and profile it did not change is
// kept as it was read, so a field a newer gt writes is not dropped by this one;
// a new file carries the version its reader expects (gt-jaagc).
func (t *Table) Save(path string) error {
	// A table no write may carry is not written, so the file never holds a table
	// apply would refuse (gt-jaagc).
	if err := t.Validate(); err != nil {
		return err
	}
	doc := make(map[string]json.RawMessage, len(t.raw)+3)
	for k, v := range t.raw {
		doc[k] = v
	}
	if _, ok := doc["version"]; !ok {
		doc["version"] = json.RawMessage("1")
	}
	roles := t.Roles
	if roles == nil {
		roles = map[string]string{}
	}
	b, err := json.Marshal(roles)
	if err != nil {
		return err
	}
	doc["roles"] = b
	if t.Default == "" {
		delete(doc, "default")
	} else {
		b, err := json.Marshal(t.Default)
		if err != nil {
			return err
		}
		doc["default"] = b
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return config.ReplaceFile(path, out, 0o644)
}

// Validate refuses a table whose roles no write may carry: a role name
// role_agents has no key for, a profile that is not there, or a profile without
// the field its role needs.
func (t *Table) Validate() error {
	roles := make([]string, 0, len(t.Roles))
	for role := range t.Roles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if err := t.ValidateRole(role); err != nil {
			return err
		}
	}
	return t.ValidateDefault()
}

// ValidateDefault refuses a default profile no write may carry.
func (t *Table) ValidateDefault() error {
	if t.Default == "" {
		return nil
	}
	p, ok := t.ProfileOf(t.Default)
	if !ok {
		return fmt.Errorf("default = %q, and there is no such profile: the profiles are %s", t.Default, t.ProfileNames())
	}
	if p.Preset == "" {
		return fmt.Errorf("default = %q, and that profile names no preset to write to default_agent", t.Default)
	}
	return nil
}

// ValidateRole refuses a role name the table may not write, and a profile that
// cannot give the role what it needs.
func (t *Table) ValidateRole(role string) error {
	switch role {
	case "polecat":
		return ErrPolecatRole
	case "default":
		return ErrDefaultRole
	}
	if !RoleKnown(role) {
		return fmt.Errorf("role %q: want one of %s", role, strings.Join(Roles, ", "))
	}
	name := t.Roles[role]
	p, ok := t.ProfileOf(name)
	if !ok {
		return fmt.Errorf("role %s runs profile %q, and there is no such profile: the profiles are %s", role, name, t.ProfileNames())
	}
	if role == OMReview {
		if p.ReviewerModel() == "" {
			return fmt.Errorf("role %s runs profile %q, which names no model and no om_backend, so it cannot set the reviewer's backend", role, name)
		}
		return nil
	}
	if p.Preset == "" {
		return fmt.Errorf("role %s runs profile %q, which names no preset to write to role_agents.%s", role, name, role)
	}
	return nil
}

// RoleKnown reports whether role is one the table may name.
func RoleKnown(role string) bool {
	for _, r := range Roles {
		if r == role {
			return true
		}
	}
	return false
}

// ReviewerModel is the model this profile has the reviewer run.
func (p Profile) ReviewerModel() string {
	if p.Model != "" {
		return p.Model
	}
	return ModelName(p.OMBackend)
}

// ReviewerBackend is the backend argv this profile gives the reviewer: its own
// om_backend when it names one, else the live backend with the model swapped,
// so a switch keeps the operator's command (gt-jaagc).
func (p Profile) ReviewerBackend(live []string) ([]string, error) {
	return p.ReviewerBackendIn(hostHome(), live)
}

// ReviewerBackendIn is ReviewerBackend with the home its "~/" paths expand
// against, so a test passes its own home and never the host's (gt-jaagc).
func (p Profile) ReviewerBackendIn(home string, live []string) ([]string, error) {
	if len(p.OMBackend) > 0 {
		out := make([]string, len(p.OMBackend))
		for i, arg := range p.OMBackend {
			out[i] = expandHome(home, arg)
		}
		return out, nil
	}
	if p.Model == "" {
		return nil, errors.New("the profile names no model and no om_backend")
	}
	if len(live) == 0 {
		return []string{"claude", "-p", "--model", p.Model}, nil
	}
	out := append([]string(nil), live...)
	for i, arg := range out {
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 < len(out) {
				out[i+1] = p.Model
				return out, nil
			}
		case strings.HasPrefix(arg, "--model="):
			out[i] = "--model=" + p.Model
			return out, nil
		}
	}
	return nil, fmt.Errorf("the live backend %q names no --model to swap, so the profile needs an om_backend", strings.Join(live, " "))
}
