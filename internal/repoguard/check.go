package repoguard

import (
	"path/filepath"
)

// Env is what Check reads the repository through. Production wiring passes the
// os and os/exec implementations; tests pass fakes.
type Env struct {
	Git      Git
	Exec     Exec
	LookPath func(file string) (string, error)
	ReadFile func(name string) ([]byte, error)
}

// Report is what the three guards found in one repository.
type Report struct {
	// Replaces are the go.mod lines the replace guard refused. repo-guards
	// prints them on stdout, ahead of the replace message.
	Replaces []string
	// Problems are the stderr lines, in the order the guards ran.
	Problems []string
}

// Failed reports whether any guard failed. Each failure adds a line to
// Problems, so a Report with none passed every guard.
func (r Report) Failed() bool {
	return len(r.Problems) > 0
}

const (
	gomodReadProblem = "repo-guards: cannot read go.mod; the replace guard did not run"
	gomodProblem     = "repo-guards: go.mod contains replace directives; they break go install ...@latest (upstream #2230)"
)

// Check runs all three guards in root and returns what they found. It runs
// every guard even after one fails, as the script did, so a run reports every
// problem at once. A go.mod that cannot be read fails the replace guard: a
// guard that cannot read its input must not pass.
func Check(root string, env Env) Report {
	var r Report
	goMod, err := env.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		r.Problems = append(r.Problems, gomodReadProblem)
	} else if bad := CheckGoModReplaces(string(goMod)); len(bad) > 0 {
		r.Replaces = bad
		r.Problems = append(r.Problems, gomodProblem)
	}
	// The seam reports tracked on any error other than exit status 1, so a git
	// that cannot run fails this guard (see CLIGit.IsTracked in trackedfiles.go).
	if CheckIssuesJSONL(env.Git, root) {
		r.Problems = append(r.Problems, IssuesJSONLProblem)
	}
	unformatted, problem := CheckGofmt(env.Git, env.Exec, env.LookPath, root)
	if problem != "" {
		r.Problems = append(r.Problems, problem)
	}
	r.Problems = append(r.Problems, unformatted...)
	return r
}
