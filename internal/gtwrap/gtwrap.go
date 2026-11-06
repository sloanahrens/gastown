// Package gtwrap decides what the wrapper runs inside the daemon image: the
// tool it was invoked as, the arguments unchanged, and the environment that
// crosses. The Docker commands themselves are a Runner behind that decision, so
// the decision is a pure function a test drives with no container.
package gtwrap

// Call is one tool invocation inside the daemon image.
type Call struct {
	// Argv is the tool and its arguments, with Argv[0] the tool to run.
	Argv []string
	// Env is the caller's environment filtered to the variables that cross.
	Env []string
	// Dir is the caller's working directory. The town is mounted at the same
	// path inside the container, so this path names the same place in both.
	Dir string
	// TTY says stdin is a terminal, so a terminal has to be attached.
	TTY bool
}

// Runner runs a Call inside the daemon image and returns the tool's own exit
// status unchanged, with an error only when the tool could not be run at all.
type Runner interface {
	// Exec runs call in the running daemon container.
	Exec(call Call) (exit int, err error)
	// Run runs call in a throwaway container of the same image.
	Run(call Call) (exit int, err error)
}
