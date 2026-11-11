package townstate

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// TownCopyRow is one row of the town copy's allowlist: a town-root-relative path
// the pod's claim must carry, the side of the split that puts it there, and why.
type TownCopyRow struct {
	// Path is relative to the town root: one exact file, a directory ending in
	// "/" (every file under it, at any depth), or a glob whose last element holds
	// a "*" (every file it matches in that one directory).
	Path string
	// Owner is the side of the split that carries the file. The live copy's
	// settings step sends the OwnerShared rows only. An OwnerDaemon row is carried
	// by the town-state export through its own manifest (internal/manifestdata),
	// which does not read this table: the daemon rows here list those files, and
	// they do not drive the export.
	Owner Owner
	// Why names what reads the file in the pod.
	Why string
}

// TownCopyRows returns the town copy's allowlist for a town whose registry names
// rigs; the caller validates those names before they reach a path. It is the whole
// list: a town file no row names is not carried, so a secret, an environment file
// or a backup copy stays on the host until a row is added for it (gt-pno1e6).
//
// The custom-theme row is settings/themes/*.txt, because a theme file is read
// only at <name>.txt (internal/polecat/namepool.go); anything else under
// settings/themes stays out with the rest of the directory.
func TownCopyRows(rigs []string) []TownCopyRow {
	rows := []TownCopyRow{
		{Path: path.Join(constants.DirMayor, constants.FileTownJSON), Owner: OwnerDaemon, Why: "the town config internal/config loads and the daemon reads; the registry may sit under its registry section"},
		{Path: path.Join(constants.DirSettings, "config.json"), Owner: OwnerShared, Why: "the town's behavioral settings internal/config reads, sent with the daemon supervisor removed"},
		{Path: path.Join(constants.DirSettings, "models.json"), Owner: OwnerShared, Why: "the role model table internal/models reads for gt model and the dashboard"},
		{Path: path.Join(constants.DirSettings, "placement.json"), Owner: OwnerShared, Why: "the component placement table internal/metrics reads for its report sources"},
		{Path: path.Join(constants.DirSettings, "regime-events.json"), Owner: OwnerShared, Why: "the regime events internal/metrics and gt regime record read; the operator edits this file by hand"},
		{Path: path.Join(constants.DirSettings, "themes") + "/*.txt", Owner: OwnerShared, Why: "the custom namepool themes, read at <town>/settings/themes/<style>.txt so every rig names from its own theme"},
	}
	for _, rig := range rigs {
		rows = append(rows, TownCopyRow{
			Path:  path.Join(rig, constants.DirSettings, "config.json"),
			Owner: OwnerShared,
			Why:   "the rig's behavioral settings internal/config writes and the landing worker reads",
		})
	}
	return append(rows,
		TownCopyRow{Path: "directives/", Owner: OwnerShared, Why: "the town directives gt prime renders into a session"},
		TownCopyRow{Path: path.Join(constants.DirBeads, "config.yaml"), Owner: OwnerDaemon, Why: "the beads workspace config bd reads"},
	)
}

// Shared reports whether the live copy's settings step carries the row.
func (r TownCopyRow) Shared() bool {
	return r.Owner == OwnerShared
}

// File reports whether the row names one file, rather than a directory or a glob.
func (r TownCopyRow) File() bool {
	return !strings.HasSuffix(r.Path, "/") && !strings.Contains(r.Path, "*")
}

// TownCopyPaths returns the town-root-relative names the tree at fsys holds for
// rows, in row order, with a directory or glob row expanded in name order. An
// exact row that is not there contributes nothing; one that exists is returned
// even when it is not a regular file, so the reader refuses it by name. A row
// whose directory cannot be listed is an error, never an empty row: a copy that
// silently carries less than the town has is the failure this table stops.
func TownCopyPaths(fsys fs.FS, rows []TownCopyRow) ([]string, error) {
	var names []string
	for _, row := range rows {
		found, err := townCopyRowPaths(fsys, row.Path)
		if err != nil {
			return nil, fmt.Errorf("listing town copy row %s: %w", row.Path, err)
		}
		names = append(names, found...)
	}
	return names, nil
}

// townCopyRowPaths expands one row's pattern against fsys.
func townCopyRowPaths(fsys fs.FS, row string) ([]string, error) {
	switch {
	case strings.HasSuffix(row, "/"):
		return townCopyTreeFiles(fsys, strings.TrimSuffix(row, "/"))
	case strings.Contains(row, "*"):
		dir, pattern := path.Split(row)
		return townCopyGlobFiles(fsys, strings.TrimSuffix(dir, "/"), pattern)
	default:
		// An exact row is returned whenever its name exists; a name that is not
		// a regular file is the reader's to refuse, with the file named.
		if _, err := fs.Stat(fsys, row); errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return []string{row}, nil
	}
}

// townCopyTreeFiles walks dir so a directive in a subdirectory is carried too.
func townCopyTreeFiles(fsys fs.FS, dir string) ([]string, error) {
	info, err := fs.Stat(fsys, dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, fmt.Errorf("%s is a file, not a directory", dir)
	}
	var names []string
	err = fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// townCopyGlobFiles matches one directory, so settings/themes/*.txt takes a theme
// and not a name nested under settings/themes.
func townCopyGlobFiles(fsys fs.FS, dir, pattern string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match, err := path.Match(pattern, e.Name())
		if err != nil {
			return nil, err
		}
		if match {
			names = append(names, path.Join(dir, e.Name()))
		}
	}
	return names, nil
}
