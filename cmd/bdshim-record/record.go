package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Runner runs bd with args in dir and returns what it wrote to stdout and its exit code. A non-zero
// exit is a result the golden records; an error means bd did not run to an exit.
type Runner func(dir string, args []string) (stdout []byte, exit int, err error)

// Copier makes a fresh copy of the fixture for one mutating row. It returns the copy's directory
// and the function that removes it.
type Copier func() (dir string, remove func() error, err error)

// Recorder runs rows against bd.
type Recorder struct {
	Fixture string // the directory read rows run in
	Run     Runner
	Copy    Copier
}

// Golden is what bd printed for one row.
type Golden struct {
	Name   string
	Stdout []byte
	Exit   int
}

// Capture runs every row in table order and returns one golden per row. A mutating row runs in a
// copy that is removed before the next row. A table whose golden names collide is refused before
// bd runs, since two rows would write the same files.
func (rc Recorder) Capture(rows []Row) ([]Golden, error) {
	if err := checkUnique(rows); err != nil {
		return nil, err
	}
	goldens := make([]Golden, 0, len(rows))
	for _, row := range rows {
		g, err := rc.runRow(row)
		if err != nil {
			return nil, err
		}
		goldens = append(goldens, g)
	}
	return goldens, nil
}

// runRow runs one row where it belongs and removes any copy it made, even when bd fails.
func (rc Recorder) runRow(row Row) (Golden, error) {
	name := GoldenName(row)
	dir, remove := rc.Fixture, func() error { return nil }
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
