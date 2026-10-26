package installer

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
)

// Binary reads the commit the gt binary at a path was built from: what an
// install would replace. It is a seam because the read runs the binary.
type Binary struct {
	// Commit is the commit gt reports, usually short, or "" when it says
	// nothing a checkout can resolve.
	Commit func(dir, gt string) string
}

// Command runs one command from dir and returns its combined output.
type Command func(dir, name string, args ...string) ([]byte, error)

// StaleBinary is the real read: 'gt stale --json' binary_commit first, 'gt
// version' as the fallback (lib/install-gt-lib.sh's igt_binary_commit_raw). run
// executes the commands; nil runs them for real.
func StaleBinary(run Command) Binary {
	if run == nil {
		run = func(dir, name string, args ...string) ([]byte, error) {
			cmd := exec.Command(name, args...)
			cmd.Dir = dir
			return cmd.CombinedOutput()
		}
	}
	return Binary{Commit: func(dir, gt string) string {
		if !isExecutable(gt) {
			return ""
		}
		if out, err := run(dir, gt, "stale", "--json"); err == nil {
			if commit := staleCommit(out); commit != "" {
				return commit
			}
		}
		out, err := run(dir, gt, "version")
		if err != nil {
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
