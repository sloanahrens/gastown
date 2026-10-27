package installer

import "os/exec"

// Run is one install: the arguments it was given, the exit contract it reports
// through, the paths it writes, and the seams it reads and moves files with.
type Run struct {
	Cfg   Config
	C     *Contract
	Paths Paths
	Repo  Repo
	// The zero Files is the real filesystem.
	Files  Files
	Bin    Binary
	Target Target
	// Sign is the signing material the installed binary is signed with; the
	// zero Signing signs nothing (Sign warns and installs ad-hoc).
	Signing Signing
	// Cmd runs the external commands the build, install, signing, smoke and
	// sync steps make; nil runs them for real.
	Cmd Execute
	// Found is a command named on PATH, the script's `command -v`; nil asks
	// exec.LookPath.
	Found func(name string) bool
}

// NewRun is a run against the real checkout at paths.RigDir, the real
// filesystem, and the gt binary the paths name.
func NewRun(cfg Config, c *Contract, paths Paths) *Run {
	return &Run{
		Cfg:   cfg,
		C:     c,
		Paths: paths,
		Repo:  GitRepo(paths.RigDir, c.Exec),
		Bin:   StaleBinary(nil),
	}
}

// found reports whether name is on PATH, through the seam when one is set.
func (r *Run) found(name string) bool {
	if r.Found != nil {
		return r.Found(name)
	}
	_, err := exec.LookPath(name)
	return err == nil
}
