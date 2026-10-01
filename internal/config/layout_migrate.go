package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
)

// ErrAlreadyMigrated: the town is on the two-file layout already. gt config
// migrate is one-shot and refuses to run again.
var ErrAlreadyMigrated = errors.New("town config is on the two-file layout already")

// LayoutStep is one thing gt config migrate does to one retired file.
type LayoutStep struct {
	// File is the retired file, relative to the town root.
	File string
	// Host and Key name the section it moves to.
	Host, Key string
	// Action is what happens to it.
	Action LayoutAction
}

// LayoutAction is a LayoutStep's kind.
type LayoutAction string

const (
	// ActionMove copies the file into its host section, then removes it.
	ActionMove LayoutAction = "move"
	// ActionRetire removes a file its host section already holds verbatim
	// (an earlier migrate stopped before removing it).
	ActionRetire LayoutAction = "retire"
	// ActionCreate writes an empty rig registry: the town had no
	// mayor/rigs.json, and the registry section marks the layout.
	ActionCreate LayoutAction = "create"
)

func (s LayoutStep) String() string {
	switch s.Action {
	case ActionMove:
		return fmt.Sprintf("move %s into %s %q, then remove it", s.File, s.Host, s.Key)
	case ActionRetire:
		return fmt.Sprintf("remove %s (%s %q already holds it)", s.File, s.Host, s.Key)
	default:
		return fmt.Sprintf("write an empty rig registry to %s %q (no %s)", s.Host, s.Key, s.File)
	}
}

// migrationState is everything a migration reads, so the plan and the
// apply see the same files.
type migrationState struct {
	root  string
	hosts map[string]*hostFile // by relative host path
	// legacy holds each present retired file's JSON tree, by relative path.
	legacy map[string]any
	steps  []LayoutStep
}

type hostFile struct {
	path   string
	exists bool
	mode   os.FileMode
	top    map[string]any
}

// PlanLayoutMigration is what MigrateLayout would do to the town at
// townRoot, without writing anything. It is ErrAlreadyMigrated on a
// two-file town, and refuses (writing nothing) when a file does not
// decode strictly or a leftover file disagrees with its host section.
func PlanLayoutMigration(townRoot string) ([]LayoutStep, error) {
	st, err := readMigrationState(townRoot)
	if err != nil {
		return nil, err
	}
	return st.steps, nil
}

// MigrateLayout moves the town at townRoot from the five-file layout to the
// two-file one (layout.go). It holds the lock of every file it touches for
// the whole move, so a concurrent gt writer waits and then follows the file
// to its section. The order is crash-safe:
//
//  1. write settings/config.json, then mayor/town.json (the registry, which
//     marks the layout, last), each atomically;
//  2. verify: re-read both hosts, decode them strictly, and check every
//     section equals the file it came from;
//  3. only then remove the retired files.
//
// A crash before 3 leaves sections and files holding the same JSON; the
// hosts win for every reader and a rerun finishes the removal. A second run
// on a migrated town is ErrAlreadyMigrated.
func MigrateLayout(townRoot string) ([]LayoutStep, error) {
	var locked []string
	for _, rel := range LegacyConfigFiles() {
		locked = append(locked, filepath.Join(townRoot, filepath.FromSlash(rel)))
	}
	locked = append(locked,
		filepath.Join(townRoot, filepath.FromSlash(OperatorConfigFile)),
		filepath.Join(townRoot, filepath.FromSlash(MachineConfigFile)))
	for _, p := range locked {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", filepath.Dir(p), err)
		}
		unlock, err := lockConfigFile(p)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}

	st, err := readMigrationState(townRoot)
	if err != nil {
		return nil, err
	}
	for _, rel := range []string{OperatorConfigFile, MachineConfigFile} {
		h := st.hosts[rel]
		if !st.hostChanges(rel) {
			continue
		}
		out, err := marshalTree(h.top)
		if err != nil {
			return nil, fmt.Errorf("encoding %s: %w", h.path, err)
		}
		if err := replaceFile(h.path, out, h.mode); err != nil {
			return nil, err
		}
	}
	if err := st.verify(); err != nil {
		return nil, fmt.Errorf("verifying the migrated files (the old files are kept): %w", err)
	}
	for _, step := range st.steps {
		if step.Action == ActionCreate {
			continue
		}
		p := filepath.Join(townRoot, filepath.FromSlash(step.File))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("removing %s: %w", p, err)
		}
	}
	// A writer waiting on a retired file's lock re-checks after taking it
	// and follows the file to its section; nothing needs the lock again.
	for _, p := range locked[:len(sections)] {
		_ = os.Remove(p + ".lock")
	}
	for _, dir := range []string{"mayor", "settings"} {
		if d, err := os.Open(filepath.Join(townRoot, dir)); err == nil { //nolint:gosec // G304: town config dir
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return st.steps, nil
}

func readMigrationState(root string) (*migrationState, error) {
	st := &migrationState{root: root, hosts: map[string]*hostFile{}, legacy: map[string]any{}}
	machine, err := readHost(root, MachineConfigFile, &TownConfig{})
	if err != nil {
		return nil, err
	}
	if !machine.exists {
		return nil, fmt.Errorf("%s does not exist: not a Gas Town root", machine.path)
	}
	operator, err := readHost(root, OperatorConfigFile, &TownSettings{})
	if err != nil {
		return nil, err
	}
	if !operator.exists {
		operator.top = map[string]any{"type": "town-settings", "version": jsonInt(CurrentTownSettingsVersion)}
		operator.mode = 0o600
	}
	st.hosts[MachineConfigFile], st.hosts[OperatorConfigFile] = machine, operator

	for _, s := range sections {
		rel := s.dir + "/" + s.file
		host := st.hosts[s.host]
		inHost, has := host.top[s.key]
		tree, present, err := readLegacy(filepath.Join(root, s.dir, s.file), s)
		if err != nil {
			return nil, err
		}
		step := LayoutStep{File: rel, Host: s.host, Key: s.key}
		switch {
		case present && has:
			if !reflect.DeepEqual(tree, inHost) {
				return nil, fmt.Errorf("%s and %s %q both exist and differ: an old gt wrote the file after a partial migration; merge them by hand, then rerun", rel, s.host, s.key)
			}
			st.legacy[rel] = tree
			step.Action = ActionRetire
		case present:
			st.legacy[rel] = tree
			host.top[s.key] = tree
			step.Action = ActionMove
		case !has && s.key == registryKey:
			host.top[s.key] = map[string]any{"version": jsonInt(CurrentRigsVersion), "rigs": map[string]any{}}
			step.Action = ActionCreate
		default:
			continue
		}
		st.steps = append(st.steps, step)
	}
	if len(st.steps) == 0 {
		return nil, fmt.Errorf("%w: %s holds the rig registry and no old file is left", ErrAlreadyMigrated, MachineConfigFile)
	}
	return st, nil
}

// hostChanges reports whether any step adds a section to the host.
func (st *migrationState) hostChanges(rel string) bool {
	for _, step := range st.steps {
		if step.Host == rel && step.Action != ActionRetire {
			return true
		}
	}
	return false
}

// verify re-reads both hosts from disk, decodes them strictly, and checks
// each moved section is the JSON of the file it came from.
func (st *migrationState) verify() error {
	machine, err := readHost(st.root, MachineConfigFile, &TownConfig{})
	if err != nil {
		return err
	}
	operator, err := readHost(st.root, OperatorConfigFile, &TownSettings{})
	if err != nil {
		return err
	}
	got := map[string]*hostFile{MachineConfigFile: machine, OperatorConfigFile: operator}
	for _, step := range st.steps {
		section, ok := got[step.Host].top[step.Key]
		if !ok {
			return fmt.Errorf("%s has no %q section after writing it", step.Host, step.Key)
		}
		if want, moved := st.legacy[step.File]; moved && !reflect.DeepEqual(section, want) {
			return fmt.Errorf("%s %q does not equal %s after writing it", step.Host, step.Key, step.File)
		}
	}
	return nil
}

// readHost reads a host file's JSON tree, decoding it strictly into typed
// (and validating it) first. An absent host has exists=false.
func readHost(root, rel string, typed any) (*hostFile, error) {
	h := &hostFile{path: filepath.Join(root, filepath.FromSlash(rel)), top: map[string]any{}}
	data, err := os.ReadFile(h.path) //nolint:gosec // G304: host config path under the town root
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", h.path, err)
	}
	if err := DecodeJSONFile(h.path, data, typed); err != nil {
		return nil, err
	}
	if tc, ok := typed.(*TownConfig); ok {
		if err := validateTownConfig(tc); err != nil {
			return nil, fmt.Errorf("%s: %w", h.path, err)
		}
	}
	tree, err := decodeTree(data)
	if err != nil {
		return nil, &ParseError{Path: h.path, Err: err}
	}
	top, ok := tree.(map[string]any)
	if !ok {
		return nil, &ParseError{Path: h.path, Err: errors.New("not a JSON object")}
	}
	h.exists, h.top = true, top
	if fi, err := os.Stat(h.path); err == nil {
		h.mode = fi.Mode().Perm()
	}
	return h, nil
}

// readLegacy reads a retired file from its own path (never its section),
// decoding it strictly into its schema type and validating it.
func readLegacy(path string, s section) (tree any, present bool, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: retired config path under the town root
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	var validate error
	switch s.key {
	case registryKey:
		var v RigsConfig
		if err := DecodeJSONFile(path, data, &v); err != nil {
			return nil, false, err
		}
		validate = validateRigsConfig(&v)
	case "overseer":
		var v OverseerConfig
		if err := DecodeJSONFile(path, data, &v); err != nil {
			return nil, false, err
		}
		validate = validateOverseerConfig(&v)
	case "daemon":
		var v DaemonPatrolConfig
		if err := DecodeJSONFile(path, data, &v); err != nil {
			return nil, false, err
		}
		validate = validateDaemonPatrolConfig(&v)
	case "escalation":
		var v EscalationConfig
		if err := DecodeJSONFile(path, data, &v); err != nil {
			return nil, false, err
		}
		validate = validateEscalationConfig(&v)
	}
	if validate != nil {
		return nil, false, fmt.Errorf("%s: %w", path, validate)
	}
	tree, err = decodeTree(data)
	if err != nil {
		return nil, false, &ParseError{Path: path, Err: err}
	}
	return tree, true, nil
}

func marshalTree(v any) ([]byte, error) {
	out, err := jsonIndent(v)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// jsonInt is n as decodeTree reads it, so a built tree compares equal to
// one read back from disk.
func jsonInt(n int) json.Number { return json.Number(strconv.Itoa(n)) }
