package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/rig"
)

// The om panel reads two things the town already writes: each rig's landings
// file (one record per landed bead, with om's verdict and score) and the
// daemon log (om's wall time per review, and every rejection with its kind).
// It runs no om and no bd. What om is running right now it reads from the
// processes on the host, through the same panel's one ps read. The bar and
// depth it reports are the rig's .om.json the landing worker reviews under,
// read out of the rig's repository, not the user's om config (gt-ndbwf).

const (
	omRecentRows = 15
	// omLastRows is how many of the newest reviews om actually ran the panel
	// lists at its top. The bead's redesign asks for five there.
	omLastRows = 5
	// omProcTimeout bounds the process list a read lists: a ps that hangs on a
	// wedged host must not hold the panel's read open.
	omProcTimeout = 2 * time.Second
	// omProcOutput caps how much of ps's listing is kept. A process with a
	// runaway argv is not worth reading past the first processes.
	omProcOutput = 512 * 1024
	// The by-day table is a fixed window: today and the three calendar days
	// before it. A day with no reviews still gets a row.
	omDayRows      = 4
	omStageJoinMin = 45 * time.Minute // a stages line this far before a landing belongs to it
	// daemonStartMarker is what the daemon logs when it starts. A landing is
	// one worker's pass, never resumed, so a start after a merge means that
	// landing died with the process: the reader drops its ghost row with it.
	daemonStartMarker = "Daemon starting (PID "
	// omPanelRig is the rig whose .om.json the panel reports: the panel is the
	// gastown dashboard, and the panel has no per-rig place to list the bars of
	// the town's other rigs (gt-ndbwf).
	omPanelRig = "gastown"
	// omRigConfigTTL is how long a read of the rig's .om.json is reused. The
	// file changes only when a landing changes it, and each read starts git.
	omRigConfigTTL = 30 * time.Second
	// omUserConfigSource names the user's om config as the origin of a value.
	omUserConfigSource = "~/.config/om/config.json"
)

var (
	omStageRe  = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): stages: (.*)$`)
	omRejectRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): rejected \((\w+)\): (.*)$`)
	omScoreRe  = regexp.MustCompile(`\bscore ([0-9.]+)`)
	// omMergeRe and omLandedRe bracket a landing: the merge line is logged when
	// the worker starts gating the merged tree, the landed line when it is on
	// the target. A merge with no landed or rejected line after it is in flight.
	//
	// The merge line carries the landing's three commits — the branch head it
	// merged, the target's tip it merged onto, and the merge candidate both
	// produced ("merged <head> onto <remote>/<target> (<tip>) as <cand>") — and
	// the candidate is the commit om is handed as --head
	// (internal/land/review.go builds "om review -C <dir> --base <base> --head
	// <head>"), which is what joins a running review back to its landing
	// (gt-7qz2h). They are read as an optional tail: a merge line that does not
	// name them still begins a merge, so a wording change costs the join rather
	// than every running row.
	omMergeRe  = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): merged (?:([0-9a-f]+) onto \S+ \(([0-9a-f]+)\) as ([0-9a-f]+))?`)
	omLandedRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): (?:landed|already landed as) `)
	// omInfraFailedRe ends a merge too. A pass that stops on an infrastructure
	// failure (gate, review, push, merge pull request) logs no [land] line:
	// the worker's infraFailure logs "<rig>: <bead>: landing failed (N in a
	// row, next try in D): ..." instead, and nothing else would close the
	// merge the pass began (gt-7lcem).
	omInfraFailedRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \S+: (\S+): landing failed \(`)
)

// infraFailedMarker is what the worker's line for a failed landing pass carries
// after the bead; scanLog keeps a line that has it.
const infraFailedMarker = ": landing failed ("

const omLogTimeLayout = "2006/01/02 15:04:05"

// omStage is one gated merge's stage timings, off a "stages:" log line. A
// stage the line does not name is nil; om is nil when the review did not run.
type omStage struct {
	At   time.Time
	Bead string
	Lint *time.Duration
	Gate *time.Duration
	OM   *time.Duration
}

// omRejection is one rejected landing, off a "rejected (kind):" log line.
type omRejection struct {
	At     time.Time
	Bead   string
	Kind   string
	Detail string
	Score  *float64
}

// omMerge is one landing in flight, off a "merged" log line: the moment the
// worker began gating a bead's merged tree, and the three commits the line
// names — the branch head, the target's tip it merged onto, and the merge
// candidate om reviews. Done is set by a later landed or rejected line for the
// bead, or by a daemon start, after which the merge is no longer live.
type omMerge struct {
	At   time.Time
	Bead string
	Head string // the branch head merged
	Tip  string // the target tip it was merged onto
	Cand string // the merge candidate, the commit om is handed as --head
	Done bool
}

// omRecord is a landings record plus the field the landing worker writes for
// a landing that touched risk paths.
type omRecord struct {
	landings.Record
	RiskPaths []string `json:"risk_paths"`
}

// omConfig is the reviewer's own config: what backend, how deep, how strict.
// Backend is the argv om runs the review with, whose model — not whose
// command — is what the panel and the header strip name.
type omConfig struct {
	Backend   []string `json:"backend"`
	Threshold *float64 `json:"threshold"`
	Depth     string   `json:"depth"`
	Timeout   int      `json:"timeout"`
}

// omModelFamilies are the model names a reviewer's backend is read as, in the
// order a longer name is given up for one of them: a name carrying "sonnet",
// "flash" or "pro" is that family, whatever release prefix its vendor puts on
// it. The short name is what an operator reads off the header, and a release
// number is not what they are looking for when they ask what the town reviews
// with.
var omModelFamilies = []string{"sonnet", "opus", "haiku", "flash", "pro"}

// omModelName names the model a reviewer's backend runs, in the short form an
// operator uses for it. The backend is an argv — ["/<home>/.local/bin/claude",
// "-p", "--model", "sonnet"] is the reviewer's own config — so the model is
// what its --model flag names, and a backend run under a wrapper that names
// none is read from the wrapper's own name ("claude-deepseek-flash" is flash).
// A backend that is neither shows under its command name, which is all the
// town knows about it. What no backend shows is its raw command line.
func omModelName(backend []string) string {
	if len(backend) == 0 {
		return ""
	}
	name := backendModel(backend)
	if name == "" {
		name = filepath.Base(backend[0])
	}
	for _, family := range omModelFamilies {
		if modelWord(name, family) {
			return family
		}
	}
	return name
}

// backendModel returns the model a backend's argv names: the value of --model
// or -m, in either the separate or the --model= spelling. A flag with no value
// after it names nothing.
func backendModel(argv []string) string {
	for i, arg := range argv {
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 < len(argv) {
				return argv[i+1]
			}
		case strings.HasPrefix(arg, "--model="):
			return strings.TrimPrefix(arg, "--model=")
		}
	}
	return ""
}

// modelWord reports whether word is one of name's own words, for a name split
// on anything that is not a letter or a digit: "flash" matches deepseek-flash
// and claude-flash-2026, and never flashback.
func modelWord(name, word string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if w == word {
			return true
		}
	}
	return false
}

// omProc is one process running on the host, off the ps line that listed it:
// its pid, how long it has been running, and its argv.
type omProc struct {
	PID     int
	Elapsed time.Duration
	Args    []string
}

// omProcLister lists the processes running on the host. It is injected rather
// than reached for, so a test lists the reviews it wants and a host that
// cannot list processes costs the panel its running rows and nothing else.
type omProcLister func(ctx context.Context) ([]omProc, error)

// listOMProcs is the process lister the dashboard reads: every process on the
// host as pid, elapsed time and argv, under a timeout and an output cap. A
// platform with no ps answers with no processes rather than an error — a host
// that cannot list its processes has no review running to show, which is what
// the panel draws — while any other failure is the reader's to drop.
func listOMProcs(ctx context.Context) ([]omProc, error) {
	ctx, cancel := context.WithTimeout(ctx, omProcTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,etime=,args=")
	out := &capWriter{n: omProcOutput}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return parseOMProcs(out.buf.Bytes()), nil
}

// capWriter keeps the first n bytes written to it and drains the rest, so a
// command whose output runs away cannot grow the read. Write always reports
// every byte consumed: what is past the cap is dropped, not refused, or the
// copy feeding the writer would fail the command with it.
type capWriter struct {
	buf bytes.Buffer
	n   int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.n - w.buf.Len(); room > 0 {
		keep := p
		if len(keep) > room {
			keep = keep[:room]
		}
		w.buf.Write(keep)
	}
	return len(p), nil
}

// parseOMProcs reads ps's listing: one "pid etime argv..." line per process. A
// line that does not open with a pid and an elapsed time is dropped, and with
// it, whatever it said.
func parseOMProcs(out []byte) []omProc {
	var procs []omProc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		elapsed, err := parsePsEtime(f[1])
		if err != nil {
			continue
		}
		procs = append(procs, omProc{PID: pid, Elapsed: elapsed, Args: f[2:]})
	}
	return procs
}

// parsePsEtime reads the elapsed time ps prints for a process, "[[dd-]hh:]mm:ss".
// Any other token is an error, and its line goes with it.
func parsePsEtime(tok string) (time.Duration, error) {
	rest, days := tok, 0
	if d, tail, ok := strings.Cut(tok, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("elapsed time %q", tok)
		}
		days, rest = n, tail
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("elapsed time %q", tok)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("elapsed time %q", tok)
		}
		nums[i] = n
	}
	h := 0
	if len(nums) == 3 {
		h = nums[0]
	}
	return time.Duration(days)*24*time.Hour + time.Duration(h)*time.Hour +
		time.Duration(nums[len(nums)-2])*time.Minute + time.Duration(nums[len(nums)-1])*time.Second, nil
}

// omReviewArgv is a running process read as an om review: the worktree it was
// pointed at, the rig that worktree names, and the commits it was handed.
type omReviewArgv struct {
	Dir  string
	Rig  string
	Base string
	Head string
}

// omReviewArgvOf reads a process's argv as an om review's, for the argv
// internal/land/review.go builds: "om review -C <dir> --base <base> --head
// <head> --out <file>". A process that is not one — no "review" argument, no
// -C with a directory after it, or a -C outside the landing worker's work
// roots — is not a review and comes back false. A review that has not reached
// its --head yet is one, with no head: it is running, not malformed.
func omReviewArgvOf(args []string) (omReviewArgv, bool) {
	var a omReviewArgv
	review := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "review":
			review = true
		case "-C":
			if i+1 < len(args) {
				a.Dir, i = args[i+1], i+1
			}
		case "--base":
			if i+1 < len(args) {
				a.Base, i = args[i+1], i+1
			}
		case "--head":
			if i+1 < len(args) {
				a.Head, i = args[i+1], i+1
			}
		}
	}
	rig, ok := omLandingRig(a.Dir)
	if !review || !ok {
		return omReviewArgv{}, false
	}
	a.Rig = rig
	return a, true
}

// omLandingRig is the rig a landing worktree belongs to. The landing worker
// roots a rig's throwaway worktrees at <work_root>/<rig> and makes each one a
// "land-*" directory under it, and the default work_root is
// $TMPDIR/gt-landing-<uid> (internal/daemon/landing_worker.go), so the rig is
// the path element after the gt-landing- one. Neither the uid nor the directory
// is hard-coded: the root is matched by the gt-landing- prefix the worker
// builds it from, whatever TMPDIR that worker ran with.
func omLandingRig(dir string) (string, bool) {
	parts := strings.Split(dir, "/")
	for i, part := range parts {
		if !strings.HasPrefix(part, "gt-landing-") || i+1 == len(parts) {
			continue
		}
		if rig := parts[i+1]; rig != "" {
			return rig, true
		}
	}
	return "", false
}

// omMergeCand is the bead a running review is gated for: the merge line whose
// candidate commit the process was handed as --head. The log writes the
// candidate eight characters short of om's argv, so either may be the prefix
// of the other. No match is no bead: the daemon has not logged this merge yet,
// and the review is still running.
func omMergeCand(cands map[string]string, head string) string {
	if head == "" {
		return ""
	}
	if bead, ok := cands[head]; ok {
		return bead
	}
	for cand, bead := range cands {
		if strings.HasPrefix(head, cand) || strings.HasPrefix(cand, head) {
			return bead
		}
	}
	return ""
}

// omReader keeps the daemon-log scan between polls, so a poll reads only what
// the log gained, and re-reads the small landings files whole.
type omReader struct {
	landings *dashLandings
	logPath  string
	cfgPath  string
	// rigOM reads the rig's .om.json as the landing worker sees it: its bytes,
	// and whether the rig has one. Nil reads as a rig with none.
	rigOM func() (data []byte, found bool, err error)
	// procs lists the host's processes, and polecatFor names who holds a bead,
	// read from the bead store by the caller: the om panel's running rows and
	// the Landings pane's live rows ask that one question of the same reader.
	procs      omProcLister
	polecatFor func(rig, bead string) string

	rigMu    sync.Mutex // guards the cached read of the rig's .om.json
	rigCfg   omRigRead
	rigAt    time.Time
	mu       sync.Mutex
	offset   int64
	stages   []omStage
	rejects  []omRejection
	merges   map[string]omMerge  // bead -> its latest merge, live until it ends
	lastTick *dashboard.Dispatch // the spec dispatcher's latest tick line
}

func newOMReader(townRoot string, landings *dashLandings, polecatFor func(rig, bead string) string) *omReader {
	return &omReader{
		landings: landings, logPath: filepath.Join(townRoot, "daemon", "daemon.log"), cfgPath: omConfigPath(),
		rigOM: func() ([]byte, bool, error) { return readRigOMJSON(townRoot, omPanelRig) },
		procs: listOMProcs, polecatFor: polecatFor,
	}
}

// readRigOMJSON reads .om.json from the tip of the rig's landing target in its
// bare repository: the file land.OMThreshold and the worker's om run read, off
// the base the worker merges onto. found is false for a target with no
// .om.json; any other failure is an error, never "none".
func readRigOMJSON(townRoot, rigName string) (data []byte, found bool, err error) {
	return readRigOMJSONIn(townRoot, rigName, func(repoPath string) omRigRepo { return git.NewGit(repoPath) })
}

// omRigRepo is the rig's bare repository as readRigOMJSONIn uses it: the
// remotes the landing target resolves against, and the file read at its tip.
type omRigRepo interface {
	omFileReader
	rig.LandingRemotes
}

// readRigOMJSONIn is readRigOMJSON over open, which returns the repository at
// a path: the rule that picks the landing remote and branch, apart from the
// git it reads them through.
func readRigOMJSONIn(townRoot, rigName string, open func(repoPath string) omRigRepo) (data []byte, found bool, err error) {
	rigPath := filepath.Join(townRoot, rigName)
	repo := open(filepath.Join(rigPath, ".repo.git"))
	remote, err := rig.ResolveLandingRemoteIn(repo, townRoot, rigName)
	if err != nil {
		return nil, false, err
	}
	branch := "main"
	cfg, err := rig.LoadRigConfigIfPresent(rigPath)
	if err != nil {
		return nil, false, err
	}
	if cfg != nil && cfg.DefaultBranch != "" {
		branch = cfg.DefaultBranch
	}
	return omFileAt(repo, remote+"/"+branch)
}

// omFileReader is the part of *git.Git omFileAt reads a file through.
type omFileReader interface {
	ShowFileAtRev(ref, path string) (string, error)
}

// omFileAt reads .om.json at ref. A tree without the file is none; a ref or
// repository git cannot read is an error, so a broken rig never reads as one
// with no .om.json.
func omFileAt(g omFileReader, ref string) (data []byte, found bool, err error) {
	out, err := g.ShowFileAtRev(ref, ".om.json")
	if errors.Is(err, git.ErrNotAtRef) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(out), true, nil
}

// omRigRead is one read of the rig's .om.json.
type omRigRead struct {
	data  []byte
	found bool
	err   error
}

// rigRead returns the rig's .om.json, reusing a read younger than
// omRigConfigTTL. It starts git, so it runs outside r.mu.
func (r *omReader) rigRead(now time.Time) omRigRead {
	if r.rigOM == nil {
		return omRigRead{}
	}
	r.rigMu.Lock()
	defer r.rigMu.Unlock()
	if age := now.Sub(r.rigAt); !r.rigAt.IsZero() && age >= 0 && age < omRigConfigTTL {
		return r.rigCfg
	}
	data, found, err := r.rigOM()
	r.rigCfg, r.rigAt = omRigRead{data: data, found: found, err: err}, now
	return r.rigCfg
}

// resolveOMConfig is the config the landing worker's reviews run under: the
// rig's .om.json over the user's config, which supplies what the rig file
// leaves out. The backend is always the user's: om ignores a backend key in
// the reviewed tree's .om.json (docs/design/forgejo-primary-landing.md). The
// second result says where the
// threshold comes from, for the panel's caption. A rig file that is there but
// cannot be read or parsed fails every review, so none of its numbers is
// guessed from the user config: threshold, depth and timeout are left empty and
// the source says why.
func resolveOMConfig(user omConfig, rigName string, rr omRigRead) (omConfig, string) {
	rigFile := rigName + " .om.json"
	if !rr.found && rr.err == nil {
		return user, omUserConfigSource + ", " + rigName + " has no .om.json"
	}
	var rc omConfig
	err := rr.err
	if err == nil {
		err = json.Unmarshal(rr.data, &rc)
	}
	if err != nil {
		return omConfig{Backend: user.Backend}, rigFile + " unreadable: " + err.Error()
	}
	out := user
	source := rigFile
	if rc.Threshold != nil {
		out.Threshold = rc.Threshold
	} else {
		source = omUserConfigSource + ", " + rigFile + " sets no threshold"
	}
	if rc.Depth != "" {
		out.Depth = rc.Depth
	}
	if rc.Timeout > 0 {
		out.Timeout = rc.Timeout
	}
	return out, source
}

// omConfigPath is the reviewer's own config, in the home of whoever runs the
// dashboard: what backend it reviews with, how deep, how strict. Empty when
// there is no home to look in, which every reader of it reads as "no config".
func omConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "om", "config.json")
}

// read is the panel's whole reading. A reader that fails leaves its part out;
// nothing is ever shown as zero for want of a read.
//
// The two slow things a read does — listing the host's processes, and reading
// a bead store for each running row's holder — are done outside r.mu, which
// the trend and dispatch reads share: neither is worth stalling another
// panel's poll for.
func (r *omReader) read(now time.Time) *dashboard.OM {
	procs := r.listProcs()
	cfg, thresholdSource := resolveOMConfig(loadOMConfig(r.cfgPath), omPanelRig, r.rigRead(now))
	om, cands := r.readPanelLocked(now, cfg)
	if om == nil {
		return nil
	}
	om.ThresholdSource = thresholdSource
	om.Running = r.runningRows(cands, procs, now)
	return om
}

// readPanelLocked is the panel's numbers off the log and the landings files,
// and the merges in flight as the candidate-commit join a running review is
// found under. r.mu is held; nil, nil is a read the log gave nothing to.
func (r *omReader) readPanelLocked(now time.Time, cfg omConfig) (*dashboard.OM, map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.scanLog(); err != nil && len(r.stages) == 0 && len(r.rejects) == 0 {
		return nil, nil
	}
	om := buildOM(now, r.landings.get(), r.stages, r.rejects, cfg)
	cands := make(map[string]string, len(r.merges))
	for _, m := range r.merges {
		if m.Cand != "" {
			cands[m.Cand] = m.Bead
		}
	}
	return om, cands
}

// listProcs lists the host's processes once, off the lock: ps is the slowest
// thing a read does, and a lister that fails leaves the panel its rows.
func (r *omReader) listProcs() []omProc {
	if r.procs == nil {
		return nil
	}
	procs, err := r.procs(context.Background())
	if err != nil {
		return nil
	}
	return procs
}

// runningRows is the om reviews running right now, off the processes on the
// host: each review's argv gives the worktree it reads and the commits it was
// handed, and the worktree's rig and the caller's bead read name the rest. A
// review is joined to its bead through the merge line whose candidate is the
// head the process was handed, and one no merge line names yet is still
// listed, with no bead. Newest first.
func (r *omReader) runningRows(cands map[string]string, procs []omProc, now time.Time) []dashboard.OMRunning {
	var rows []dashboard.OMRunning
	for _, p := range procs {
		a, ok := omReviewArgvOf(p.Args)
		if !ok {
			continue
		}
		row := dashboard.OMRunning{Rig: a.Rig, Started: now.Add(-p.Elapsed),
			Base: shortSHA(a.Base), Head: shortSHA(a.Head), PID: p.PID}
		row.Bead = omMergeCand(cands, a.Head)
		if row.Bead != "" && r.polecatFor != nil {
			row.Polecat = r.polecatFor(row.Rig, row.Bead)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Started.After(rows[j].Started) })
	return rows
}

// readOMRecords reads one rig's om record file line by line. A line that is
// not a record is skipped. A reader, not a Scanner: a line longer than the
// scanner's buffer would make the scan stop, silently hiding every record
// after it (gt-2czgm). A read that fails part-way returns what it read.
func readOMRecords(path string) []omRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []omRecord
	br := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			var rec omRecord
			if json.Unmarshal([]byte(strings.TrimRight(line, "\r\n")), &rec) == nil && rec.Bead != "" {
				out = append(out, rec)
			}
		}
		if err != nil {
			return out
		}
	}
}

func loadOMConfig(path string) omConfig {
	var c omConfig
	if path == "" {
		return c
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

// scanLog reads what the daemon log gained since the last scan. A log that
// shrank was rotated: it is read from the top, and what was kept stays kept.
func (r *omReader) scanLog() error {
	f, err := os.Open(r.logPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < r.offset {
		r.offset = 0
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// A last line with no newline yet is not consumed: the writer is
			// mid-line, and the next scan reads it whole.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		r.offset += int64(len(line))
		if !strings.Contains(line, "[land]") && !strings.Contains(line, infraFailedMarker) && !strings.Contains(line, dispatchTickMarker) && !strings.Contains(line, daemonStartMarker) {
			continue
		}
		r.parseLogLine(strings.TrimRight(line, "\r\n"))
	}
}

func (r *omReader) parseLogLine(line string) {
	if strings.Contains(line, dispatchTickMarker) {
		if d := parseDispatchTick(line); d != nil {
			r.lastTick = d
		}
		return
	}
	if strings.Contains(line, daemonStartMarker) {
		r.endAllMerges()
		return
	}
	if m := omMergeRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			r.beginMerge(omMerge{At: at, Bead: m[2], Head: m[3], Tip: m[4], Cand: m[5]})
		}
		return
	}
	if m := omLandedRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			r.endMerge(m[2], at)
		}
		return
	}
	if m := omInfraFailedRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			r.endMerge(m[2], at)
		}
		return
	}
	if m := omStageRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			lint, gate, om := omStageTimes(m[3])
			r.stages = append(r.stages, omStage{At: at, Bead: m[2], Lint: lint, Gate: gate, OM: om})
		}
		return
	}
	if m := omRejectRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			rj := omRejection{At: at, Bead: m[2], Kind: m[3], Detail: m[4]}
			if sm := omScoreRe.FindStringSubmatch(m[4]); sm != nil {
				if v, err := strconv.ParseFloat(sm[1], 64); err == nil {
					rj.Score = &v
				}
			}
			r.rejects = append(r.rejects, rj)
			r.endMerge(m[2], at)
		}
	}
}

// omLogAt parses the timestamp a daemon.log line opens with. The log is
// stamped in local time with no zone.
func omLogAt(stamp string) (time.Time, error) {
	return time.ParseInLocation(omLogTimeLayout, stamp, time.Local)
}

// beginMerge records that a bead's tree has been merged and its landing is in
// flight. A landed bead with no later record is re-merged for the repair, and
// a rejected landing can be requeued and merged again: the newest merge is the
// live one, so it replaces whatever the bead had.
func (r *omReader) beginMerge(m omMerge) {
	if r.merges == nil {
		r.merges = map[string]omMerge{}
	}
	// A merge line that names no commits must not erase the ones the bead's
	// landing began with: a rig whose landing merges through a pull request
	// logs a second line for it ("merged <cand> on main through pull request
	// #N", the daemon log of 2026-10-12), and the commits — the candidate a
	// running review is joined by — came from the first.
	if old, ok := r.merges[m.Bead]; ok && m.Cand == "" && old.Cand != "" {
		m.Head, m.Tip, m.Cand = old.Head, old.Tip, old.Cand
	}
	r.merges[m.Bead] = m
}

// endMerge marks a bead's merge no longer in flight. A line at or before the
// merge it would close is ignored: it belongs to an earlier attempt of the
// same bead, and the merge being read is the newer one.
func (r *omReader) endMerge(bead string, at time.Time) {
	m, ok := r.merges[bead]
	if !ok || m.At.After(at) {
		return
	}
	m.Done = true
	r.merges[bead] = m
}

// endAllMerges closes every merge in flight. The worker lands one pass and
// exits; nothing resumes a pass, so a merge a restart followed never finished,
// and one line ends every landing that was live when the daemon came back.
func (r *omReader) endAllMerges() {
	for bead, m := range r.merges {
		m.Done = true
		r.merges[bead] = m
	}
}

// running returns the landings in flight at this read: every merge at or after
// since with no landed or rejected line after it and no daemon start after it,
// newest merge first. Merges the window has passed are dropped, so the map
// stays the size of the window rather than the log.
func (r *omReader) running(since time.Time) []omMerge {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.scanLog()
	var out []omMerge
	for bead, m := range r.merges {
		if m.At.Before(since) {
			delete(r.merges, bead)
			continue
		}
		if !m.Done {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// omStageDuration reads om's time off "lint 14s, gate 34s, om 40s"; nil when
// the review did not run (no om stage).
func omStageDuration(stages string) *time.Duration {
	_, _, om := omStageTimes(stages)
	return om
}

// omStageTimes reads each stage's duration off "lint 14s, gate 34s, om 40s".
// A stage the line does not name comes back nil, as does one whose token is
// not a duration; a trailing "(timed out)" after the token is ignored.
//
// The gate time is the ci stage on a cut-over rig: the candidate gate runs in
// Forgejo CI (StageCI in internal/land), so its line is "ci 2m46s, om 21s". A
// line that names both prefers the explicit gate.
func omStageTimes(stages string) (lint, gate, om *time.Duration) {
	for _, part := range strings.Split(stages, ", ") {
		name, rest, ok := strings.Cut(part, " ")
		if !ok {
			continue
		}
		tok, _, _ := strings.Cut(rest, " ")
		d, err := time.ParseDuration(tok)
		if err != nil {
			continue
		}
		switch name {
		case "lint":
			lint = &d
		case "gate":
			gate = &d
		case "ci":
			if gate == nil {
				gate = &d
			}
		case "om":
			om = &d
		}
	}
	return lint, gate, om
}

// omOutcome classifies a landing record's om verdict.
func omOutcome(verdict string) string {
	switch {
	case strings.HasPrefix(verdict, "approve"):
		return "approved"
	case strings.HasPrefix(verdict, "error"):
		return "error"
	}
	// "skipped", and the operator override ("overseer:<sha>"), landed without
	// a review of their own.
	return "skipped"
}

// buildOM folds the records, stages and rejections into the panel's numbers.
func buildOM(now time.Time, recs []omRecord, stages []omStage, rejs []omRejection, cfg omConfig) *dashboard.OM {
	om := &dashboard.OM{Rejects: map[string]int{}, Routes: map[string]int{}}
	om.Model = omModelName(cfg.Backend)
	om.Depth, om.TimeoutS, om.Threshold = cfg.Depth, cfg.Timeout, cfg.Threshold

	type win struct {
		label string
		since time.Time
	}
	wins := []win{
		{"1h", now.Add(-1 * time.Hour)},
		{"6h", now.Add(-6 * time.Hour)},
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"all", time.Time{}},
	}
	out := make([]dashboard.OMWindow, len(wins))
	scoreSum := make([]float64, len(wins))
	scoreN := make([]int, len(wins))
	secs := make([][]float64, len(wins))
	for i, w := range wins {
		out[i].Label = w.label
	}
	inWin := func(i int, at time.Time) bool { return wins[i].since.IsZero() || !at.Before(wins[i].since) }

	dayKeys := make([]string, omDayRows)
	dayAllowed := make(map[string]bool, omDayRows)
	today := now.In(time.Local)
	for i := range dayKeys {
		k := today.AddDate(0, 0, -i).Format("2006-01-02")
		dayKeys[i], dayAllowed[k] = k, true
	}

	days := map[string]*dashboard.OMDay{}
	daySecs := map[string][]float64{}
	// day returns the row an event's timestamp belongs to, or nil when its day
	// is older than the three days before today.
	day := func(at time.Time) *dashboard.OMDay {
		k := at.In(time.Local).Format("2006-01-02")
		if !dayAllowed[k] {
			return nil
		}
		if days[k] == nil {
			days[k] = &dashboard.OMDay{Day: k}
		}
		return days[k]
	}
	bucket := func(score float64) {
		i := int(score * 10)
		if i < 0 {
			i = 0
		}
		if i > 9 {
			i = 9
		}
		om.Scores[i]++
	}

	var first time.Time
	note := func(at time.Time) {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	// omSecs is om's own wall time for a landing, off the stages line the
	// review wrote; nil when no line for it names an om stage.
	omSecs := func(bead string, at time.Time) *float64 {
		if st := omStageFor(stages, bead, at); st != nil && st.OM != nil {
			s := st.OM.Seconds()
			return &s
		}
		return nil
	}
	// byBead is a bead's last landing record, for the facts a rejection's own
	// log line does not carry: the rig that holds it and its route.
	byBead := make(map[string]omRecord, len(recs))
	// omRan collects the reviews om actually ran, newest first once sorted:
	// what Last lists, as against Recent's longer tail of everything the review
	// loop decided.
	var omRan []dashboard.OMReview

	var recent []dashboard.OMReview
	for _, rec := range recs {
		byBead[rec.Bead] = rec
		note(rec.LandedAt)
		oc := omOutcome(rec.OMVerdict)
		om.Routes[rec.Route]++
		if len(rec.RiskPaths) > 0 {
			om.RiskPaths++
		}
		d := day(rec.LandedAt)
		for i := range wins {
			if !inWin(i, rec.LandedAt) {
				continue
			}
			out[i].Landed++
			switch oc {
			case "approved":
				out[i].Approved++
				if rec.OMScore > 0 {
					scoreSum[i] += rec.OMScore
					scoreN[i]++
				}
			case "error":
				out[i].Errors++
			default:
				out[i].Skipped++
			}
		}
		if d != nil {
			switch oc {
			case "approved":
				d.Approved++
			case "error":
				d.Errors++
			default:
				d.Skipped++
			}
		}
		if oc == "approved" && rec.OMScore > 0 {
			bucket(rec.OMScore)
		}
		rv := dashboard.OMReview{At: rec.LandedAt, Bead: rec.Bead, Rig: rec.Rig, Outcome: oc, Route: rec.Route,
			Gate: rec.GateResult, Risk: len(rec.RiskPaths) > 0}
		if rec.OMScore > 0 {
			s := rec.OMScore
			rv.Score = &s
		}
		if oc == "error" {
			rv.Detail = truncateRunes(rec.OMVerdict, 120)
		}
		rv.Secs = omSecs(rec.Bead, rec.LandedAt)
		recent = append(recent, rv)
		if oc != "skipped" {
			// om ran this landing: it approved it, or the review errored on
			// it. A skipped landing is one nothing reviewed (an operator
			// route), and belongs only in the tail.
			omRan = append(omRan, rv)
		}
	}

	for _, rj := range rejs {
		note(rj.At)
		om.Rejects[rj.Kind]++
		if d := day(rj.At); d != nil {
			d.Rejected++
		}
		for i := range wins {
			if inWin(i, rj.At) {
				out[i].Rejected++
				if rj.Kind == "review" && rj.Score != nil {
					scoreSum[i] += *rj.Score
					scoreN[i]++
				}
			}
		}
		if rj.Kind == "review" && rj.Score != nil {
			bucket(*rj.Score)
		}
		recent = append(recent, dashboard.OMReview{At: rj.At, Bead: rj.Bead, Outcome: "rejected", Kind: rj.Kind,
			Score: rj.Score, Detail: truncateRunes(rj.Detail, 140)})
		if rj.Kind == "review" {
			// om ran this landing too, and asked for changes: a review
			// rejection is a review that ran. A gate, conflict, policy or
			// empty rejection was decided before om ran, and belongs only in
			// the tail. The rig and route are the ones the bead's own landing
			// record names, and a rejection without a score is still a
			// review that ran.
			rv := dashboard.OMReview{At: rj.At, Bead: rj.Bead, Outcome: "rejected", Kind: rj.Kind,
				Score: rj.Score, Secs: omSecs(rj.Bead, rj.At), Detail: truncateRunes(rj.Detail, 140)}
			if rec, ok := byBead[rj.Bead]; ok {
				rv.Rig, rv.Route = rec.Rig, rec.Route
			}
			omRan = append(omRan, rv)
		}
	}

	for _, st := range stages {
		if st.OM == nil {
			continue
		}
		note(st.At)
		s := st.OM.Seconds()
		for i := range wins {
			if inWin(i, st.At) {
				secs[i] = append(secs[i], s)
			}
		}
		if d := day(st.At); d != nil {
			daySecs[d.Day] = append(daySecs[d.Day], s)
		}
	}

	for i := range out {
		if scoreN[i] > 0 {
			v := scoreSum[i] / float64(scoreN[i])
			out[i].AvgScore = &v
		}
		if len(secs[i]) > 0 {
			sort.Float64s(secs[i])
			m, p := percentile(secs[i], 0.5), percentile(secs[i], 0.95)
			out[i].MedianSecs, out[i].P95Secs = &m, &p
		}
	}
	om.Windows = out
	om.Since = first

	for _, k := range dayKeys {
		d := dashboard.OMDay{Day: k}
		if v := days[k]; v != nil {
			d = *v
		}
		if s := daySecs[k]; len(s) > 0 {
			var sum float64
			for _, v := range s {
				sum += v
			}
			avg := sum / float64(len(s))
			d.AvgSecs = &avg
		}
		om.Days = append(om.Days, d)
	}

	sort.SliceStable(recent, func(i, j int) bool { return recent[i].At.After(recent[j].At) })
	if len(recent) > omRecentRows {
		recent = recent[:omRecentRows]
	}
	om.Recent = recent

	sort.SliceStable(omRan, func(i, j int) bool { return omRan[i].At.After(omRan[j].At) })
	if len(omRan) > omLastRows {
		omRan = omRan[:omLastRows]
	}
	om.Last = omRan
	return om
}

// omStageFor is the stages line a landing belongs to: the latest one for the
// bead no more than a couple of minutes after the landing and within
// omStageJoinMin before it.
func omStageFor(stages []omStage, bead string, landedAt time.Time) *omStage {
	var best *omStage
	for i := range stages {
		st := &stages[i]
		if st.Bead != bead {
			continue
		}
		if st.At.After(landedAt.Add(2*time.Minute)) || st.At.Before(landedAt.Add(-omStageJoinMin)) {
			continue
		}
		if best == nil || st.At.After(best.At) {
			best = st
		}
	}
	return best
}

// percentile of an ascending slice, nearest rank.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// trendInputs returns the stage lines and rejections the review scan has
// collected at or after since, for the trend panel's window.
func (r *omReader) trendInputs(since time.Time) ([]omStage, []omRejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.scanLog()
	var rejs []omRejection
	for _, rj := range r.rejects {
		if !rj.At.Before(since) {
			rejs = append(rejs, rj)
		}
	}
	return r.stagesSinceLocked(since), rejs
}

// stagesSince returns the stage lines at or after since. The rework table's
// reader asks for the stages of its own window, to time each om verdict
// (gt-uiiml); the trend panel asks for the same lines through trendInputs.
func (r *omReader) stagesSince(since time.Time) []omStage {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.scanLog()
	return r.stagesSinceLocked(since)
}

// stagesSinceLocked is stagesSince with the lock already held.
func (r *omReader) stagesSinceLocked(since time.Time) []omStage {
	var stages []omStage
	for _, st := range r.stages {
		if !st.At.Before(since) {
			stages = append(stages, st)
		}
	}
	return stages
}

// dispatch returns the latest dispatcher tick in the log, nil when there is none.
func (r *omReader) dispatch() *dashboard.Dispatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.scanLog(); err != nil && r.lastTick == nil {
		return nil
	}
	if r.lastTick == nil {
		return nil
	}
	d := *r.lastTick
	return &d
}
