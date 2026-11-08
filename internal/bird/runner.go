package bird

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// Runner runs the gt bird operations: starting claude under a bird name,
// binding the running session's name to a bead, and listing the live sessions
// (gt-qipl5). Every process, file and bd call it makes is a field, so a unit
// test drives each one without starting anything.
type Runner struct {
	// Registry is the state the sessions are recorded in.
	Registry *Registry
	// Claude is the real claude binary a start replaces itself with.
	Claude string
	// Home is the operator's home directory, which holds the bead stores a
	// start labels.
	Home string
	// Terminal says which of the caller's streams are terminals, which is where
	// Plan reads whether the invocation belongs to a new session.
	Terminal Terminal
	// Environ is the caller's environment; nil means the process's own.
	Environ func() []string
	// Stat reads the claude binary's file info; nil means os.Stat.
	Stat func(string) (fs.FileInfo, error)
	// PID is the process a start records as the session's; nil means this one.
	PID func() int
	// Getwd is the directory a start records; nil means os.Getwd.
	Getwd func() (string, error)
	// Exec replaces this process with path, as syscall.Exec does; nil means
	// syscall.Exec.
	Exec func(path string, argv, env []string) error
	// Cmux runs one cmux command line and returns what it printed; nil runs the
	// real cmux, so a test asserts a spawn's argv without opening a tab
	// (gt-b58mk).
	Cmux func(argv []string) ([]byte, error)
	// LabelBead adds label to bead in the store directory that holds it; nil
	// adds no label. Start runs it best effort, so a bd that cannot write the
	// label is a warning rather than a session that does not start.
	LabelBead func(dir, bead, label string) error
	// UnlabelBead removes label from bead in the store that holds it; nil
	// removes none. A start whose exec fails takes back the label it added.
	UnlabelBead func(dir, bead, label string) error
	// Stdout is where a command's answer goes and Stderr where its warnings go;
	// nil means the process's own streams.
	Stdout io.Writer
	Stderr io.Writer
}

// MissingClaudeError is a start with no claude it can run, carrying the status
// a shell gives a command that is not there.
type MissingClaudeError struct {
	// Path is the binary that could not run, empty when none was named.
	Path string
	// Err says why it could not run.
	Err error
}

// Error names the binary and why it cannot run.
func (e *MissingClaudeError) Error() string {
	if e.Path == "" {
		return "gt bird: no claude binary to run"
	}
	return fmt.Sprintf("gt bird: cannot run %s: %v", e.Path, e.Err)
}

// Unwrap is the read or the mode check that refused the binary.
func (e *MissingClaudeError) Unwrap() error { return e.Err }

// ExitStatus is 127, the status a shell exits with for a missing command.
func (e *MissingClaudeError) ExitStatus() int { return 127 }

// Start replaces this process with claude, under a bird name when Plan chose
// one: the name is claimed and its bead bound and labeled before the exec, so
// the pid the record names is the claude process itself.
func (r *Runner) Start(args []string) error {
	if err := r.checkClaude(); err != nil {
		return err
	}
	d, err := r.launch(args)
	if err != nil {
		return err
	}
	if d.Stderr != "" {
		fmt.Fprintln(r.stderr(), d.Stderr)
	}
	argv, env := d.Passthrough, r.environ()
	var bound, labeled bool
	if d.Launch != nil {
		bound, labeled, err = r.bindBead(d.Launch)
		if err != nil {
			return r.unclaim(d.Launch, bound, err)
		}
		argv, env = d.Launch.Argv, d.Launch.Env
	}
	err = r.exec(r.Claude, append([]string{r.Claude}, argv...), env)
	if err != nil && d.Launch != nil {
		// No session runs under the name, so the record, the binding and the
		// label this launch added must not hold it as though one did.
		if labeled {
			r.unlabel(d.Launch.Bead, d.Launch.Name)
		}
		return r.unclaim(d.Launch, bound, err)
	}
	return err
}

// unclaim drops the record and, when bound says this launch added it, the
// binding a start made before it failed. A rollback that fails too is added to
// err, since the record may then still hold the name.
func (r *Runner) unclaim(l *Launch, bound bool, err error) error {
	if rbErr := r.Registry.Unclaim(l.Name, r.pid(), l.Bead, bound); rbErr != nil {
		return fmt.Errorf("%w; rolling the claim back also failed: %v", err, rbErr)
	}
	return err
}

// Bind binds the name in this session's environment to bead, in one write with
// the bead's agent record, and reports a bead that last ran under another name,
// since that is the session resuming the bead (gt-qipl5).
func (r *Runner) Bind(bead string) error {
	name := envGet(r.environ(), agentNameVar)
	if name == "" {
		return fmt.Errorf("gt bird bind: %s is not set; run it inside a bird session", agentNameVar)
	}
	if !ValidName(name) {
		return fmt.Errorf("gt bird bind: %s %q: %w", agentNameVar, name, ErrInvalidName)
	}
	if !beadShape.MatchString(bead) {
		return &UsageError{Arg: "gt bird bind " + bead, Wants: "a bead id matches " + beadShape.String()}
	}
	var prior string
	var had bool
	err := r.Registry.Update(func(st *State) error {
		prior, had = st.Bound(bead)
		st.Bind(name, bead, r.Registry.now())
		r.recordBead(st, name, bead)
		return nil
	})
	if err != nil {
		return fmt.Errorf("binding %s to %s: %w", name, bead, err)
	}
	if had && prior != name {
		fmt.Fprintf(r.stdout(), "%s last ran as %s; it is bound to %s now\n", bead, prior, name)
	}
	return nil
}

// List prints the live sessions, pruning the records of the ones whose process
// has gone.
func (r *Runner) List(w io.Writer) error {
	agents, err := r.Registry.List()
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		fmt.Fprintln(w, "no live birds")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBEAD\tCWD")
	for _, a := range agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, dash(a.Bead), dash(a.Cwd))
	}
	return tw.Flush()
}

// launch decides claude's invocation and claims the name it runs under. Plan
// only reads the registry and never reserves a name, so the name it chose may be
// claimed by another start before this one reaches the lock; when that happens Plan is
// asked again, which now sees the name held and takes the next free one. A name
// the caller pinned comes back the same every time, so a second refusal of it
// is the answer.
func (r *Runner) launch(args []string) (Decision, error) {
	env := r.environ()
	tried := make(map[string]bool)
	for {
		d, err := Plan(args, env, r.Terminal, r.Registry)
		if err != nil {
			return Decision{}, err
		}
		if d.Launch == nil {
			return d, nil
		}
		claimed, err := r.claim(d.Launch)
		if err != nil {
			return Decision{}, err
		}
		if claimed {
			return d, nil
		}
		if tried[d.Launch.Name] {
			return Decision{}, fmt.Errorf("gt bird: the name %s is already running; stop it or start under another name", d.Launch.Name)
		}
		tried[d.Launch.Name] = true
	}
}

// claim records this process as the agent holding the launch's name, with the
// bead it runs and the directory it runs in. It fails rather than record a
// process the kernel does not report as running, or one it gives no start token
// for, since such a record names no process. A launch whose name the caller
// chose takes the name over from the reservation a spawn made for it.
func (r *Runner) claim(l *Launch) (bool, error) {
	pid := r.pid()
	start, live, err := r.procStart()(pid)
	if err != nil {
		return false, fmt.Errorf("reading the start token of pid %d: %w", pid, err)
	}
	if !live || start == "" {
		return false, fmt.Errorf("reading the start token of pid %d: the kernel gives none for a running process", pid)
	}
	a := Agent{Name: l.Name, PID: pid, Start: start, Bead: l.Bead, Cwd: r.cwd()}
	if l.Pinned {
		return r.Registry.ClaimPinned(a)
	}
	return r.Registry.Claim(a)
}

// bindBead records that the launch's name ran the bead it named and labels the
// bead agent:<name>, so bind and list can find the session later. It reports
// whether the binding was new and whether the label was written, which is what
// a failed exec has to undo.
func (r *Runner) bindBead(l *Launch) (added, labeled bool, err error) {
	if l.Bead == "" {
		return false, false, nil
	}
	added, err = r.Registry.Bind(l.Name, l.Bead)
	if err != nil {
		return false, false, fmt.Errorf("recording that %s ran %s: %w", l.Name, l.Bead, err)
	}
	return added, r.label(l.Bead, l.Name), nil
}

// label tags bead agent:<name> and reports whether the tag was written.
func (r *Runner) label(bead, name string) bool {
	if r.LabelBead == nil {
		return false
	}
	if err := r.LabelBead(r.beadDir(bead), bead, "agent:"+name); err != nil {
		fmt.Fprintf(r.stderr(), "gt bird: %s is not labeled agent:%s: %v\n", bead, name, err)
		return false
	}
	return true
}

// unlabel takes back the agent:<name> tag a launch that never ran put on bead.
// Like label it is best effort: the next bind of the bead writes it again.
func (r *Runner) unlabel(bead, name string) {
	if r.UnlabelBead == nil {
		return
	}
	if err := r.UnlabelBead(r.beadDir(bead), bead, "agent:"+name); err != nil {
		fmt.Fprintf(r.stderr(), "gt bird: %s still carries agent:%s: %v\n", bead, name, err)
	}
}

// beadDir is the store a bead is labeled in: ~/.claude for the claude-* beads,
// ~/gt for the town's own, since a bead is only found from the directory its
// store is rooted in.
func (r *Runner) beadDir(bead string) string {
	if strings.HasPrefix(bead, "claude-") {
		return filepath.Join(r.Home, ".claude")
	}
	return filepath.Join(r.Home, "gt")
}

// recordBead points name's record at bead, keeping the pid and the directory it
// was recorded with. A name the state does not hold is left alone: the record
// is the session's own statement of what runs, and bind does not invent one.
func (r *Runner) recordBead(st *State, name, bead string) {
	for i := range st.Agents {
		if st.Agents[i].Name == name {
			st.Agents[i].Bead = bead
			return
		}
	}
}

// checkClaude fails before a name is claimed for a session that could never
// run, so a missing claude leaves the registry as it was.
func (r *Runner) checkClaude() error {
	if r.Claude == "" {
		return &MissingClaudeError{}
	}
	info, err := r.statFile(r.Claude)
	if err != nil {
		return &MissingClaudeError{Path: r.Claude, Err: err}
	}
	// claude is the operator's own install, so the owner's execute bit is the
	// one that says it runs; a group or other bit alone does not.
	if info.IsDir() || info.Mode().Perm()&0o100 == 0 {
		return &MissingClaudeError{Path: r.Claude, Err: errors.New("it is not an executable file")}
	}
	return nil
}

// dash is what a column prints for a value the record does not have, so an
// empty one never reads as a blank.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (r *Runner) environ() []string {
	if r.Environ != nil {
		return r.Environ()
	}
	return os.Environ()
}

func (r *Runner) pid() int {
	if r.PID != nil {
		return r.PID()
	}
	return os.Getpid()
}

// cwd is the directory a record names: a session whose directory is gone still
// runs, so a getwd that fails leaves the record without one.
func (r *Runner) cwd() string {
	getwd := r.Getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	cwd, err := getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func (r *Runner) statFile(path string) (fs.FileInfo, error) {
	if r.Stat != nil {
		return r.Stat(path)
	}
	return os.Stat(path)
}

// procStart is the registry's process table, so a start records a token the
// pruning that follows reads the same way.
func (r *Runner) procStart() func(int) (string, bool, error) {
	return r.Registry.procStart()
}

func (r *Runner) exec(path string, argv, env []string) error {
	if r.Exec != nil {
		return r.Exec(path, argv, env)
	}
	return execProcess(path, argv, env)
}

// cmux runs one cmux command line through the runner's seam, or the real cmux
// when a test injected none.
func (r *Runner) cmux(argv []string) ([]byte, error) {
	if r.Cmux != nil {
		return r.Cmux(argv)
	}
	return runCmux(argv)
}

func (r *Runner) stdout() io.Writer {
	if r.Stdout != nil {
		return r.Stdout
	}
	return os.Stdout
}

func (r *Runner) stderr() io.Writer {
	if r.Stderr != nil {
		return r.Stderr
	}
	return os.Stderr
}
