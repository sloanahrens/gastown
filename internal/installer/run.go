package installer

import (
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/imagebuild"
)

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
	// Getenv reads the INSTALL_GT_IMAGE_* settings the sandbox image step
	// takes; nil reads the process's (gt-tpg0p.12).
	Getenv func(key string) string
	// ImageRunner runs the sandbox image build; nil runs it for real, in this
	// process before the swap and as a worker process after it (gt-tpg0p.12).
	ImageRunner ImageRunner
	// ImageSupervisor runs one supervised build in this process; nil runs
	// imagebuild.Run itself. It is a seam below ImageRunner, so a test can
	// check what a build before the swap is started with without starting it
	// (gt-tpg0p.12).
	ImageSupervisor func(imagebuild.Config) int
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

// getenv reads one of the run's settings, through the seam when one is set.
func (r *Run) getenv(name string) string {
	if r.Getenv != nil {
		return r.Getenv(name)
	}
	return os.Getenv(name)
}
