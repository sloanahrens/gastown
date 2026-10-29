package bird

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Lookup is the read-only view of the registry Plan decides from: the name a
// bead is bound to, whether a name is ours, the names in use and the pool to
// draw from. It has no write, so a plan whose launch never happens leaves the
// registry as it was (gt-36yqq).
type Lookup interface {
	// Bound returns the newest name bound to bead.
	Bound(bead string) (string, bool, error)
	// Owns reports whether name is one of ours: in the pool, or bound to a bead.
	Owns(name string) (bool, error)
	// Taken returns every name in use, bound, reserved and live alike.
	Taken() (map[string]bool, error)
	// Names returns the pool Plan draws a free name from.
	Names() []string
}

// A Registry is the Lookup Plan decides from; this fails the build if it stops
// being one.
var _ Lookup = (*Registry)(nil)

// Terminal says which of the streams claude reads and writes are terminals,
// since a launch whose input or output is a pipe belongs to whatever is at the
// other end, not to a new session.
type Terminal struct {
	Stdin  bool
	Stdout bool
}

// Decision is what Plan chose: a Launch to run claude under, or a Passthrough
// to run claude as the caller wrote it.
type Decision struct {
	// Launch is the invocation Plan chose, or nil when claude runs as the
	// caller wrote it.
	Launch *Launch
	// Passthrough is what claude runs with when Launch is nil: the caller's own
	// arguments without --bead, which is gt's flag and not one claude has
	// (gt-qipl5). Everything else reaches claude as the caller wrote it.
	Passthrough []string
	// Stderr is a line for the caller to print before claude runs, without a
	// trailing newline; it is empty when there is nothing to say.
	Stderr string
}

// Launch is the claude invocation Plan chose.
type Launch struct {
	// Name is the bird name, and the value of the leading --name.
	Name string
	// Argv is claude's arguments with the program left off: --name Name first,
	// then the caller's own arguments without the flags Plan consumed.
	Argv []string
	// Env is the caller's environment with any earlier identity dropped and
	// this name's identity set.
	Env []string
	// Bead is the bead --bead named, empty when the caller named none; it is
	// the bead the name is bound to and labeled with once the launch is real.
	Bead string
	// Pinned is true when the caller chose the name itself, with --name or with
	// --resume of a bird of ours, so the launch may take the name over from the
	// reservation a spawn made for it (gt-b58mk).
	Pinned bool
}

// agentNameVar is the variable a launch sets to the bird name, and the one a
// running session reads its own name from.
const agentNameVar = "CLAUDE_AGENT_NAME"

// callerIdentity are the variables that say the claude being started is
// already a bird: any one of them set means its caller chose the identity, and
// a second name would replace it.
var callerIdentity = []string{"GT_ROLE", "BD_ACTOR", "BEADS_ACTOR", agentNameVar}

// selfFlags are the arguments that ask claude for something other than a
// session: a printed answer, help, a version, or a continued conversation.
var selfFlags = []string{"-p", "--print", "-h", "--help", "-v", "--version", "-c", "--continue"}

// identityVars is the identity a launch carries, in the order it is appended:
// the session name and the actor beads sees, then the identity commits are
// attributed to.
var identityVars = []struct {
	key   string
	value func(name string) string
}{
	{agentNameVar, func(name string) string { return name }},
	{"BEADS_ACTOR", func(name string) string { return "town/crew/" + name }},
	{"BD_ACTOR", func(name string) string { return "town/crew/" + name }},
	{"GIT_AUTHOR_NAME", func(name string) string { return name }},
	{"GIT_COMMITTER_NAME", func(name string) string { return name }},
	{"GIT_AUTHOR_EMAIL", func(name string) string { return name + "@gastown.local" }},
	{"GIT_COMMITTER_EMAIL", func(name string) string { return name + "@gastown.local" }},
}

// beadShape is the bead id grammar --bead is held to. It is written out rather
// than reusing nameShape, so a change to the name rules cannot silently change
// which beads are accepted (gt-36yqq).
var beadShape = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// poolExhausted is what the caller prints when every name is in use: claude
// still runs, so the line explains the missing name rather than stopping it.
const poolExhausted = "gt bird: every name in the pool is taken; claude runs without one"

// Plan decides from the caller's arguments, environment and terminals whether
// claude runs under a bird name and which, returning the Launch that runs it or
// the Passthrough that leaves claude as the caller wrote it.
func Plan(args, env []string, term Terminal, lookup Lookup) (Decision, error) {
	p := parseArgs(args)
	pass := Decision{Passthrough: p.passthrough}
	if alreadyNamed(env) {
		return pass, nil
	}
	if !term.Stdin || !term.Stdout {
		return pass, nil
	}
	if p.self {
		return pass, nil
	}
	// A first word with no whitespace names one of claude's own subcommands,
	// so claude is asked for that and not for a session to run in.
	if len(p.words) > 0 && !isPrompt(p.words[0]) {
		return pass, nil
	}
	if p.beadSet && !beadShape.MatchString(p.bead) {
		return Decision{}, &UsageError{
			Arg:   fmt.Sprintf("--bead %q", p.bead),
			Wants: "a bead id matches " + beadShape.String(),
		}
	}
	d, err := decide(p, env, lookup)
	if err != nil {
		return Decision{}, fmt.Errorf("planning the claude launch: %w", err)
	}
	return d, nil
}

// decide returns the decision for arguments whose streams and flags leave claude
// to run under a name.
func decide(p parsed, env []string, lookup Lookup) (Decision, error) {
	pass := Decision{Passthrough: p.passthrough}
	switch {
	case p.nameSet:
		if !ValidName(p.name) {
			// The name the caller typed is claude's to refuse; replacing it
			// with one of ours would hide the mistake.
			return pass, nil
		}
		return decisionFor(p.name, p, env), nil
	case p.resumeSet:
		ours, err := lookup.Owns(p.resume)
		if err != nil {
			return Decision{}, err
		}
		if !ours {
			// A session that is not ours resumes under its own name and its
			// own identity, so claude runs as the caller wrote it.
			return pass, nil
		}
		return decisionFor(p.resume, p, env), nil
	case p.beadSet:
		name, bound, err := lookup.Bound(p.bead)
		if err != nil {
			return Decision{}, err
		}
		if bound {
			return decisionFor(name, p, env), nil
		}
	}
	name, free, err := freeName(lookup)
	if err != nil {
		return Decision{}, err
	}
	if !free {
		return Decision{Passthrough: p.passthrough, Stderr: poolExhausted}, nil
	}
	return decisionFor(name, p, env), nil
}

// decisionFor is the launch under name, with the caller's own arguments minus
// the flags Plan consumed and the identity this name brings.
func decisionFor(name string, p parsed, env []string) Decision {
	return Decision{Launch: &Launch{
		Name:   name,
		Argv:   argvFor(name, p.argv),
		Env:    launchEnv(env, name),
		Bead:   p.bead,
		Pinned: p.nameSet || p.resumeSet,
	}}
}

// argvFor puts --name first, since claude reads the session name off its own
// arguments and Plan removed whatever --name the caller wrote.
func argvFor(name string, args []string) []string {
	argv := make([]string, 0, 2+len(args))
	argv = append(argv, "--name", name)
	return append(argv, args...)
}

// freeName returns the first pool name nothing holds. The pool is taken in its
// own order, so a caller that loses a race for the name it was given re-plans
// to the next free one rather than drawing the taken name again (gt-36yqq).
func freeName(lookup Lookup) (string, bool, error) {
	taken, err := lookup.Taken()
	if err != nil {
		return "", false, err
	}
	for _, name := range lookup.Names() {
		if ValidName(name) && !taken[name] {
			return name, true, nil
		}
	}
	return "", false, nil
}

// launchEnv returns env with any earlier definition of the identity dropped, so
// two launches never inherit each other's git author, and this name's identity
// appended. Only a git identity can be there to drop: the four variables that
// gate a launch are absent by the time it is planned.
func launchEnv(env []string, name string) []string {
	out := make([]string, 0, len(env)+len(identityVars))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if isIdentityKey(key) {
			continue
		}
		out = append(out, kv)
	}
	for _, v := range identityVars {
		out = append(out, v.key+"="+v.value(name))
	}
	return out
}

// isIdentityKey reports whether key is one of the variables a launch sets.
func isIdentityKey(key string) bool {
	for _, v := range identityVars {
		if v.key == key {
			return true
		}
	}
	return false
}

// alreadyNamed reports whether env carries an identity this launch would
// replace.
func alreadyNamed(env []string) bool {
	return slices.ContainsFunc(callerIdentity, func(key string) bool { return envHas(env, key) })
}

// envHas reports whether env defines key at all, empty or not, which is how the
// shell's ${key+x} reads a variable.
func envHas(env []string, key string) bool {
	prefix := key + "="
	return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, prefix) })
}

// envGet is the last value env gives key, as a process reads its own
// environment.
func envGet(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = kv[len(prefix):]
		}
	}
	return value
}

// isPrompt reports whether word is a prompt rather than one of claude's
// subcommands, which hold no whitespace.
func isPrompt(word string) bool {
	return strings.ContainsFunc(word, unicode.IsSpace)
}

// parsed is the caller's command line as Plan reads it.
type parsed struct {
	name      string
	bead      string
	resume    string
	nameSet   bool
	beadSet   bool
	resumeSet bool
	// self is true when the caller asked claude for itself with a flag.
	self bool
	// words holds every argument that is neither a flag nor a flag's value.
	words []string
	// argv holds what claude receives under a name, and passthrough what it
	// receives when it runs as the caller wrote it: only --bead, gt's own
	// flag, leaves both (gt-qipl5).
	argv        []string
	passthrough []string
}

// parseArgs classifies the arguments once, so a value consumed by --name,
// --bead or --resume is never read as a flag or as claude's own subcommand
// (gt-36yqq).
func parseArgs(args []string) parsed {
	var p parsed
	for i := 0; i < len(args); i++ {
		if flag, value, used, ok := ownFlag(args[i:]); ok {
			// kept is the flag as the caller wrote it, --resume=x or --resume x
			// alike, for the argv that hands it back to claude.
			kept := slices.Clone(args[i : i+used])
			switch flag {
			case "--name":
				p.name, p.nameSet = value, true
			case "--bead":
				p.bead, p.beadSet = value, true
				kept = nil
			case "--resume":
				p.resume, p.resumeSet = value, true
				// claude reads --resume itself, so it keeps it and its value.
				p.argv = append(p.argv, kept...)
			}
			p.passthrough = append(p.passthrough, kept...)
			i += used - 1
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			if slices.Contains(selfFlags, args[i]) {
				p.self = true
			}
			p.argv = append(p.argv, args[i])
			p.passthrough = append(p.passthrough, args[i])
			continue
		}
		p.words = append(p.words, args[i])
		p.argv = append(p.argv, args[i])
		p.passthrough = append(p.passthrough, args[i])
	}
	return p
}

// ownFlag reads the first argument as one of the flags Plan reads: its value is
// inline (--name=x) or the next argument, used is how many arguments that took,
// and ok is false for every other argument.
func ownFlag(args []string) (flag, value string, used int, ok bool) {
	arg := args[0]
	for _, key := range []string{"--name", "--bead", "--resume"} {
		if arg == key {
			if len(args) > 1 {
				return key, args[1], 2, true
			}
			return key, "", 1, true
		}
		if rest, found := strings.CutPrefix(arg, key+"="); found {
			return key, rest, 1, true
		}
	}
	return "", "", 0, false
}

// UsageError is an argument Plan refuses, carrying the status the caller exits
// with once it has printed the error.
type UsageError struct {
	// Arg is the flag and the value it was given, as the caller wrote them.
	Arg string
	// Wants says what a usable value is.
	Wants string
}

// Error is the line the caller prints before exiting.
func (e *UsageError) Error() string {
	return e.Arg + ": " + e.Wants
}

// ExitStatus is the status the caller exits with after printing the error, as
// a shell refused a malformed argument.
func (e *UsageError) ExitStatus() int { return 2 }
