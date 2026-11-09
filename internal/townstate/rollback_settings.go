package townstate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"

	"github.com/steveyegge/gastown/internal/constants"
)

// maxSettingsBytes bounds a town settings file the replay reads whole, so an
// archive cannot make an import hold an unbounded file in memory.
const maxSettingsBytes = 1 << 20

// rollbackSettingsWhy is the reason each settings row carries.
const rollbackSettingsWhy = "the settings file the pod owns after cut-over; the rollback replays it to the Mac (gt-1od2k8)"

// townSettingsRel is the town's settings file, relative to the town root. Each
// registered rig keeps its own at <rig>/settings/config.json.
var townSettingsRel = path.Join(constants.DirSettings, "config.json")

// rollbackSettingsRows returns the settings files the rollback export carries by
// name after its rows: the town's, then each registered rig's. The manifest keeps
// both settings rows shared, so a normal export and the seed's daemon stream
// leave them out, and only the rollback writes them.
func rollbackSettingsRows(rigs []string) []Entry {
	rows := []Entry{{Path: townSettingsRel, Owner: OwnerShared, Why: rollbackSettingsWhy}}
	for _, rig := range rigs {
		rows = append(rows, Entry{Path: path.Join(rig, constants.DirSettings, "config.json"), Owner: OwnerShared, Why: rollbackSettingsWhy})
	}
	return rows
}

// rollbackSettingsEntry returns the row for name when name is one of the settings
// files the rollback carries for a rig the destination registers. The replay is
// the only import that accepts one; a seed into an empty root refuses it.
//
// The row comes back Private, so the replay writes the file at the mode its
// archive entry carries instead of the 0644 a shared row is masked to. The
// manifest cannot hold that flag on a shared row (Validate refuses it), so the
// flag is set on this copy only. The copy is never validated or persisted, and
// the import reads nothing from it but Private.
func rollbackSettingsEntry(name string, rigs []string) (Entry, bool) {
	for _, e := range rollbackSettingsRows(rigs) {
		if e.Path == name {
			e.Private = true
			return e, true
		}
	}
	return Entry{}, false
}

// keepMacSupervisor reads the archived town settings file from tr and returns it
// with daemon.supervisor taken from the Mac's file at dest. The archive is read
// under maxSettingsBytes: a file that size is refused, never truncated.
func keepMacSupervisor(ops townOps, dest, name string, tr io.Reader) ([]byte, error) {
	archived, err := io.ReadAll(io.LimitReader(tr, maxSettingsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %q from the archive: %w", name, err)
	}
	if len(archived) > maxSettingsBytes {
		return nil, fmt.Errorf("the archive entry %q is larger than %d bytes; a replay does not carry a settings file that size", name, maxSettingsBytes)
	}
	mac, found, err := readMacSettings(ops, dest)
	if err != nil {
		return nil, err
	}
	out, err := keepSupervisor(archived, mac, found)
	if err != nil {
		return nil, fmt.Errorf("the archive entry %q: %w", name, err)
	}
	return out, nil
}

// keepSupervisor returns the archived town settings with daemon.supervisor taken
// from the Mac's own file: set to the Mac's value when that file has one, removed
// when it has none or is absent. Every other key and value is left as archived.
// Only an archive with no supervisor on either side comes back byte for byte;
// any other document is re-encoded, even when the two supervisors already agree.
// A file that is not one JSON object is refused on either side, so a replay never
// overwrites a file it could not read.
func keepSupervisor(archived, mac []byte, macExists bool) ([]byte, error) {
	root, err := decodeSettings(archived)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("settings is not one JSON object")
	}
	var macRoot map[string]any
	if macExists {
		if macRoot, err = decodeSettings(mac); err != nil {
			return nil, fmt.Errorf("the Mac's settings file: %w", err)
		}
		if macRoot == nil {
			return nil, errors.New("the Mac's settings file is not one JSON object")
		}
	}
	macDaemon, err := daemonObject(macRoot)
	if err != nil {
		return nil, fmt.Errorf("the Mac's settings file: %w", err)
	}
	macSupervisor, macHas := macDaemon["supervisor"]
	daemon, err := daemonObject(root)
	if err != nil {
		return nil, err
	}
	_, archHas := daemon["supervisor"]
	if !macHas && !archHas {
		return archived, nil
	}
	if !macHas {
		delete(daemon, "supervisor")
		return encodeSettings(root), nil
	}
	if daemon == nil {
		daemon = map[string]any{}
		root["daemon"] = daemon
	}
	daemon["supervisor"] = macSupervisor
	return encodeSettings(root), nil
}

// daemonObject returns the object under "daemon", or nil when the document has
// none. A daemon that is not an object is an error.
func daemonObject(root map[string]any) (map[string]any, error) {
	raw, has := root["daemon"]
	if !has {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("settings: daemon is not an object")
	}
	return obj, nil
}

// readMacSettings reads the Mac's settings file at dest. A file that is not there
// is reported as not found; any other read failure is an error.
func readMacSettings(ops townOps, dest string) ([]byte, bool, error) {
	f, err := ops.open(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the Mac's settings file: %w", err)
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("reading the Mac's settings file: %w", readErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("closing the Mac's settings file: %w", closeErr)
	}
	return data, true, nil
}
