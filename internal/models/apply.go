package models

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// WrittenFile is a file a command has written, and the backup that puts it back.
// Backup is "" when the write created the file.
type WrittenFile struct {
	Path   string
	Backup string
}

// Restore puts the file back as its backup has it, or removes it when the write
// created it.
func (f WrittenFile) Restore() error {
	if f.Backup == "" {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := os.ReadFile(f.Backup) //nolint:gosec // G304: a backup this command just wrote
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(f.Backup); err == nil {
		mode = fi.Mode().Perm()
	}
	return config.ReplaceFile(f.Path, data, mode)
}

// Written are the files a command has written, in the order written.
//
// A command that writes the table and the live config has two files and no
// transaction across them. Rather than leave them disagreeing when the second
// write fails, it puts every file it has written back, so a failed switch changes
// nothing and the table still says what the town runs (gt-jaagc).
type Written []WrittenFile

// Fail is cause, with every file written so far put back as it was. A file that
// cannot be put back is named in the error, so a rollback is never reported as
// clean when it was not.
func (ws Written) Fail(cause error) error {
	// Every file is tried, last written first, even after one cannot be put back:
	// a file that can be restored is restored, and each one that cannot is named.
	var stuck []string
	for i := len(ws) - 1; i >= 0; i-- {
		if err := ws[i].Restore(); err != nil {
			stuck = append(stuck, fmt.Sprintf("%s (%v)", ws[i].Path, err))
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("%w; written and not put back: %s", cause, strings.Join(stuck, "; "))
	}
	if len(ws) == 0 {
		return cause
	}
	return fmt.Errorf("%w (nothing was changed: every file written before it was put back)", cause)
}

// backupFile copies path to path.<stamp>.bak when path exists, and returns the
// backup's path, or "" when there was no file to back up. The backup is kept
// after a successful apply on purpose: it is the operator's copy of the settings
// as they were before the switch, and the rollback reads it.
func backupFile(path, stamp string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the settings file this command writes
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("backing up %s: %w", path, err)
	}
	backup := path + "." + stamp + ".bak"
	if err := config.ReplaceFile(backup, data, 0o600); err != nil {
		return "", fmt.Errorf("backing up %s: %w", path, err)
	}
	return backup, nil
}

// ApplyRoles writes the table's roles and default into the settings file at path:
// role_agents for each role's preset, and default_agent for the default profile's
// preset. The reviewer is not a role_agents entry, so it is left to the reviewer's
// own config, and a table-only role is skipped because role_agents has no key for
// it (gt-2ih67). The table is validated first, and nothing is written when it does
// not validate (gt-jaagc).
//
// It returns the settings file it wrote, so a caller that writes more can put
// that file back with Written.Fail. stamp names the backup.
func ApplyRoles(path, stamp string, t *Table) (Written, error) {
	// The stamp becomes part of the backup's name, so it is one path element.
	if stamp == "" || stamp == "." || stamp == ".." || strings.ContainsAny(stamp, `/\`) {
		return nil, fmt.Errorf("apply: stamp %q is not a file name element", stamp)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	presets := map[string]string{}
	for role, name := range t.Roles {
		if role == OMReview {
			continue
		}
		// A table-only row has no role_agents setting to write, and a retired
		// one would refuse the file it landed in (gt-2ih67).
		if _, only := TableOnlyOf(role); only {
			continue
		}
		p, _ := t.ProfileOf(name)
		presets[role] = p.Preset
	}
	defaultPreset := ""
	if t.Default != "" {
		p, _ := t.ProfileOf(t.Default)
		defaultPreset = p.Preset
	}

	backup, err := backupFile(path, stamp)
	if err != nil {
		return nil, err
	}
	err = config.UpdateConfigJSON(path, 0o600, func(ts *config.TownSettings, exists bool) error {
		if !exists {
			*ts = *config.NewTownSettings()
		}
		if ts.RoleAgents == nil {
			ts.RoleAgents = map[string]string{}
		}
		for role, preset := range presets {
			ts.RoleAgents[role] = preset
		}
		if defaultPreset != "" {
			ts.DefaultAgent = defaultPreset
		}
		return nil
	})
	if err != nil {
		if backup != "" {
			_ = os.Remove(backup)
		}
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return Written{{Path: path, Backup: backup}}, nil
}
