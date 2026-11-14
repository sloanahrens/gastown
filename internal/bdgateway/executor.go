package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxPumpGraces bounds how many further grace periods the drain waits for a
	// pump that is not inside a client write before it stops waiting.
	maxPumpGraces = 5
	// MaxStdinBytes caps one call's stdin body: the design's 1 MiB, sized by
	// the largest user, a bead body sent with --body-file=-.
	MaxStdinBytes = 1 << 20

	// DefaultMaxConcurrent caps bd processes running at once, the design's 16.
	DefaultMaxConcurrent = 16

	// DefaultTimeout bounds a call whose head names no timeout_ms. The design's
	// class maximums (reads 30 s, writes 60 s, list and ready 120 s) are applied
	// by the server that knows the token class (G6) before Exec; this is the
	// shortest of them, so a head that leaves timeout_ms out still cannot run
	// without a bound.
	DefaultTimeout = 30 * time.Second

	// DefaultKillGrace bounds how long Exec waits for bd's process to end after
	// the deadline's kill. A process still running past it is reported as
	// ErrKillFailed and reaped by a goroutine, so a bd that ignores the kill
	// cannot hold Exec and its concurrency slot past the deadline.
	DefaultKillGrace = 2 * time.Second
)

// The executor's named refusals. A caller matches these rather than message
// text; the HTTP layer maps them to its statuses (G6).
var (
	ErrUnconfigured        = errors.New("bdgateway: executor is not configured")
	ErrBadRig              = errors.New("bdgateway: rig does not name one workspace")
	ErrCwdOutsideWorkspace = errors.New("bdgateway: cwd is outside the workspace")
	ErrNoWorkspace         = errors.New("bdgateway: the workspace is not present")
	ErrStdinTooLarge       = errors.New("bdgateway: stdin is over the cap")
	ErrEnvNotAllowed       = errors.New("bdgateway: env holds a variable the gateway does not accept")
	ErrTooManyCalls        = errors.New("bdgateway: the concurrency cap is full")
	ErrBadTimeout          = errors.New("bdgateway: timeout_ms is not a duration the gateway can run")
	ErrTimeout             = errors.New("bdgateway: bd did not finish before the timeout")
	ErrKillFailed          = errors.New("bdgateway: bd was still running after the kill")
	ErrSignaled            = errors.New("bdgateway: bd was killed by a signal")
	ErrClientGone          = errors.New("bdgateway: the client is gone")
)

// The refusal codes an error frame carries. An exit frame always follows one.
const (
	CodeTimeout     = "timeout"
	CodeCanceled    = "canceled"
	CodeStartFailed = "bd-start"
	CodeWaitFailed  = "bd-wait"
	CodeSignaled    = "bd-signaled"
)

// Options is the executor's fixed configuration. Everything the caller may not
// set, including bd's binary and the computed half of its environment, lives
// here rather than in the request head.
type Options struct {
	// Binary is the bd program to run. Injected so a test runs a stand-in.
	Binary string
	// WorkspaceRoot is the gateway's workspace tree, /ws in the container. It
	// must be an absolute path: a relative one would resolve against the
	// process's own directory, which is not the workspace.
	WorkspaceRoot string
	// Home and Path are the child's HOME and PATH. The caller cannot set them.
	Home string
	Path string
	// DoltHost and DoltPort name the Dolt endpoint the workspace's metadata
	// resolves to; the caller cannot point bd at another database.
	DoltHost string
	DoltPort string
	// TrackerURL and TrackerTokenFile are the gateway's tracker credential,
	// never the caller's. Either may be empty when the town runs no tracker.
	TrackerURL       string
	TrackerTokenFile string
	// MaxConcurrent caps bd processes running at once; 0 means
	// DefaultMaxConcurrent.
	MaxConcurrent int
	// DefaultTimeout applies when the head's timeout_ms is 0; 0 means
	// DefaultTimeout.
	DefaultTimeout time.Duration
	// KillGrace bounds the wait for bd's process after the deadline's kill; 0
	// means DefaultKillGrace.
	KillGrace time.Duration
	// runner starts one process. Unexported so only this package's tests can
	// put a fake behind it.
	runner processRunner
}

// Executor runs one allowed bd call inside the gateway's workspace tree. The
// policy, authentication and call log decide whether a call reaches Exec
// (G2-G4); Exec maps the call to the workspace and runs it.
type Executor struct {
	binary         string
	workspaceRoot  string
	home           string
	searchPath     string
	doltHost       string
	doltPort       string
	trackerURL     string
	trackerToken   string
	defaultTimeout time.Duration
	killGrace      time.Duration
	slots          chan struct{}
	runner         processRunner
}

// New builds an executor. A field bd cannot run without is refused by name
// rather than defaulted, since a default the operator did not set is a
// workspace or a database the caller never asked for.
func New(opts Options) (*Executor, error) {
	root := filepath.Clean(opts.WorkspaceRoot)
	if opts.Binary == "" {
		return nil, fmt.Errorf("%w: no bd binary", ErrUnconfigured)
	}
	if opts.WorkspaceRoot == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: workspace root %q must be an absolute path", ErrUnconfigured, opts.WorkspaceRoot)
	}
	for _, f := range []struct{ name, value string }{
		{"HOME", opts.Home},
		{"PATH", opts.Path},
		{"Dolt host", opts.DoltHost},
		{"Dolt port", opts.DoltPort},
	} {
		if f.value == "" {
			return nil, fmt.Errorf("%w: %s is empty", ErrUnconfigured, f.name)
		}
	}
	if opts.MaxConcurrent < 0 {
		return nil, fmt.Errorf("%w: MaxConcurrent %d is negative", ErrUnconfigured, opts.MaxConcurrent)
	}
	if opts.DefaultTimeout < 0 {
		return nil, fmt.Errorf("%w: DefaultTimeout %s is negative", ErrUnconfigured, opts.DefaultTimeout)
	}
	if opts.KillGrace < 0 {
		return nil, fmt.Errorf("%w: KillGrace %s is negative", ErrUnconfigured, opts.KillGrace)
	}
	max := opts.MaxConcurrent
	if max == 0 {
		max = DefaultMaxConcurrent
	}
	limit := opts.DefaultTimeout
	if limit == 0 {
		limit = DefaultTimeout
	}
	grace := opts.KillGrace
	if grace == 0 {
		grace = DefaultKillGrace
	}
	runner := opts.runner
	if runner == nil {
		runner = realRunner{}
	}
	return &Executor{
		binary:         opts.Binary,
		workspaceRoot:  root,
		home:           opts.Home,
		searchPath:     opts.Path,
		doltHost:       opts.DoltHost,
		doltPort:       opts.DoltPort,
		trackerURL:     opts.TrackerURL,
		trackerToken:   opts.TrackerTokenFile,
		defaultTimeout: limit,
		killGrace:      grace,
		slots:          make(chan struct{}, max),
		runner:         runner,
	}, nil
}

// Exec runs one bd call: it maps the head to the workspace, builds bd's
// environment, streams stdin up to the cap, frames bd's output and exit status
// on frames, and kills the whole process group when the call times out or the
// client goes away. A refusal before the process starts returns a named error
// and writes no frames; the HTTP layer answers those before the stream begins.
// A start that fails is the one exception: the stream has begun by then, so it
// writes the error frame and the exit frame the design pairs with it.
//
// Once the deadline or a client departure fires, that ending is the call's
// answer whatever status bd later reports, and a bd still running after the
// kill's grace is ErrKillFailed rather than a wait with no bound.
func (e *Executor) Exec(ctx context.Context, req Request, stdin io.Reader, frames io.Writer) error {
	if e == nil || e.slots == nil || frames == nil {
		return ErrUnconfigured
	}
	body, err := readStdin(stdin)
	if err != nil {
		return err
	}
	dir, beadsDir, err := e.workspace(req.Rig, req.Cwd)
	if err != nil {
		return err
	}
	env, err := e.buildEnv(beadsDir, req.Env)
	if err != nil {
		return err
	}
	limit, err := e.timeout(req.TimeoutMS)
	if err != nil {
		return err
	}
	// A request the client has already ended starts no process: the kill below
	// would be its only effect, and the caller learns the same refusal without
	// the spawn.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrClientGone, err)
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return ErrTooManyCalls
	}

	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	// fw is the one writer of this call's stream: the pumps and the ending frames
	// all go through its lock, so a pump still alive when the call ends can never
	// interleave its frames with the ending's.
	fw := &frameWriter{w: frames}
	// Once Exec returns the call's stream is closed to its pumps: a pump the drain
	// gave up on, or one that outlived the call, writes nothing after the ending
	// frames. The defer runs after the ending is written.
	defer fw.discard.Store(true)
	proc, err := e.runner.Start(e.binary, req.Argv, env, dir)
	if err != nil {
		writeEnding(fw, CodeStartFailed, 125, err.Error())
		return fmt.Errorf("bdgateway: starting bd: %w", err)
	}

	// out is the one record of how this call ends. The watcher writes the
	// ending before it kills, so a status bd reports afterwards can never be
	// read as its own answer: the call the gateway ended is that ending.
	out := &callOutcome{}
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-runCtx.Done():
		case <-stopWatch:
			// The wait finished first, but the deadline or a cancel may already
			// have fired while it ran; the record says so before Exec reads it.
			if runCtx.Err() == nil {
				return
			}
		}
		out.setEnding(callEndingOf(ctx, runCtx))
		err := proc.Kill()
		switch {
		case err == nil:
			out.killedBy(nil)
		case errors.Is(err, os.ErrProcessDone):
			// bd had already ended on its own, so no signal of ours reached it.
		default:
			out.killedBy(err)
		}
	}()

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		// Errors here are bd's answer, not the request's failure: a write fails
		// when bd closed stdin or exited before reading the body, and Wait
		// reports the exit; the call's deadline still bounds a close that
		// leaves bd waiting for an EOF it never sees.
		if len(body) > 0 {
			_, _ = proc.Stdin().Write(body)
		}
		_ = proc.Stdin().Close()
	}()

	pumpErrs := make(chan error, 2)
	go func() { pumpErrs <- fw.copy(proc.Stdout(), FrameStdout) }()
	go func() { pumpErrs <- fw.copy(proc.Stderr(), FrameStderr) }()

	// A pump that cannot write is the client gone; cancel so the watcher kills
	// bd and the other pump's read ends rather than the call hanging.
	var writeErr, readErr error
	record := func(err error) {
		switch {
		case err == nil:
		case errors.Is(err, errFrameWrite):
			if writeErr == nil {
				writeErr = err
				cancel()
			}
		default:
			if readErr == nil {
				readErr = err
				cancel()
			}
		}
	}
	// The pumps are waited for, but not past the call's own end: a process that
	// outlived bd's group still holding bd's pipes would keep a read blocked
	// after the kill, so the ends this side owns are closed and the reads end.
	// The errors those closes cause are the close's, not bd's, so they are not
	// read as bd failing.
	for pending := 2; pending > 0; {
		select {
		case err := <-pumpErrs:
			pending--
			record(err)
		case <-runCtx.Done():
			_ = proc.Stdin().Close()
			proc.CloseOutputs()
			// Closing bd's ends frees a pump that is reading, but not one that
			// is writing to a client that stopped reading its stream, so the
			// wait for the pumps has the same bound as the wait for bd. A pump
			// still writing after it is the client's: it ends when its write
			// returns (pumpErrs has room for both), the call is recorded as a
			// failed write, and nothing more is written to that client.
			grace := time.NewTimer(e.killGrace)
			// A pump that missed one grace is not yet a stalled client: under load
			// it may only be slow to be scheduled. The client is declared stalled
			// when a pump is still inside a write to it at two graces in a row; a
			// pump that is not writing is waited for again, up to maxPumpGraces.
			inWriteBefore, idleGraces := false, 0
			for pending > 0 {
				select {
				case err := <-pumpErrs:
					pending--
					if !errors.Is(err, os.ErrClosed) {
						record(err)
					}
				case <-grace.C:
					switch {
					case fw.writesInProgress() > 0 && inWriteBefore:
						record(fmt.Errorf("%w: the client stopped reading its stream, %d pump(s) still writing %s after the call ended", errFrameWrite, pending, e.killGrace))
						fw.discard.Store(true) // the lock is held by the blocked write, so stop writing without it
						pending = 0
					case fw.writesInProgress() > 0:
						inWriteBefore = true
						grace.Reset(e.killGrace)
					case idleGraces >= maxPumpGraces:
						record(fmt.Errorf("bdgateway: %d stream pump(s) did not end within %d grace periods of the call's end", pending, maxPumpGraces))
						pending = 0
					default:
						inWriteBefore = false
						idleGraces++
						grace.Reset(e.killGrace)
					}
				}
			}
			grace.Stop()
		}
	}
	// The stdin write is bounded the same way: closing our end ends it even
	// when the child's reader is a process the group kill missed.
	select {
	case <-stdinDone:
	case <-runCtx.Done():
		_ = proc.Stdin().Close()
		<-stdinDone
	}

	// bd's own end is waited for, but not past the deadline's kill grace: a
	// process that survived the kill would otherwise hold Exec, and the slot it
	// took, for as long as it ran.
	waitCh := make(chan waitResult, 1)
	go func() {
		status, err := proc.Wait()
		waitCh <- waitResult{status: status, err: err}
	}()
	select {
	case w := <-waitCh:
		out.waitedAs(w.status, w.err)
	case <-runCtx.Done():
		// The call's end fired while the wait was running. Both pumps have
		// ended by here, so what is left to bound is bd's own end.
		grace := time.NewTimer(e.killGrace)
		defer grace.Stop()
		select {
		case w := <-waitCh:
			out.waitedAs(w.status, w.err)
		case <-grace.C:
			// bd is still running: the wait goroutine reaps it when it ends.
		}
	}
	close(stopWatch)
	<-watchDone

	end, killErr, killed, waited, status, waitErr := out.state()
	if !waited {
		return e.killFailed(fw, proc, end, killErr, killed, limit)
	}
	switch end {
	case deadlineEnding:
		// The deadline ended this call, so its status is not bd's answer: a
		// call killed at its deadline is never a success.
		writeEnding(fw, CodeTimeout, 124, fmt.Sprintf("bd did not finish within %s", limit))
		return withKillErr(fmt.Errorf("%w: %s", ErrTimeout, limit), killErr)
	case canceledEnding:
		switch {
		case readErr != nil:
			// A read the gateway could not make is its failure, even though the
			// cancel it caused is what ended the wait.
			writeEnding(fw, CodeWaitFailed, 125, readErr.Error())
			return withKillErr(fmt.Errorf("bdgateway: reading bd's output: %w", readErr), killErr)
		case ctx.Err() != nil:
			writeEnding(fw, CodeCanceled, 124, "the request ended before bd did")
			return withKillErr(fmt.Errorf("%w: %v", ErrClientGone, ctx.Err()), killErr)
		}
		// The gateway stopped the call because the client's stream could not be
		// written.
		writeEnding(fw, CodeCanceled, 124, "the client's stream could not be written")
		return withKillErr(fmt.Errorf("%w: %v", ErrClientGone, writeErr), killErr)
	}
	switch {
	case waitErr != nil:
		writeEnding(fw, CodeWaitFailed, 125, waitErr.Error())
		return withKillErr(fmt.Errorf("bdgateway: waiting for bd: %w", waitErr), killErr)
	case status < 0:
		writeEnding(fw, CodeSignaled, 125, "bd was killed by a signal")
		return withKillErr(ErrSignaled, killErr)
	}
	// bd exited with a status of its own, which is the answer when no ending
	// fired: the gateway did not stop this call.
	if err := fw.exit(status); err != nil {
		return fmt.Errorf("%w: writing the exit frame: %v", ErrClientGone, err)
	}
	return nil
}

// waitResult is what one Wait reported, handed from its goroutine to Exec.
type waitResult struct {
	status int
	err    error
}

// callEnding is how the gateway ended a call before bd did.
type callEnding int

const (
	noEnding callEnding = iota
	deadlineEnding
	canceledEnding
)

// callEndingOf names the ending a fired runCtx stands for. The call's own limit
// running out is the timeout; any other end is a cancel: the caller's context
// ending (its own deadline included, which is not this call's limit), or a pump
// that could not write.
func callEndingOf(caller, run context.Context) callEnding {
	if caller.Err() == nil && errors.Is(run.Err(), context.DeadlineExceeded) {
		return deadlineEnding
	}
	return canceledEnding
}

// callOutcome is one call's single record of how it ended. The ending is
// written before the kill, so the status bd reports afterwards cannot replace
// it, and Exec decides its error and frames from this record in one place.
type callOutcome struct {
	mu      sync.Mutex
	end     callEnding
	killed  bool
	killErr error
	waited  bool
	status  int
	waitErr error
}

func (o *callOutcome) setEnding(end callEnding) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.end = end
}

func (o *callOutcome) killedBy(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.killed = true
	o.killErr = err
}

func (o *callOutcome) waitedAs(status int, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.waited, o.status, o.waitErr = true, status, err
}

func (o *callOutcome) state() (end callEnding, killErr error, killed, waited bool, status int, waitErr error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.end, o.killErr, o.killed, o.waited, o.status, o.waitErr
}

// killFailed ends a call whose bd outlived the kill grace. The call returns at
// once with its named ending and ErrKillFailed, and the wait goroutine reaps bd
// when it ends, so a process that ignores the kill holds neither Exec nor the
// slot it took.
func (e *Executor) killFailed(fw *frameWriter, proc Process, end callEnding, killErr error, killed bool, limit time.Duration) error {
	named := fmt.Errorf("%w: %s", ErrTimeout, limit)
	code := CodeTimeout
	message := fmt.Sprintf("bd did not finish within %s and did not end after the kill", limit)
	if end == canceledEnding {
		named = fmt.Errorf("%w: the request ended before bd did", ErrClientGone)
		code = CodeCanceled
		message = "the request ended before bd did, and bd did not end after the kill"
	}
	failed := fmt.Errorf("%w: process %d did not end within %s of the kill", ErrKillFailed, proc.Pid(), e.killGrace)
	if !killed {
		failed = fmt.Errorf("%w: process %d never reported its end and the kill did not reach it", ErrKillFailed, proc.Pid())
	}
	if killErr != nil {
		failed = errors.Join(failed, fmt.Errorf("bdgateway: killing bd's process group: %w", killErr))
	}
	writeEnding(fw, code, 124, message)
	return errors.Join(named, failed)
}

// readStdin buffers the request body, one byte over the cap so a body past the
// cap is told apart from one exactly at it. Buffering is what lets the over-cap
// refusal happen before the process starts; 1 MiB is not worth streaming.
func readStdin(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r, MaxStdinBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bdgateway: reading stdin: %w", err)
	}
	if len(body) > MaxStdinBytes {
		return nil, fmt.Errorf("%w: over %d bytes", ErrStdinTooLarge, MaxStdinBytes)
	}
	return body, nil
}

// timeout turns the head's timeout_ms into the limit this call runs under. A
// negative value and one whose milliseconds do not fit a Duration are refused,
// so an overflow cannot become a negative (unbounded) deadline.
func (e *Executor) timeout(ms int) (time.Duration, error) {
	if ms == 0 {
		return e.defaultTimeout, nil
	}
	if ms < 0 || int64(ms) > math.MaxInt64/int64(time.Millisecond) {
		return 0, fmt.Errorf("%w: %d", ErrBadTimeout, ms)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// workspace maps rig and cwd to the directory bd runs in and the BEADS_DIR it
// reads. An empty rig is the town's own workspace (.beads at the root); an
// empty cwd is the root. Anything that is not a local relative path is refused
// before it reaches a filesystem call.
//
// The lexical checks are not enough on their own: a symbolic link inside the
// tree would carry bd, or the metadata it reads, out of the workspace even
// though every element is local. The gateway writes the workspace itself, so a
// link below its root is never its own, and each existing element below the
// root is refused if it is one.
//
// The workspace tree holds only the five files the design pushes, so the
// caller's directory is created on first use: bd starts in the directory the
// caller named, not in the root. A read-only workspace volume therefore
// refuses every call from a directory it does not already hold.
func (e *Executor) workspace(rig, cwd string) (dir, beadsDir string, err error) {
	root := e.workspaceRoot
	beadsDir = filepath.Join(root, ".beads")
	if rig != "" {
		if rig == "." || !filepath.IsLocal(rig) || strings.ContainsRune(rig, filepath.Separator) {
			return "", "", fmt.Errorf("%w: %q is not one path element", ErrBadRig, rig)
		}
		beadsDir = filepath.Join(root, rig, ".beads")
	}
	dir = root
	if cwd != "" {
		// IsLocal refuses an absolute path and any ".." that leaves the
		// subtree, so the joined path stays inside the workspace.
		if !filepath.IsLocal(cwd) {
			return "", "", fmt.Errorf("%w: %q", ErrCwdOutsideWorkspace, cwd)
		}
		dir = filepath.Join(root, cwd)
	}
	if err := noSymlinkBelow(root, beadsDir); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrNoWorkspace, err)
	}
	if err := noSymlinkBelow(root, dir); err != nil {
		// A link is the caller's path leaving the workspace; any other failure
		// is the workspace not holding the directory the caller named.
		if errors.Is(err, errSymlinkElement) {
			return "", "", fmt.Errorf("%w: %v", ErrCwdOutsideWorkspace, err)
		}
		return "", "", fmt.Errorf("%w: %v", ErrNoWorkspace, err)
	}
	fi, err := os.Stat(beadsDir)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrNoWorkspace, err)
	}
	if !fi.IsDir() {
		return "", "", fmt.Errorf("%w: %s is not a directory", ErrNoWorkspace, beadsDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("%w: creating the caller's directory %s: %v", ErrNoWorkspace, dir, err)
	}
	return dir, beadsDir, nil
}

// errSymlinkElement marks a path refused because an element below the
// workspace root is a symbolic link.
var errSymlinkElement = errors.New("bdgateway: a symbolic link below the workspace root")

// noSymlinkBelow refuses path when an element of it below root is a symbolic
// link. Elements past the first that does not exist are not checked: only
// MkdirAll creates them, and it makes real directories.
func noSymlinkBelow(root, path string) error {
	at := root
	for _, el := range strings.Split(strings.TrimPrefix(path, root), string(filepath.Separator)) {
		if el == "" || el == "." {
			continue
		}
		at = filepath.Join(at, el)
		fi, err := os.Lstat(at)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", errSymlinkElement, at)
		}
		if !fi.IsDir() {
			// A file where the path must pass through a directory: nothing
			// below it exists to be a link, and the caller's stat or mkdir
			// names the failure.
			return nil
		}
	}
	return nil
}

// buildEnv is bd's environment: the computed values first, then the caller's
// allowlisted ones, sorted so one call's environment is the same every run. The
// allowlist is callerEnv (env.go), the one list the gateway screens with, so
// the executor and the policy cannot drift apart; everything else bd needs is
// computed here, so a caller cannot aim bd at another database, another home or
// another token. A key outside the list is refused rather than dropped, so the
// caller learns it sent something the gateway does not take; BD_ALLOW_NEW_DATABASE
// is refused by the same rule, since init is not served.
func (e *Executor) buildEnv(beadsDir string, fromCaller map[string]string) ([]string, error) {
	env := []string{
		"HOME=" + e.home,
		"PATH=" + e.searchPath,
		"BEADS_DIR=" + beadsDir,
		"BEADS_DOLT_SERVER_HOST=" + e.doltHost,
		"BEADS_DOLT_SERVER_PORT=" + e.doltPort,
		"TRACKER_URL=" + e.trackerURL,
		"TRACKER_TOKEN_FILE=" + e.trackerToken,
	}
	keys := make([]string, 0, len(fromCaller))
	for k := range fromCaller {
		if !callerEnv[k] {
			return nil, fmt.Errorf("%w: %q", ErrEnvNotAllowed, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := fromCaller[k]
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("%w: %q holds a NUL byte", ErrEnvNotAllowed, k)
		}
		env = append(env, k+"="+v)
	}
	return env, nil
}

// errFrameWrite marks a pump that could not write a frame, which is the
// client's stream broken rather than bd's.
var errFrameWrite = errors.New("bdgateway: writing a frame")

// errRead marks a pump that could not read bd's output.
var errRead = errors.New("bdgateway: reading bd's output")

// frameWriter serializes the stdout and stderr pumps onto one frame stream.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
	// writing counts the writes in progress or waiting for the lock. A pump that
	// is only slow to be scheduled is not in a write; one blocked on a client that
	// stopped reading is, and that is what the drain after a call's end tells
	// apart (writesInProgress).
	writing atomic.Int32
	// discard, once set, makes every write a no-op that does not touch the lock:
	// the client stopped reading and a pump blocked in a write holds the lock.
	discard atomic.Bool
	// holds counts how many times a write took the lock; an ending is one hold.
	holds atomic.Int32
}

// writesInProgress is how many writes to the client have begun and not ended.
func (f *frameWriter) writesInProgress() int { return int(f.writing.Load()) }

func (f *frameWriter) write(kind FrameKind, payload []byte) error {
	return f.send(Frame{Kind: kind, Payload: payload})
}

// send is the one place a frame reaches the client: it counts the write, honours
// discard without touching the lock, and writes under the lock.
func (f *frameWriter) send(fr Frame) error {
	return f.sendAll(fr)
}

// sendAll writes frames in order under one hold of the lock, so nothing else
// writes between them.
func (f *frameWriter) sendAll(frames ...Frame) error {
	if f.discard.Load() {
		return nil
	}
	f.writing.Add(1)
	defer f.writing.Add(-1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holds.Add(1)
	for _, fr := range frames {
		if err := WriteFrame(f.w, fr); err != nil {
			return err
		}
	}
	return nil
}

// copy frames r until it ends. A read or write failure stops the copy, so the
// caller can kill the process rather than let a broken stream hold it.
func (f *frameWriter) copy(r io.Reader, kind FrameKind) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := f.write(kind, buf[:n]); werr != nil {
				return fmt.Errorf("%w: %v", errFrameWrite, werr)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// Both are wrapped so a caller can tell a read this package's own
			// close ended (os.ErrClosed) from one bd's output caused.
			return fmt.Errorf("%w: %w", errRead, err)
		}
	}
}

func (f *frameWriter) exit(status int) error {
	return f.send(ExitFrame(status))
}

// withKillErr adds a failed kill to the refusal that ended a call, so a call
// that could not stop bd's group is not reported as one that could.
func withKillErr(err, killErr error) error {
	if killErr == nil {
		return err
	}
	return errors.Join(err, fmt.Errorf("bdgateway: killing bd's process group: %w", killErr))
}

// writeEnding writes the frame that explains a call the gateway ended, then
// the exit frame the design pairs with it. A failed write is the client being
// gone, which the caller already learns from the returned error.
func writeEnding(fw *frameWriter, code string, status int, message string) {
	payload, err := json.Marshal(ErrorPayload{Code: code, Message: message})
	if err != nil {
		return
	}
	// One lock hold writes both frames, so a pump that is alive but not stalled
	// can never put a frame between the error frame and its exit frame.
	_ = fw.sendAll(Frame{Kind: FrameError, Payload: payload}, ExitFrame(status))
}
