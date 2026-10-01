package testutil

import (
	"io"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// environment is the process environment the harness reads and rewrites.
// procEnv is the test binary's own; the harness's tests pass a mapEnv, so
// the scrub, the redirects and the go-env carry can be checked without
// touching the process that runs them.
type environment interface {
	LookupEnv(key string) (string, bool)
	Setenv(key, value string) error
	Unsetenv(key string) error
	Environ() []string
}

// procEnv is the process environment.
type procEnv struct{}

func (procEnv) LookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

func (procEnv) Setenv(key, value string) error {
	return os.Setenv(key, value) //testpolicy:allow prod-no-setenv — the harness's job is to rewrite the test process's environment before m.Run
}

func (procEnv) Unsetenv(key string) error {
	return os.Unsetenv(key) //testpolicy:allow prod-no-setenv — the scrub removes inherited live-town variables from the test process
}

func (procEnv) Environ() []string { return os.Environ() }

func getenv(env environment, key string) string {
	v, _ := env.LookupEnv(key)
	return v
}

// harnessHost is everything outside the harness's own logic that it
// touches: the environment, the go and tmux commands, the working directory
// and the live town it resolves from it, the tmux package's default socket,
// the shared Dolt container, the in-process town resolvers it probes, and
// where it reports. processHost is the real one.
type harnessHost struct {
	env           environment
	run           func(name string, args ...string) ([]byte, error)
	lookPath      func(file string) (string, error)
	getwd         func() (string, error)
	findTown      func() (string, error)
	isWorkspace   func(dir string) (bool, error)
	pid           int
	setTmuxSocket func(socket string)
	tmuxSocketDir func() string
	ensureDolt    func() error
	terminateDolt func() error
	startUnitTier func(...unittier.Option) (*unittier.Run, error)
	resolvers     []liveTownResolver
	forbidden     func(dir string) bool
	stderr        io.Writer
	// tempDir is where StartHermetic makes its sandbox; "" is os.TempDir.
	tempDir string
}

// processHost is the harness's real host: the test binary's process.
func processHost() *harnessHost {
	return &harnessHost{
		env: procEnv{},
		run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).Output() //nolint:gosec // callers pass fixed commands
		},
		lookPath:      exec.LookPath,
		getwd:         os.Getwd,
		findTown:      workspace.FindFromCwd,
		isWorkspace:   workspace.IsWorkspace,
		pid:           os.Getpid(),
		setTmuxSocket: tmux.SetDefaultSocket,
		tmuxSocketDir: tmux.SocketDir,
		ensureDolt:    EnsureDoltContainerForTestMain,
		terminateDolt: TerminateDoltContainer,
		startUnitTier: unittier.Start,
		resolvers:     liveTownResolvers,
		forbidden:     workspace.IsForbiddenRoot,
		stderr:        os.Stderr,
	}
}
