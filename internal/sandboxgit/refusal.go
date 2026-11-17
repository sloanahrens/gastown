// Package sandboxgit states which git calls may cross into a sandbox.
//
// The in-sandbox runner sends one git command per call, and it cannot tell a
// branch from another command, so a call is checked by its verb and its shape
// before anything is sent. Refusal is that rule, shared by the runner and by a
// caller that models it.
package sandboxgit

import (
	"fmt"
	"path"
	"strings"
)

// Refusal returns the error for a git call that may not be sent into the
// sandbox named label, or nil when it may. A call is refused when its clone
// directory is empty, relative, or holds ".." or a newline, when its first
// argument is not a git verb the overrun save runs, or when an argument is an
// option able to change what git runs or holds a newline or NUL. The error
// names label, the sandbox, and, when one is known, the git verb.
func Refusal(label, dir string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sandbox %s: refusing a git call with no verb", label)
	}
	verb := args[0]
	if !verbs[verb] {
		return fmt.Errorf("sandbox %s: refusing git verb %q: not a command the overrun save runs", label, verb)
	}
	switch {
	case dir == "":
		return fmt.Errorf("sandbox %s: refusing git %s: clone directory is empty", label, verb)
	case !path.IsAbs(dir):
		return fmt.Errorf("sandbox %s: refusing git %s: clone directory %q is not absolute", label, verb, dir)
	case strings.Contains(dir, ".."):
		return fmt.Errorf("sandbox %s: refusing git %s: clone directory %q holds ..", label, verb, dir)
	case strings.Contains(dir, "\n"):
		return fmt.Errorf("sandbox %s: refusing git %s: clone directory %q holds a newline", label, verb, dir)
	}
	for _, arg := range args[1:] {
		switch {
		case strings.ContainsAny(arg, "\n\x00"):
			return fmt.Errorf("sandbox %s: refusing git %s argument %q: it holds a newline or NUL", label, verb, arg)
		case deniedOption(arg):
			return fmt.Errorf("sandbox %s: refusing git %s argument %q: it can change what git runs", label, verb, arg)
		}
	}
	return nil
}

// verbs are the git verbs the overrun save runs in a sandbox clone. Anything
// else is refused: with no way to tell a branch from another command, only the
// verbs the save uses reach git.
var verbs = map[string]bool{
	"status": true, "add": true, "commit": true, "push": true, "fetch": true,
	"rev-parse": true, "symbolic-ref": true, "log": true, "diff": true,
	"reset": true, "ls-remote": true, "cherry": true, "merge-base": true,
	"cat-file": true,
}

// deniedLong are the long options that change what git runs. git accepts any
// unambiguous prefix of a long option, so a name is refused when it is a prefix
// of one of these, not only when it matches exactly.
var deniedLong = []string{
	"--git-dir", "--work-tree", "--exec-path", "--upload-pack",
	"--receive-pack", "--exec", "--config-env",
}

// deniedOption reports whether arg is an option that changes what git runs:
// another config (-c, --config-env), directory (-C, --git-dir, --work-tree),
// executable (--exec-path, --exec), or remote helper (--upload-pack,
// --receive-pack). It refuses the exact short flags -c and -C; an argument
// after the verb is never git's global option, so an attached form such as
// -Cdir is left to the subcommand. A long option is refused when its name, with
// or without "=", is a prefix of a denied one, so an abbreviation git would
// accept is refused too. The denylist covers options that change what git runs,
// not what it destroys: limiting a push or a reset is the caller's job.
func deniedOption(arg string) bool {
	if arg == "-c" || arg == "-C" {
		return true
	}
	if !strings.HasPrefix(arg, "--") {
		return false
	}
	name, _, _ := strings.Cut(arg, "=")
	if len(name) <= 2 {
		return false
	}
	for _, opt := range deniedLong {
		if strings.HasPrefix(opt, name) {
			return true
		}
	}
	return false
}
