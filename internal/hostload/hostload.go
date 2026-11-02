// Package hostload attributes the host's CPU to the sessions and services
// that used it: one sample reads command lines, CPU over an interval and each
// process's working directory, then sums the CPU per owner (gt-nu89c).
//
// A process that starts and exits between two samples is invisible, because a
// sample reads one interval rather than tracing the host.
package hostload

import (
	"sort"
	"strings"
)

// Owner names are the rows a table prints. A crew session and a VM get a row
// each, so their names carry the session or the program: crew/dunlin,
// vm/qemu-system-aarch64.
const (
	OwnerLanding   = "landing"
	OwnerInstaller = "installer"
	OwnerDaemon    = "daemon"
	OwnerDolt      = "dolt"
	OwnerDashboard = "dashboard"
	OwnerOther     = "other"
	OwnerCrew      = "crew"    // prefix: crew/<name>
	OwnerPolecat   = "polecat" // prefix: polecat/<name>
	OwnerVM        = "vm"      // prefix: vm/<program>
)

// Row is one owner's share of a sample: its processes' CPU summed, in percent
// of one core.
type Row struct {
	Owner string  `json:"owner"`
	CPU   float64 `json:"cpu"`
	Procs int     `json:"procs"`
}

// Classify names the owner of the process with this working directory and
// command line. The working directory decides first, so a crew session's gt
// and bd children are charged to the session; the command line decides only
// when the directory names no owner. A process neither names is OwnerOther.
func Classify(cwd, command string) string {
	segments := pathSegments(cwd)
	for _, r := range pathRules {
		if owner, ok := r.match(segments); ok {
			return owner
		}
	}
	program, args := programOf(command)
	for _, r := range cmdRules {
		if owner, ok := r.match(program, args); ok {
			return owner
		}
	}
	return OwnerOther
}

// pathRules name an owner by the working directory a process runs in. They are
// tried before the command rules.
var pathRules = []pathRule{
	// <town>/<rig>/crew/<name>: the worktree a bird or crew session runs in.
	{owner: OwnerCrew, segment: "crew", each: true},
	// <town>/<rig>/polecats/<name>: the worktree a polecat session runs in.
	{owner: OwnerPolecat, segment: "polecats", each: true},
	// The landing gates' throwaway trees, gt-landing-<uid> and
	// gt-landing.noindex.
	{owner: OwnerLanding, segment: "gt-landing", prefix: true},
	// The installer's source tree, <rig>/mayor/rig.
	{owner: OwnerInstaller, pair: [2]string{"mayor", "rig"}},
}

// pathRule is one row of pathRules. A rule names a segment, a segment prefix,
// or two consecutive segments; the first row that matches wins.
type pathRule struct {
	owner string
	// segment is matched against each path segment of the working directory,
	// in full or, with prefix, from its start.
	segment string
	prefix  bool
	// each credits the segment after segment as well, so a crew worktree is
	// crew/<name>.
	each bool
	// pair is two consecutive path segments, for a tree named by both.
	pair [2]string
}

// match reports the owner this rule credits for the path, if it names one.
func (r pathRule) match(segments []string) (string, bool) {
	if r.pair[0] != "" {
		for i := 0; i+1 < len(segments); i++ {
			if segments[i] == r.pair[0] && segments[i+1] == r.pair[1] {
				return r.owner, true
			}
		}
		return "", false
	}
	for i, s := range segments {
		hit := s == r.segment
		if r.prefix {
			hit = strings.HasPrefix(s, r.segment)
		}
		if !hit {
			continue
		}
		if r.each {
			// A crew directory with nothing under it names no session, so
			// the path falls through to the command rules.
			if i+1 < len(segments) {
				return r.owner + "/" + segments[i+1], true
			}
			continue
		}
		return r.owner, true
	}
	return "", false
}

// cmdRules name an owner by the process's command line, for a process whose
// working directory names none.
var cmdRules = []cmdRule{
	// The installer run from any directory.
	{owner: OwnerInstaller, program: "gt-installer"},
	// The Dolt SQL server, whichever town or test database it serves.
	{owner: OwnerDolt, program: "dolt"},
	// The daemon that ticks and the dashboard that serves the UI.
	{owner: OwnerDaemon, program: "gt", args: []string{"daemon"}},
	{owner: OwnerDashboard, program: "gt", args: []string{"dashboard"}},
	// The processes that run a VM's guest: Docker Desktop's virtualization
	// helper, a qemu guest, a lima or colima VM.
	{owner: OwnerVM, each: true, program: "com.docker.virtualization"},
	{owner: OwnerVM, each: true, program: "qemu-system", prefix: true},
	{owner: OwnerVM, each: true, program: "limactl"},
	{owner: OwnerVM, each: true, program: "colima"},
}

// cmdRule is one row of cmdRules: a program's base name, optionally matched by
// prefix, and the leading arguments that must follow it.
type cmdRule struct {
	owner   string
	each    bool
	program string
	prefix  bool
	args    []string
}

// match reports the owner this rule credits for the command line, if any.
func (r cmdRule) match(program string, args []string) (string, bool) {
	hit := program == r.program
	if r.prefix {
		hit = strings.HasPrefix(program, r.program)
	}
	if !hit {
		return "", false
	}
	for i, a := range r.args {
		if i >= len(args) || args[i] != a {
			return "", false
		}
	}
	if r.each {
		return r.owner + "/" + program, true
	}
	return r.owner, true
}

// Attribute sums the CPU of the processes in cpu by owner, reading commands
// from cmds and working directories from cwds. A pid with no working directory
// is classified by its command. A process that used no CPU adds no row, since
// the table answers who is using the CPU.
func Attribute(cmds map[int]string, cpu map[int]float64, cwds map[int]string) []Row {
	sums := make(map[string]*Row)
	for pid, pct := range cpu {
		if pct <= 0 {
			continue
		}
		owner := Classify(cwds[pid], cmds[pid])
		row, ok := sums[owner]
		if !ok {
			row = &Row{Owner: owner}
			sums[owner] = row
		}
		row.CPU += pct
		row.Procs++
	}
	rows := make([]Row, 0, len(sums))
	for _, r := range sums {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CPU != rows[j].CPU {
			return rows[i].CPU > rows[j].CPU
		}
		return rows[i].Owner < rows[j].Owner
	})
	return rows
}

// Table reads one sample's three readings and returns CPU by owner: command
// lines from ps, CPU from top's interval sample, working directories from
// lsof.
func Table(psOut, topOut, lsofOut string) ([]Row, error) {
	cmds, err := ParsePS(psOut)
	if err != nil {
		return nil, err
	}
	cpu, err := ParseTop(topOut)
	if err != nil {
		return nil, err
	}
	cwds, err := ParseLsof(lsofOut)
	if err != nil {
		return nil, err
	}
	return Attribute(cmds, cpu, cwds), nil
}

// pathSegments splits a path into its non-empty segments.
func pathSegments(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
}

// programOf splits a command line into its program's base name and the
// arguments after it.
func programOf(command string) (string, []string) {
	f := strings.Fields(command)
	if len(f) == 0 {
		return "", nil
	}
	name := f[0]
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name, f[1:]
}
