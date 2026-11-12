// Package gtwrap holds what the Mac wrapper (cmd/gt-wrap) needs to run a tool
// inside the daemon image: the docker and kubernetes targets, the choice of
// tool and of the environment that crosses, and the reader of a worktree's
// branch and HEAD. The commands themselves are a Runner behind that decision,
// so the decision is a pure function a test drives with no container.
// cmd/gt-wrap decides which calls change: it adds gt done's worktree facts
// (withWorktreeFacts) before the call is planned.
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
