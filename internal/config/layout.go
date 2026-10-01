package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The two-file layout (gt-y3pgh.7, D5 Q2). A town's config used to be
// spread over mayor/town.json, mayor/rigs.json, mayor/overseer.json,
// mayor/daemon.json and settings/escalation.json beside settings/config.json.
// The two-file layout keeps two of them:
//
//   - mayor/town.json is machine-owned: town identity, the Dolt endpoint, the
//     rig registry (prefix and parked) under "registry" and the overseer
//     identity under "overseer". gt verbs write it; nobody edits it by hand.
//   - settings/config.json is operator-owned: agents, thresholds, secret
//     references, the daemon's patrols under "daemon" and escalation routing
//     under "escalation". gt config validate checks it.
//
// Each retired file lives on as a section of its host holding the file's
// whole document, byte-for-byte the same JSON. The loaders and the writer
// resolve a retired file's path to its section once the host carries it, so
// a caller that names mayor/rigs.json reads and writes the registry in
// mayor/town.json without knowing which layout the town is on. A host that
// carries a section always wins over a leftover file of the old name.
// gt config migrate (layout_migrate.go) moves a town from five files to two.

// section is where a retired file lives in the two-file layout.
type section struct {
	// dir and file name the retired file: <townRoot>/<dir>/<file>.
	dir, file string
	// host is the file holding it, relative to the town root, under key.
	host, key string
}

// Host files of the two-file layout, relative to the town root.
const (
	MachineConfigFile  = "mayor/town.json"
	OperatorConfigFile = "settings/config.json"
)

// sections lists every retired file, in migration order.
var sections = []section{
	{dir: "mayor", file: "rigs.json", host: MachineConfigFile, key: registryKey},
	{dir: "mayor", file: "overseer.json", host: MachineConfigFile, key: "overseer"},
	{dir: "mayor", file: "daemon.json", host: OperatorConfigFile, key: "daemon"},
	{dir: "settings", file: "escalation.json", host: OperatorConfigFile, key: "escalation"},
}

// LegacyConfigFiles lists the files the two-file layout retires, relative
// to the town root.
func LegacyConfigFiles() []string {
	out := make([]string, len(sections))
	for i, s := range sections {
		out[i] = s.dir + "/" + s.file
	}
	return out
}

// sectionOf returns the retired-file entry path names and its town root,
// or ok=false when path is not one of them.
func sectionOf(path string) (s section, townRoot string, ok bool) {
	dir := filepath.Dir(path)
	for _, s := range sections {
		if filepath.Base(path) == s.file && filepath.Base(dir) == s.dir {
			return s, filepath.Dir(dir), true
		}
	}
	return section{}, "", false
}

// sectionFor reports where path's content lives. For a retired file it is
// the host section (ok=true) when the host carries the section or the town
// is on the two-file layout (mayor/town.json carries "registry"); exists
// says whether the section is there. Otherwise the content is in path
// itself (ok=false). A host that exists but is not a JSON object is an
// error: it cannot be told whether it carries the section.
func sectionFor(path string) (host, key string, ok, exists bool, err error) {
	s, root, isLegacy := sectionOf(path)
	if !isLegacy {
		return "", "", false, false, nil
	}
	host = filepath.Join(root, filepath.FromSlash(s.host))
	top, err := readTopLevel(host)
	if err != nil {
		return "", "", false, false, err
	}
	if _, has := top[s.key]; has {
		return host, s.key, true, true, nil
	}
	migrated, err := isTwoFile(root, s.host, top)
	if err != nil || !migrated {
		return "", "", false, false, err
	}
	return host, s.key, true, false, nil
}

// isTwoFile reports whether mayor/town.json carries the registry section,
// which gt config migrate always writes. top is host's top level when host
// is the machine file, to save a read.
func isTwoFile(root, host string, top map[string]json.RawMessage) (bool, error) {
	if host != MachineConfigFile {
		var err error
		top, err = readTopLevel(filepath.Join(root, filepath.FromSlash(MachineConfigFile)))
		if err != nil {
			return false, err
		}
	}
	_, has := top[registryKey]
	return has, nil
}

// registryKey is the machine file's rig registry section, the two-file
// layout's marker.
const registryKey = "registry"

// readTopLevel reads a JSON object's top-level keys, nil when the file is
// absent.
func readTopLevel(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: host config path under the town root
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		var pe *ParseError
		if derr := DecodeJSONFile(path, data, &map[string]any{}); errors.As(derr, &pe) {
			return nil, pe
		}
		return nil, &ParseError{Path: path, Err: err}
	}
	return top, nil
}

// sectionLabel names a section in errors: "<host> (section registry)".
func sectionLabel(host, key string) string {
	return fmt.Sprintf("%s (section %q)", host, key)
}

// readConfigFile returns the content of a config file, following a retired
// file to its host section, and the name to report in errors. An absent
// file is an error satisfying os.IsNotExist.
func readConfigFile(path string) (data []byte, label string, err error) {
	host, key, ok, exists, err := sectionFor(path)
	var pe *ParseError
	if errors.As(err, &pe) {
		// The broken host is reported by its own loader, once; this file
		// reads from its own path, absent on a two-file town.
		ok, err = false, nil
	}
	if err != nil {
		return nil, path, err
	}
	if !ok {
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is an internal config path
		return data, path, err
	}
	notExist := &os.PathError{Op: "open", Path: sectionLabel(host, key), Err: os.ErrNotExist}
	if !exists {
		return nil, sectionLabel(host, key), notExist
	}
	top, err := readTopLevel(host)
	if err != nil {
		return nil, host, err
	}
	raw, has := top[key]
	if !has {
		// A writer dropped the section between the two reads.
		return nil, sectionLabel(host, key), notExist
	}
	return raw, sectionLabel(host, key), nil
}

// updateSection is UpdateConfigJSON for a retired file whose host carries
// its section: it decodes the section into a T under the host's lock, calls
// mutate, and writes the host back with only the section's changes applied.
func updateSection[T any](host, key string, mutate func(v *T, exists bool) error) error {
	unlock, err := lockConfigFile(host)
	if err != nil {
		return err
	}
	defer unlock()

	top := map[string]any{}
	mode := os.FileMode(0o600)
	data, err := os.ReadFile(host) //nolint:gosec // G304: host config path
	switch {
	case err == nil:
		tree, err := decodeTree(data)
		if err != nil {
			return &ParseError{Path: host, Err: err}
		}
		m, ok := tree.(map[string]any)
		if !ok {
			return &ParseError{Path: host, Err: errors.New("not a JSON object")}
		}
		top = m
		if fi, err := os.Stat(host); err == nil {
			mode = fi.Mode().Perm()
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("reading %s before writing: %w", host, err)
	}
	orig, exists := top[key]
	var cur T
	if exists {
		raw, err := json.Marshal(orig)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", sectionLabel(host, key), err)
		}
		if err := DecodeJSONFile(sectionLabel(host, key), raw, &cur); err != nil {
			return err
		}
	}
	base, err := jsonValue(&cur)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", sectionLabel(host, key), err)
	}
	if err := mutate(&cur, exists); err != nil {
		return err
	}
	next, err := jsonValue(&cur)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", sectionLabel(host, key), err)
	}
	if exists {
		top[key] = applyChanges(orig, base, next)
	} else {
		top[key] = next
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", host, err)
	}
	return replaceFile(host, append(out, '\n'), mode)
}

// Layout is which config layout a town is on.
type Layout string

const (
	// LayoutFiveFile: no host carries a section; every retired file is
	// read from its own path.
	LayoutFiveFile Layout = "five-file"
	// LayoutTwoFile: mayor/town.json carries the registry and no retired
	// file is left. A section the old town never had a file for (no
	// overseer.json, say) stays absent.
	LayoutTwoFile Layout = "two-file"
	// LayoutPartial: some sections are in their hosts and some retired
	// files remain (a migration that stopped part way, or a stray file
	// written by an old binary), or sections moved without the registry.
	// The hosts win; gt config migrate finishes.
	LayoutPartial Layout = "partial"
)

// LayoutReport is what DetectLayout found.
type LayoutReport struct {
	Layout Layout
	// Moved lists the retired files (relative paths) whose host carries
	// their section.
	Moved []string
	// Left lists the retired files (relative paths) still on disk.
	Left []string
}

// DetectLayout reports which layout the town at townRoot is on. It reads
// the hosts' top-level keys and stats the retired files; it decodes
// nothing strictly (townconfig.Check does that).
func DetectLayout(townRoot string) (LayoutReport, error) {
	var r LayoutReport
	tops := map[string]map[string]json.RawMessage{}
	for _, s := range sections {
		top, seen := tops[s.host]
		if !seen {
			var err error
			top, err = readTopLevel(filepath.Join(townRoot, filepath.FromSlash(s.host)))
			if err != nil {
				return r, err
			}
			tops[s.host] = top
		}
		rel := s.dir + "/" + s.file
		if _, has := top[s.key]; has {
			r.Moved = append(r.Moved, rel)
		}
		if _, err := os.Stat(filepath.Join(townRoot, s.dir, s.file)); err == nil {
			r.Left = append(r.Left, rel)
		}
	}
	sort.Strings(r.Moved)
	sort.Strings(r.Left)
	_, twoFile := tops[MachineConfigFile][registryKey]
	switch {
	case len(r.Moved) == 0:
		r.Layout = LayoutFiveFile
	case twoFile && len(r.Left) == 0:
		r.Layout = LayoutTwoFile
	default:
		r.Layout = LayoutPartial
	}
	return r, nil
}
