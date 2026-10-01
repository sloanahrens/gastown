package townconfig

// Parked state (gt-y3pgh.4, D5 Q5, G3-11).
//
// A rig is parked when its mayor/rigs.json entry carries a "parked" record
// {since, by, reason}. gt rig park and gt rig unpark are the only writers
// (Park, Unpark, MigrateLegacyParked); dispatch and the daemon read it
// through ParkState / IsParked, which fail closed: a town whose kernel does
// not load, an unregistered rig, or a park record that cannot be read all
// answer parked, with the error saying why.
//
// Before the registry field, gt rig park wrote "status": "parked" into the
// rig's .beads-wisp/config/<rig>.json, and readers also honored a
// status:parked label on the rig identity bead (which nothing wrote). The
// label is gone. The wisp file is no longer a source of truth, but a rig
// parked that way must not read as unparked between installing this binary
// and migrating, so the kernel records any leftover "status" value as a
// legacy park: ParkState answers parked with ErrLegacyParked until gt rig
// park --migrate (MigrateLegacyParked) moves it into the registry, or gt rig
// park / unpark rewrites that rig. Both remove the key from the wisp file,
// and the file itself once nothing else is left in it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
)

// LegacyParkedDir holds the pre-registry per-rig wisp config files,
// <rig>.json, relative to the town root.
const LegacyParkedDir = ".beads-wisp/config"

// legacyStatusKey is the wisp config key gt rig park used to write.
const legacyStatusKey = "status"

// ErrLegacyParked: the rig still has a pre-registry park record in its wisp
// config file. It reads as parked until gt rig park --migrate runs.
var ErrLegacyParked = errors.New("legacy park record not migrated")

// legacyWispFile is the wisp config file layout (internal/wisp.ConfigFile).
// It is spelled out here, not imported, so the kernel stays a leaf.
type legacyWispFile struct {
	Rig     string         `json:"rig"`
	Values  map[string]any `json:"values"`
	Blocked []string       `json:"blocked"`
}

// legacyParkedPath is a rig's pre-registry wisp config file.
func legacyParkedPath(root, rigName string) string {
	return filepath.Join(root, LegacyParkedDir, rigName+".json")
}

// readLegacyStatus returns the "status" value in a rig's wisp config file
// ("" when the file or key is absent) and the file's modification time.
func readLegacyStatus(root, rigName string) (string, time.Time, error) {
	path := legacyParkedPath(root, rigName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: wisp file under the town root
	if errors.Is(err, os.ErrNotExist) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	var f legacyWispFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	raw, ok := f.Values[legacyStatusKey]
	if !ok {
		return "", time.Time{}, nil
	}
	status, _ := raw.(string)
	if status == "" {
		status = fmt.Sprint(raw)
	}
	var mtime time.Time
	if fi, err := os.Stat(path); err == nil {
		mtime = fi.ModTime()
	}
	return status, mtime, nil
}

// loadLegacyParked records, per registered rig, a leftover wisp "status"
// value or a wisp file that cannot be read. Neither fails the load: they
// make only that rig read as parked.
func (t *Town) loadLegacyParked() error {
	t.legacyParked = map[string]error{}
	for name := range t.rigs {
		status, _, err := readLegacyStatus(t.root, name)
		switch {
		case err != nil:
			t.legacyParked[name] = fmt.Errorf("%w: %w", ErrLegacyParked, err)
		case status != "":
			t.legacyParked[name] = fmt.Errorf("%w: %s has status %q; run: gt rig park --migrate",
				ErrLegacyParked, legacyParkedPath(t.root, name), status)
		}
	}
	return nil
}

// RigParked returns a registered rig's park record, nil when the rig is not
// parked. An unregistered rig is ErrUnknownRig; a rig with an unmigrated
// wisp park record is ErrLegacyParked. Callers that gate work treat any
// error as parked; ParkState does that for them.
func (t *Town) RigParked(name string) (*config.RigParked, error) {
	entry, ok := t.rigs[name]
	if !ok {
		return nil, fmt.Errorf("%w %q: not in %s", ErrUnknownRig, name, t.path(FileRigs))
	}
	if err := t.legacyParked[name]; err != nil {
		return nil, err
	}
	if entry.Parked == nil {
		return nil, nil
	}
	p := *entry.Parked
	return &p, nil
}

// ParkState loads the kernel under root and returns rigName's park record.
// (nil, nil) means the rig is not parked. Any error means it must be treated
// as parked: the kernel did not load, the rig is not registered, or a legacy
// park record is waiting for gt rig park --migrate.
func ParkState(root, rigName string) (*config.RigParked, error) {
	t, err := Load(root)
	if err != nil {
		return nil, err
	}
	return t.RigParked(rigName)
}

// IsParked is ParkState as a yes/no and a one-line reason, failing closed:
// an error answers parked, with the error as the reason.
func IsParked(root, rigName string) (bool, string) {
	p, err := ParkState(root, rigName)
	if err != nil {
		return true, "park state unreadable (treated as parked): " + err.Error()
	}
	if p == nil {
		return false, ""
	}
	return true, Describe(p)
}

// Describe renders a park record as one line.
func Describe(p *config.RigParked) string {
	s := "parked since " + p.Since.Format(time.RFC3339)
	if p.By != "" {
		s += " by " + p.By
	}
	if p.Reason != "" {
		s += ": " + p.Reason
	}
	return s
}

// Park writes rec as rigName's park record and removes any legacy wisp
// record. A rig that is already parked keeps its record; the record in
// effect is returned. Only gt rig park calls it.
func Park(root, rigName string, rec config.RigParked) (config.RigParked, error) {
	var kept config.RigParked
	err := updateRig(root, rigName, func(entry *config.RigEntry) {
		if entry.Parked == nil {
			r := rec
			entry.Parked = &r
		}
		kept = *entry.Parked
	})
	if err != nil {
		return config.RigParked{}, err
	}
	// The registry says parked before the legacy record goes, so the rig
	// never reads as unparked in between.
	if err := clearLegacyStatus(root, rigName); err != nil {
		return kept, err
	}
	return kept, nil
}

// Unpark clears rigName's park record and any legacy wisp record. It
// reports whether the rig was parked by either. Only gt rig unpark calls it.
func Unpark(root, rigName string) (bool, error) {
	status, _, err := readLegacyStatus(root, rigName)
	if err != nil {
		return false, err
	}
	was := status != ""
	if err := updateRig(root, rigName, func(entry *config.RigEntry) {
		was = was || entry.Parked != nil
		entry.Parked = nil
	}); err != nil {
		return false, err
	}
	// The legacy record goes last: until it does, the rig still reads parked.
	return was, clearLegacyStatus(root, rigName)
}

// MigrateLegacyParked moves every registered rig's wisp park record into
// the registry: "status": "parked" becomes a park record dated by the wisp
// file's modification time, then the key is removed. Any other status value
// is an error and is left in place, so that rig keeps reading as parked
// until an operator decides. It returns the rigs it migrated. Running it
// again finds nothing to do. Only gt rig park --migrate calls it.
func MigrateLegacyParked(root, by string) ([]string, error) {
	rigs, err := config.LoadRigsConfig(filepath.Join(root, FileRigs))
	if err != nil {
		return nil, err
	}
	var migrated []string
	var errs []error
	for _, name := range sortedKeys(rigs.Rigs) {
		status, mtime, err := readLegacyStatus(root, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch status {
		case "":
			continue
		case "parked":
		default:
			errs = append(errs, fmt.Errorf("%s: status %q is not a park record; left in place (the rig reads as parked): resolve it with gt rig park %s or gt rig unpark %s",
				legacyParkedPath(root, name), status, name, name))
			continue
		}
		rec := config.RigParked{
			Since:  mtime.UTC(),
			By:     by,
			Reason: "migrated from " + filepath.Join(LegacyParkedDir, name+".json"),
		}
		if _, err := Park(root, name, rec); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		migrated = append(migrated, name)
	}
	return migrated, errors.Join(errs...)
}

// updateRig changes one registered rig's entry under the rigs.json lock.
func updateRig(root, rigName string, mutate func(*config.RigEntry)) error {
	path := filepath.Join(root, FileRigs)
	return config.UpdateConfigJSON(path, 0o644, func(rc *config.RigsConfig, exists bool) error {
		entry, ok := rc.Rigs[rigName]
		if !exists || !ok {
			return fmt.Errorf("%w %q: not in %s", ErrUnknownRig, rigName, path)
		}
		mutate(&entry)
		rc.Rigs[rigName] = entry
		return nil
	})
}

// clearLegacyStatus removes the "status" key from a rig's wisp config file,
// and the file when nothing else is left in it.
func clearLegacyStatus(root, rigName string) error {
	path := legacyParkedPath(root, rigName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: wisp file under the town root
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var f legacyWispFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := f.Values[legacyStatusKey]; !ok {
		return nil
	}
	delete(f.Values, legacyStatusKey)
	if len(f.Values) == 0 && len(f.Blocked) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
		return nil
	}
	if f.Blocked == nil {
		f.Blocked = []string{}
	}
	if err := atomicfile.WriteJSON(path, &f); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
