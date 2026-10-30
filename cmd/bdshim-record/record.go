package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/bdshim/flagrecord"
)

// Runner runs bd with args in dir and returns what it wrote to stdout and its exit code; it is the
// runner flagrecord walks bd's help through.
type Runner = flagrecord.Runner

// Copier makes a fresh copy of the fixture: a directory holding a database provisioned from it. It
// returns the copy's directory and the function that removes the directory and its database.
type Copier func() (dir string, remove func() error, err error)

// Recorder runs rows against bd. Every row runs in a copy Copy makes, so the fixture file itself is
// read only by the Copier and never by a row (gt-p85pw.87, gt-p85pw.92).
type Recorder struct {
	Run  Runner
	Copy Copier
}

// Golden is what bd printed for one row.
type Golden struct {
	Name   string
	Stdout []byte
	Exit   int
}

// Capture runs every row and returns one golden per row, in table order. Nothing runs in the fixture
// itself: the read rows share one copy of it and each mutating row gets a copy of its own, so a row
// that writes cannot reach the operator's fixture or another row, and every copy is removed before
// Capture returns. A table whose golden names collide is refused before anything is copied, since
// two rows would write the same files. The fixture and the table are checked before any row runs
// (checkPreflight).
func (rc Recorder) Capture(rows []Row) ([]Golden, error) {
	if err := checkUnique(rows); err != nil {
		return nil, err
	}
	sharedDir, removeShared, err := rc.Copy()
	if err != nil {
		return nil, fmt.Errorf("copy fixture: %w", err)
	}
	goldens, err := rc.captureRows(rows, sharedDir)
	if rmErr := removeShared(); rmErr != nil {
		err = errors.Join(err, fmt.Errorf("remove fixture copy: %w", rmErr))
	}
	if err != nil {
		return nil, err
	}
	return goldens, nil
}

// captureRows checks the preflight and runs every row in the copy it belongs to. The caller removes
// the shared copy afterwards.
func (rc Recorder) captureRows(rows []Row, sharedDir string) ([]Golden, error) {
	if err := checkPreflight(rc.Run, sharedDir, rows); err != nil {
		return nil, err
	}
	goldens := make([]Golden, 0, len(rows))
	for _, row := range rows {
		g, err := rc.runRow(row, sharedDir)
		if err != nil {
			return nil, err
		}
		goldens = append(goldens, g)
	}
	return goldens, nil
}

// checkPreflight refuses a fixture or a table bd cannot answer: the fixture must hold every bead the
// rows name, and bd must know every verb they name. Otherwise the recording succeeds while recording
// bd's "not found" or "unknown command" as its answer.
func checkPreflight(run Runner, dir string, rows []Row) error {
	for _, id := range fixtureIDs {
		if err := expectExit(run, dir, []string{"show", id, "--json"}, "check the fixture bead "+id); err != nil {
			return err
		}
	}
	for _, verb := range distinctVerbs(rows) {
		if err := expectExit(run, dir, append(strings.Fields(verb), "--help"), "check the verb "+verb); err != nil {
			return err
		}
	}
	return nil
}

// expectExit runs one bd call whose answer must be exit 0, and names check when it is not.
func expectExit(run Runner, dir string, args []string, check string) error {
	_, exit, err := run(dir, args)
	if err != nil {
		return fmt.Errorf("%s: %w", check, err)
	}
	if exit != 0 {
		return fmt.Errorf("%s: bd exited %d", check, exit)
	}
	return nil
}

// distinctVerbs returns each verb the rows name once, in table order.
func distinctVerbs(rows []Row) []string {
	seen := make(map[string]bool, len(rows))
	var verbs []string
	for _, row := range rows {
		if !seen[row.Verb] {
			seen[row.Verb] = true
			verbs = append(verbs, row.Verb)
		}
	}
	return verbs
}

// runRow runs one row in the copy it belongs to and removes the copy it made, even when bd fails.
func (rc Recorder) runRow(row Row, sharedDir string) (Golden, error) {
	name := GoldenName(row)
	dir, remove := sharedDir, func() error { return nil }
	if row.Mutates {
		var err error
		if dir, remove, err = rc.Copy(); err != nil {
			return Golden{}, fmt.Errorf("%s: copy fixture: %w", name, err)
		}
	}
	stdout, exit, err := rc.Run(dir, row.Args())
	if rmErr := remove(); rmErr != nil && err == nil {
		err = fmt.Errorf("remove fixture copy: %w", rmErr)
	}
	if err != nil {
		return Golden{}, fmt.Errorf("%s: %w", name, err)
	}
	return Golden{Name: name, Stdout: stdout, Exit: exit}, nil
}

// checkUnique refuses a table in which two rows would write the same golden.
func checkUnique(rows []Row) error {
	seen := make(map[string]string, len(rows))
	for _, row := range rows {
		name := GoldenName(row)
		args := strings.Join(row.Args(), " ")
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("%s: rows %q and %q both write this golden", name, prev, args)
		}
		seen[name] = args
	}
	return nil
}

// goldenMode is the mode golden files are written with. Git records no mode beyond the executable
// bit, so owner-only costs nothing in the repo (gosec G306).
const goldenMode = 0o600

// goldenExts are the extensions of the two files one golden is made of.
var goldenExts = []string{".json", ".exit"}

// Write writes each golden as <name>.json, bd's stdout, and <name>.exit, bd's exit code on one line.
func Write(dir string, goldens []Golden) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, g := range goldens {
		if err := os.WriteFile(filepath.Join(dir, g.Name+".json"), g.Stdout, goldenMode); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, g.Name+".exit"), exitFile(g.Exit), goldenMode); err != nil {
			return err
		}
	}
	return nil
}

// RemoveDropped deletes the goldens in dir whose row the table no longer holds and returns their
// names, so the directory holds only the goldens its table names.
func RemoveDropped(dir string, goldens []Golden) ([]string, error) {
	dropped, err := Orphans(dir, goldens)
	if err == nil {
		err = removeGoldens(dir, dropped)
	}
	if err != nil {
		return nil, err
	}
	return dropped, nil
}

// removeGoldens deletes both files of each named golden. A file that is already gone is not an error,
// since a golden whose row was dropped may have been written with only one of its two files.
func removeGoldens(dir string, names []string) error {
	for _, name := range names {
		for _, ext := range goldenExts {
			path := filepath.Join(dir, name+ext)
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove golden %s: %w", path, err)
			}
		}
	}
	return nil
}

// Orphans returns the goldens in dir whose row the table no longer holds, sorted, so a row that was
// renamed or dropped is reported rather than left looking like a recorded answer. A directory that
// is not there holds no orphans; a failure to list one is an error.
func Orphans(dir string, goldens []Golden) ([]string, error) {
	keep := make(map[string]bool, len(goldens))
	for _, g := range goldens {
		keep[g.Name] = true
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list goldens directory %s: %w", dir, err)
	}
	seen := make(map[string]bool, len(entries))
	var orphans []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := goldenStem(e.Name())
		if !ok || keep[name] || seen[name] {
			continue
		}
		seen[name] = true
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// goldenStem returns the golden a file in the goldens directory belongs to, and whether the file is
// part of a golden at all.
func goldenStem(file string) (string, bool) {
	for _, ext := range goldenExts {
		if stem, ok := strings.CutSuffix(file, ext); ok && stem != "" {
			return stem, true
		}
	}
	return "", false
}

// Differ returns the names of the goldens in dir whose stdout or exit code differs from the
// captured output, or whose files are missing, in table order. It writes nothing. A golden it
// cannot read is an error, not a difference, so drift is never hidden by a read failure.
func Differ(dir string, goldens []Golden) ([]string, error) {
	var differ []string
	for _, g := range goldens {
		same, err := sameGolden(dir, g)
		if err != nil {
			return nil, err
		}
		if !same {
			differ = append(differ, g.Name)
		}
	}
	return differ, nil
}

func sameGolden(dir string, g Golden) (bool, error) {
	stdout, found, err := readGolden(filepath.Join(dir, g.Name+".json"))
	if err != nil || !found {
		return false, err
	}
	exit, found, err := readGolden(filepath.Join(dir, g.Name+".exit"))
	if err != nil || !found {
		return false, err
	}
	return bytes.Equal(stdout, g.Stdout) && bytes.Equal(exit, exitFile(g.Exit)), nil
}

// readGolden returns a golden file's bytes. A missing file is found=false, not an error; any other
// read failure is an error.
func readGolden(path string) (data []byte, found bool, err error) {
	data, err = os.ReadFile(path) //nolint:gosec // G304: path is a golden file in the output directory the operator names
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read golden %s: %w", path, err)
	}
	return data, true, nil
}

func exitFile(code int) []byte {
	return []byte(strconv.Itoa(code) + "\n")
}
