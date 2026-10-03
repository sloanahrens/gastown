package doctor

import (
	"errors"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// bdCLI is the bd surface doctor's checks use directly: typed wrappers over
// the subcommands they used to build as raw argv. A *beads.Beads from
// beads.NewPlain provides it, sending exactly the argv, directory and
// environment the raw commands did; unit tests hand the checks a
// beadsfake.Fake instead (CheckContext.openBD).
type bdCLI interface {
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
	SQLCSV(query beadsql.Query) ([][]string, error)
	CountIssues() (int, error)
	TableExists(name string) bool
	StatsJSON() ([]byte, error)
	InitDatabase(opts beads.InitOptions) error
	WispGCCandidates(age time.Duration) ([]string, error)
	Show(id string) (*beads.Issue, error)
}

// bdOpener returns the bdCLI that runs bd in dir with env (nil inherits the
// process environment).
type bdOpener func(dir string, env []string) bdCLI

// bd returns the bd client for dir and env: the real bd CLI, unless the
// context carries an opener.
func (ctx *CheckContext) bd(dir string, env []string) bdCLI {
	if ctx != nil && ctx.openBD != nil {
		return ctx.openBD(dir, env)
	}
	return beads.NewPlain(dir, env)
}

// bdInstalled reports whether checks can run bd: an injected client always
// can, the real one when bd is on PATH.
func (ctx *CheckContext) bdInstalled() bool {
	if ctx != nil && ctx.openBD != nil {
		return true
	}
	_, err := exec.LookPath("bd")
	return err == nil
}

// bdOutput is what a failed bd call printed, stdout then stderr (what the
// raw command's CombinedOutput returned), or the error's text when it did
// not come from a bd process.
func bdOutput(err error) string {
	var cliErr *beads.CLIError
	if errors.As(err, &cliErr) {
		return cliErr.Output()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// bdCause is the process error under a failed bd call (for example "exit
// status 1"), or err itself when it did not come from a bd process.
func bdCause(err error) error {
	var cliErr *beads.CLIError
	if errors.As(err, &cliErr) {
		return cliErr.Err
	}
	return err
}

// environWithPWD is the environment exec.Command gives a child run in dir
// when Env is nil: the process environment plus PWD=dir.
func environWithPWD(dir string) []string {
	return (&exec.Cmd{Dir: dir}).Environ()
}

// doctorBeads is the bead store checks read and repair agent, hook and rig
// beads through: the shared Client plus the *beads.Beads helpers they call.
// *beads.Beads implements it; unit tests answer from beadsfake.
type doctorBeads interface {
	beads.Client
	bdRepairer
	ListAgentBeads() (map[string]*beads.Issue, error)
	ListAgentBeadsFromWisps() (map[string]*beads.Issue, error)
	ListWispIDs() (map[string]bool, error)
	CreateAgentBead(id, title string, fields *beads.AgentFields) (*beads.Issue, error)
}

var _ doctorBeads = (*beads.Beads)(nil)

// beadsSite is where a check opens a bead store: bd's working directory,
// an explicit .beads ("" resolves workDir's), and whether prefix routing is
// off (beads.NewRigLocal).
type beadsSite struct {
	workDir, beadsDir string
	rigLocal          bool
}

// openBeadsAt is the store at site: ctx's opener, else bd.
func (ctx *CheckContext) openBeadsAt(site beadsSite) doctorBeads {
	if ctx != nil && ctx.openBeads != nil {
		return ctx.openBeads(site)
	}
	if site.rigLocal {
		return beads.NewRigLocal(site.workDir)
	}
	return beads.NewWithBeadsDir(site.workDir, site.beadsDir)
}

// beadsAt is beads.New(workDir) through ctx's opener.
func (ctx *CheckContext) beadsAt(workDir string) doctorBeads {
	return ctx.openBeadsAt(beadsSite{workDir: workDir})
}

// beadsWithDir is beads.NewWithBeadsDir through ctx's opener.
func (ctx *CheckContext) beadsWithDir(workDir, beadsDir string) doctorBeads {
	return ctx.openBeadsAt(beadsSite{workDir: workDir, beadsDir: beadsDir})
}

// beadsRigLocal is beads.NewRigLocal through ctx's opener.
func (ctx *CheckContext) beadsRigLocal(dir string) doctorBeads {
	return ctx.openBeadsAt(beadsSite{workDir: dir, rigLocal: true})
}
