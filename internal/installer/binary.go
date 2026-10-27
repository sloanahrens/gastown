package installer

import (
	"encoding/json"
	"os"
	"regexp"
)

// Binary reads the commit the gt binary at a path was built from: what an
// install would replace. It is a seam because the read runs the binary.
type Binary struct {
	// Commit is the commit gt reports, usually short, or "" when it says
	// nothing a checkout can resolve.
	Commit func(dir, gt string) string
}

// StaleBinary is the real read: 'gt stale --json' binary_commit first, 'gt
// version' as the fallback (lib/install-gt-lib.sh's igt_binary_commit_raw). run
// executes the commands; nil runs them for real. Only stdout is read, as the
// script's pipelines into python do, and a read that did not exit 0 answers
// nothing.
func StaleBinary(run Execute) Binary {
	if run == nil {
		run = RealExecute
	}
	return Binary{Commit: func(dir, gt string) string {
		if !isExecutable(gt) {
			return ""
		}
		if out, _, code := run(dir, gt, "stale", "--json"); code == 0 {
			if commit := staleCommit(out); commit != "" {
				return commit
			}
		}
		out, _, code := run(dir, gt, "version")
		if code != 0 {
			return ""
		}
		return versionCommit(out)
	}}
}

// staleCommit is 'gt stale --json' binary_commit, or "" when the output is not
// that JSON.
func staleCommit(out []byte) string {
	var stale struct {
		BinaryCommit string `json:"binary_commit"`
	}
	if json.Unmarshal(out, &stale) != nil {
		return ""
	}
	return stale.BinaryCommit
}

// versionPattern matches the commit in 'gt version'. From a cwd outside a repo
// the line carries no "@", so the ref before the commit is optional (gt-b5mpe).
var versionPattern = regexp.MustCompile(`\([^:()]+: ([^@()]*@)?([0-9a-f]{7,40})\)`)

// versionCommit is the commit in 'gt version', or "" when the line names none.
func versionCommit(out []byte) string {
	m := versionPattern.FindSubmatch(out)
	if m == nil {
		return ""
	}
	return string(m[2])
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
