package repoguard

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
)

// Exec runs a command. CLIExec runs it with os/exec; tests inject fakes.
type Exec interface {
	// Run runs name with args in dir and returns what it wrote to stdout.
	Run(dir string, name string, args ...string) (stdout string, err error)
}

// CLIExec runs commands with os/exec. The zero value runs them for real. start
// replaces the run step, so a unit test can answer a command without starting
// a process (the seam internal/tmux uses).
type CLIExec struct {
	start func(*exec.Cmd) error
}

// Run runs name in dir and returns its stdout. Stderr is discarded: a gofmt
// error can echo file contents, so the guard names the failed rule instead of
// printing it.
func (e CLIExec) Run(dir string, name string, args ...string) (string, error) {
	start := e.start
	if start == nil {
		start = (*exec.Cmd).Run
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := start(cmd)
	return stdout.String(), err
}

// gofmtArgBytes caps the file names passed to one gofmt call, so argv stays
// well under the OS limit however many files are tracked.
const gofmtArgBytes = 64 << 10

const (
	gofmtMissingProblem     = "repo-guards: gofmt is not on PATH; make lint needs the Go toolchain"
	gofmtFailedProblem      = "repo-guards: gofmt -s failed"
	gofmtListFailedProblem  = "repo-guards: cannot list the tracked .go files"
	gofmtUnformattedProblem = "repo-guards: these files are not gofmt -s clean; run 'gofmt -s -w' on them:"
)

// CheckGofmt runs gofmt -s -l over the tracked .go files outside testdata
// directories. It returns the files gofmt -s would rewrite, and a problem line
// when the check cannot run or finds files to fix. gofmt is looked up before
// the file list is read, so a missing toolchain fails even with no .go files.
func CheckGofmt(g Git, e Exec, lookPath func(string) (string, error), root string) (unformatted []string, problem string) {
	gofmt, err := lookPath("gofmt")
	if err != nil {
		return nil, gofmtMissingProblem
	}
	listed, err := g.LsFiles(root, true, "*.go")
	if err != nil {
		return nil, gofmtListFailedProblem
	}
	deleted, err := g.LsDeleted(root, "*.go")
	if err != nil {
		return nil, gofmtListFailedProblem
	}
	listed = dropPaths(listed, deleted)
	for _, batch := range gofmtBatches(checkableGoFiles(listed)) {
		out, err := e.Run(root, gofmt, append([]string{"-s", "-l"}, batch...)...)
		if err != nil {
			return nil, gofmtFailedProblem
		}
		unformatted = append(unformatted, nonEmptyFields(out, "\n")...)
	}
	if len(unformatted) > 0 {
		return unformatted, gofmtUnformattedProblem
	}
	return nil, ""
}

// checkableGoFiles drops fixtures under a testdata directory: the Go toolchain
// does not treat them as source, so they may hold code no formatter has seen.
func checkableGoFiles(paths []string) []string {
	var keep []string
	for _, p := range paths {
		if strings.HasPrefix(p, "testdata/") || strings.Contains(p, "/testdata/") {
			continue
		}
		keep = append(keep, p)
	}
	return keep
}

// dropPaths returns paths without the names in drop. A tracked file deleted from
// the working tree has nothing for gofmt to read, so it is not a formatting
// failure; the deletion is for git status to report.
func dropPaths(paths, drop []string) []string {
	var keep []string
	for _, p := range paths {
		if !slices.Contains(drop, p) {
			keep = append(keep, p)
		}
	}
	return keep
}

// gofmtBatches splits files into groups whose names fit in gofmtArgBytes. A
// name longer than the cap travels alone rather than being dropped.
func gofmtBatches(files []string) [][]string {
	var batches [][]string
	var cur []string
	size := 0
	for _, f := range files {
		if len(cur) > 0 && size+len(f)+1 > gofmtArgBytes {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
		cur = append(cur, f)
		size += len(f) + 1
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

// nonEmptyFields splits out on sep and drops the empty pieces, such as the one
// after a trailing separator.
func nonEmptyFields(out, sep string) []string {
	var fields []string
	for _, f := range strings.Split(out, sep) {
		if f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}
