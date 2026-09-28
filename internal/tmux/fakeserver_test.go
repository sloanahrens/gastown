package tmux

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jonboulle/clockwork"
)

// fakeServer is a small stateful model of a tmux server plus the process
// table ps and kill see, answering through a scripted runner. It knows the
// subcommands the package sends and the #{...} formats it reads; anything
// else answers empty, like a tmux call whose output the caller ignores.
//
// It is for the package's own unit tests: tmuxfake models the consumer-facing
// surface, this models the argv the Tmux methods produce.
type fakeServer struct {
	mu       sync.Mutex
	sessions map[string]*fsess
	order    []string // session creation order
	procs    map[string]*fproc
	killed   []string // "SIG pid", in order
	nextPID  int
	nextPane int
	nextSess int
	global   map[string]string
	noServer bool
	// onCall, when set, runs (under the lock) before each tmux call is
	// answered, so a test can change state mid-sequence.
	onCall func(c tmuxCall)
}

type fsess struct {
	name     string
	id       string
	env      map[string]string
	panes    []*fpane
	activity int64 // session_activity
}

type fpane struct {
	id         string
	window     int
	index      int
	cmd        string
	pid        string
	path       string
	content    string
	dead       bool
	deadStatus string
	activity   int64 // window_activity
	keys       []string
	// composer, when set, makes the pane behave like an agent's input box:
	// literal send-keys text is typed into it and Enter submits it into the
	// transcript. capture-pane then renders transcript + prompt + composer.
	composer   *composer
	windowSize string // window-size option of the pane's window
}

type composer struct {
	transcript []string
	input      string
	submitted  []string
}

func (c *composer) render() string {
	return strings.Join(append(append([]string(nil), c.transcript...), "❯ "+c.input), "\n")
}

// withComposer gives pane p an agent input box with the given transcript.
func (f *fakeServer) withComposer(p *fpane, transcript ...string) {
	f.with(func() { p.composer = &composer{transcript: transcript} })
}

func (f *fakeServer) submitted(p *fpane) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), p.composer.submitted...)
}

func (f *fakeServer) paneKeys(p *fpane) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), p.keys...)
}

type fproc struct {
	pid, ppid, pgid, comm string
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		sessions: map[string]*fsess{},
		procs:    map[string]*fproc{},
		nextPID:  1000,
		global:   map[string]string{},
	}
}

// tmux returns a Tmux on socket gt-test-unit backed by f, on clk (a new fake
// clock when nil), and the scripted runner recording its calls.
func (f *fakeServer) tmux(clk clockwork.Clock) (*Tmux, *scripted) {
	s := newScripted(f.answer)
	return unitTmux(s, clk), s
}

// addSession creates a session whose first pane runs cmd (a shell when "").
func (f *fakeServer) addSession(name, cmd string) *fsess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addSessionLocked(name, "", cmd, nil)
}

func (f *fakeServer) addSessionLocked(name, dir, cmd string, env map[string]string) *fsess {
	f.nextSess++
	s := &fsess{name: name, id: fmt.Sprintf("$%d", f.nextSess), env: map[string]string{}}
	for k, v := range env {
		s.env[k] = v
	}
	f.sessions[name] = s
	f.order = append(f.order, name)
	f.addPaneLocked(s, dir, cmd)
	return s
}

func (f *fakeServer) addPaneLocked(s *fsess, dir, cmd string) *fpane {
	if cmd == "" {
		cmd = "bash"
	}
	f.nextPane++
	pid := f.spawnLocked("1", cmd)
	p := &fpane{id: fmt.Sprintf("%%%d", f.nextPane), cmd: cmd, pid: pid, path: dir}
	for _, q := range s.panes {
		if q.window == 0 {
			p.index++
		}
	}
	s.panes = append(s.panes, p)
	return p
}

// addWindow opens a new window in session name running cmd and returns its
// pane. tmux makes a new window the active one.
func (f *fakeServer) addWindow(name, cmd string) *fpane {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addWindowLocked(f.sessions[name], cmd)
}

func (f *fakeServer) addWindowLocked(s *fsess, cmd string) *fpane {
	if cmd == "" {
		cmd = "bash"
	}
	w := 0
	for _, q := range s.panes {
		if q.window >= w {
			w = q.window + 1
		}
	}
	f.nextPane++
	p := &fpane{id: fmt.Sprintf("%%%d", f.nextPane), window: w, cmd: cmd, pid: f.spawnLocked("1", cmd)}
	s.panes = append(s.panes, p)
	return p
}

// addPane splits session name, returning the new pane (running cmd).
func (f *fakeServer) addPane(name, cmd string) *fpane {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addPaneLocked(f.sessions[name], "", cmd)
}

// spawn adds a process to the table and returns its pid.
func (f *fakeServer) spawn(ppid, comm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawnLocked(ppid, comm)
}

func (f *fakeServer) spawnLocked(ppid, comm string) string {
	f.nextPID++
	pid := strconv.Itoa(f.nextPID)
	pgid := pid
	if p := f.procs[ppid]; p != nil {
		pgid = p.pgid
	}
	f.procs[pid] = &fproc{pid: pid, ppid: ppid, pgid: pgid, comm: comm}
	return pid
}

// setProc overrides a process's parent and group (to model reparenting).
func (f *fakeServer) setProc(pid, ppid, pgid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.procs[pid]
	p.ppid, p.pgid = ppid, pgid
}

func (f *fakeServer) session(name string) *fsess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[name]
}

func (f *fakeServer) has(name string) bool { return f.session(name) != nil }

func (f *fakeServer) kills() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

func (f *fakeServer) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// resolve maps a target to its session and pane. Accepted forms: name,
// =name, name:^, name:W, name:W.P, %id.
func (f *fakeServer) resolve(target string) (*fsess, *fpane) {
	if strings.HasPrefix(target, "%") {
		for _, n := range f.order {
			s := f.sessions[n]
			if s == nil {
				continue
			}
			for _, p := range s.panes {
				if p.id == target {
					return s, p
				}
			}
		}
		return nil, nil
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(target, "="), ":")
	s := f.sessions[name]
	if s == nil || len(s.panes) == 0 {
		return s, nil
	}
	if rest == "" || rest == "^" {
		return s, s.panes[0]
	}
	ws, ps, hasPane := strings.Cut(rest, ".")
	w, err := strconv.Atoi(ws)
	if err != nil {
		return s, nil
	}
	pi := 0
	if hasPane {
		if pi, err = strconv.Atoi(ps); err != nil {
			return s, nil
		}
	}
	for _, p := range s.panes {
		if p.window == w && p.index == pi {
			return s, p
		}
	}
	return s, nil
}

var formatVar = regexp.MustCompile(`#\{([a-z_]+)\}`)

func (f *fakeServer) expand(format string, s *fsess, p *fpane) string {
	return formatVar.ReplaceAllStringFunc(format, func(m string) string {
		switch m[2 : len(m)-1] {
		case "session_name":
			return s.name
		case "session_id":
			return s.id
		case "session_windows":
			return "1"
		case "session_attached":
			return "0"
		case "session_activity":
			return strconv.FormatInt(s.activity, 10)
		case "window_activity":
			if p != nil {
				return strconv.FormatInt(p.activity, 10)
			}
		case "pane_id":
			return p.id
		case "pane_pid":
			return p.pid
		case "pane_current_command":
			return p.cmd
		case "pane_current_path":
			return p.path
		case "pane_index":
			return strconv.Itoa(p.index)
		case "window_index":
			return strconv.Itoa(p.window)
		case "pane_dead":
			if p.dead {
				return "1"
			}
			return "0"
		case "pane_dead_status":
			return p.deadStatus
		case "window_width":
			return "80"
		}
		return ""
	})
}

// flagValue returns the value after flag in args.
func flagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func missing(target string) reply { return fail("can't find session: " + target) }

// paneCommandFor is what tmux reports as pane_current_command after running
// command: exec/env prefixes and assignments are skipped, then the base name.
func paneCommandFor(command string) string {
	for _, w := range strings.Fields(command) {
		if w == "exec" || w == "env" || (strings.Contains(w, "=") && !strings.HasPrefix(w, "/")) {
			continue
		}
		return filepath.Base(w)
	}
	return "bash"
}

func (f *fakeServer) answer(c tmuxCall) reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(c)
	}
	switch c.name {
	case "ps":
		return f.ps(c.args)
	case "kill":
		if len(c.args) == 2 {
			sig, pid := strings.TrimPrefix(c.args[0], "-"), c.args[1]
			f.killed = append(f.killed, sig+" "+pid)
			delete(f.procs, pid)
		}
		return ok("")
	case "tmux":
	default:
		return ok("")
	}
	if f.noServer && c.sub() != "new-session" {
		return fail("no server running on /tmp/tmux-501/gt-test-unit")
	}
	a := c.args
	target := flagValue(a, "-t")
	switch c.sub() {
	case "has-session":
		if s, _ := f.resolve(target); s == nil {
			return missing(target)
		}
	case "new-session":
		name := flagValue(a, "-s")
		if f.sessions[name] != nil {
			return fail("duplicate session: " + name)
		}
		env := map[string]string{}
		for i := 0; i+1 < len(a); i++ {
			if a[i] == "-e" {
				k, v, _ := strings.Cut(a[i+1], "=")
				env[k] = v
			}
		}
		f.noServer = false
		f.addSessionLocked(name, flagValue(a, "-c"), "", env)
	case "kill-session":
		s, _ := f.resolve(target)
		if s == nil {
			return missing(target)
		}
		delete(f.sessions, s.name)
	case "kill-server":
		f.sessions = map[string]*fsess{}
		f.noServer = true
	case "list-sessions":
		format := flagValue(a, "-F")
		filter := flagValue(a, "-f")
		var lines []string
		for _, n := range f.order {
			s := f.sessions[n]
			if s == nil {
				continue
			}
			if filter != "" && filter != "#{==:#{session_name},"+n+"}" {
				continue
			}
			lines = append(lines, f.expand(format, s, s.panes[0]))
		}
		return ok(strings.Join(lines, "\n"))
	case "list-panes":
		format := flagValue(a, "-F")
		var ss []*fsess
		if c.has("-a") {
			for _, n := range f.order {
				if s := f.sessions[n]; s != nil {
					ss = append(ss, s)
				}
			}
		} else if s, _ := f.resolve(target); s != nil {
			ss = append(ss, s)
		} else {
			return missing(target)
		}
		var lines []string
		for _, s := range ss {
			for _, p := range s.panes {
				lines = append(lines, f.expand(format, s, p))
			}
		}
		return ok(strings.Join(lines, "\n"))
	case "display-message":
		s, p := f.resolve(target)
		if s == nil || p == nil {
			return missing(target)
		}
		return ok(f.expand(c.last(), s, p))
	case "respawn-pane":
		s, p := f.resolve(target)
		if p == nil {
			return missing(target)
		}
		delete(f.procs, p.pid)
		p.pid = f.spawnLocked("1", "")
		p.dead = false
		if len(a) > 0 && !strings.HasPrefix(c.last(), "-") && c.last() != target && c.last() != flagValue(a, "-c") {
			p.cmd = paneCommandFor(c.last())
		} else {
			p.cmd = "bash"
		}
		f.procs[p.pid].comm = p.cmd
		_ = s
	case "split-window":
		s, _ := f.resolve(target)
		if s == nil {
			return missing(target)
		}
		f.addPaneLocked(s, "", "")
	case "set-environment":
		if c.has("-g") {
			if c.has("-u") {
				delete(f.global, c.last())
			}
			return ok("")
		}
		s, _ := f.resolve(target)
		if s == nil {
			return missing(target)
		}
		if c.has("-u") {
			delete(s.env, c.last())
			return ok("")
		}
		s.env[a[len(a)-2]] = c.last()
	case "show-environment":
		s, _ := f.resolve(target)
		if s == nil {
			return missing(target)
		}
		key := c.last()
		v, found := s.env[key]
		if !found {
			return fail("unknown variable: " + key)
		}
		return ok(key + "=" + v)
	case "capture-pane":
		_, p := f.resolve(target)
		if p == nil {
			return fail("can't find pane: " + target)
		}
		if p.composer != nil {
			return ok(p.composer.render())
		}
		return ok(p.content)
	case "send-keys":
		_, p := f.resolve(target)
		if p == nil {
			return fail("can't find pane: " + target)
		}
		rest := a[3:]
		p.keys = append(p.keys, strings.Join(rest, " "))
		if c := p.composer; c != nil {
			if len(rest) >= 2 && rest[0] == "-l" {
				c.input += rest[len(rest)-1]
			} else {
				for _, k := range rest {
					switch k {
					case "Enter":
						if c.input != "" {
							c.transcript = append(c.transcript, "❯ "+c.input, "⏺ ok")
							c.submitted = append(c.submitted, c.input)
							c.input = ""
						}
					case "C-u":
						c.input = ""
					}
				}
			}
		}
	case "new-window":
		s, _ := f.resolve(target)
		if s == nil {
			return missing(target)
		}
		f.addWindowLocked(s, "")
	case "set-option":
		if c.has("-w", "-t") && c.has("window-size") {
			if _, p := f.resolve(target); p != nil {
				p.windowSize = c.last()
			}
		}
	case "show-options":
		if _, p := f.resolve(target); p != nil && c.has("window-size") {
			return ok("window-size " + p.windowSize)
		}
	}
	return ok("")
}

func (f *fakeServer) ps(args []string) reply {
	joined := strings.Join(args, " ")
	pids := make([]string, 0, len(f.procs))
	for pid := range f.procs {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	switch {
	case joined == "-axo pid=,ppid=,comm=":
		var b strings.Builder
		for _, pid := range pids {
			p := f.procs[pid]
			fmt.Fprintf(&b, "%5s %5s %s\n", p.pid, p.ppid, p.comm)
		}
		return ok(b.String())
	case joined == "-axo pid,pgid":
		var b strings.Builder
		b.WriteString("  PID  PGID\n")
		for _, pid := range pids {
			fmt.Fprintf(&b, "%5s %5s\n", pid, f.procs[pid].pgid)
		}
		return ok(b.String())
	}
	pid := flagValue(args, "-p")
	p := f.procs[pid]
	if p == nil {
		return reply{err: exitError(1)}
	}
	switch flagValue(args, "-o") {
	case "comm=":
		return ok(p.comm + "\n")
	case "ppid=":
		return ok(p.ppid + "\n")
	case "pgid=":
		return ok(p.pgid + "\n")
	}
	return ok("")
}
