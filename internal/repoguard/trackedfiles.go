package repoguard

import (
	"errors"
)

// Git is the slice of git the guards read. CLIGit runs git; tests inject fakes.
type Git interface {
	// LsFiles lists the paths tracked under root that match pathspec. nul
	// selects NUL-separated git output, so a name with a newline survives.
	LsFiles(root string, nul bool, pathspec ...string) ([]string, error)
	// IsTracked reports whether path, relative to root, is in the index.
	IsTracked(root, path string) bool
}

// issuesJSONLPath is the beads export file, relative to the repository root.
const issuesJSONLPath = ".beads/issues.jsonl"

// IssuesJSONLProblem is the line repo-guards prints when the export is tracked.
const IssuesJSONLProblem = "repo-guards: .beads/issues.jsonl is tracked; the Dolt server is the only beads backend (git rm --cached it)"

// CheckIssuesJSONL reports whether the beads export file is tracked. Only the
// index counts: bd exports the file locally, and an untracked copy is fine.
func CheckIssuesJSONL(g Git, root string) bool {
	return g.IsTracked(root, issuesJSONLPath)
}

// CLIGit is the Git that runs the git binary through ex. The zero value runs
// it with CLIExec.
type CLIGit struct {
	ex Exec
}

func (g CLIGit) run() Exec {
	if g.ex == nil {
		return CLIExec{}
	}
	return g.ex
}

// LsFiles runs git ls-files in root. Output is split on NUL when nul is set,
// on newlines otherwise.
func (g CLIGit) LsFiles(root string, nul bool, pathspec ...string) ([]string, error) {
	args := []string{"ls-files"}
	if nul {
		args = append(args, "-z")
	}
	args = append(args, "--")
	args = append(args, pathspec...)
	out, err := g.run().Run(root, "git", args...)
	if err != nil {
		return nil, err
	}
	sep := "\n"
	if nul {
		sep = "\x00"
	}
	return nonEmptyFields(out, sep), nil
}

// IsTracked runs git ls-files --error-unmatch. Exit status 1 means the path is
// not in the index. Any other failure (no repository, git missing) is reported
// as tracked: a guard that cannot prove the file untracked must not pass it.
func (g CLIGit) IsTracked(root, path string) bool {
	_, err := g.run().Run(root, "git", "ls-files", "--error-unmatch", "--", path)
	if err == nil {
		return true
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	return true
}
