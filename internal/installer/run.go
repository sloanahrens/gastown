package installer

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
