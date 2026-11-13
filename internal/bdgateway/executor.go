package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MaxStdinBytes caps one call's stdin body: the design's 1 MiB, sized by
	// the largest user, a bead body sent with --body-file=-.
	MaxStdinBytes = 1 << 20

	// DefaultMaxConcurrent caps bd processes running at once, the design's 16.
	DefaultMaxConcurrent = 16

	// DefaultTimeout bounds a call whose head names no timeout_ms. The class
	// maximums (reads 30 s, writes 60 s) are the policy's (G2); this is the
	// shortest of them, so a head that leaves timeout_ms out still cannot run
	// without a bound.
	DefaultTimeout = 30 * time.Second
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
	max := opts.MaxConcurrent
	if max == 0 {
		max = DefaultMaxConcurrent
	}
	limit := opts.DefaultTimeout
	if limit == 0 {
		limit = DefaultTimeout
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
		slots:          make(chan struct{}, max),
		runner:         runner,
	}, nil
}

// Exec runs one bd call: it maps the head to the workspace, builds bd's
// environment, streams stdin up to the cap, frames bd's output and exit status
// on frames, and kills the whole process group when the call times out or the
// client goes away. A refusal before the process starts returns a named error
// and writes no frames; the HTTP layer answers those before the stream begins.
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
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return ErrTooManyCalls
	}

	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	proc, err := e.runner.Start(e.binary, req.Argv, env, dir)
	if err != nil {
		writeEnding(frames, CodeStartFailed, 125, err.Error())
		return fmt.Errorf("bdgateway: starting bd: %w", err)
	}

	// The watcher kills the group at the deadline or when the client goes
	// away. A group that ignores SIGTERM costs KillProcessGroup's grace past
	// the deadline (gt-6t43); the HTTP layer's own bound must allow for it.
	// A kill that fails is carried into the returned error, so a call that
	// could not stop bd does not read as one that did.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	var killErr error
	go func() {
		defer close(watchDone)
		select {
		case <-runCtx.Done():
			if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				killErr = err
			}
		case <-stopWatch:
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

	fw := &frameWriter{w: frames}
	pumpErrs := make(chan error, 2)
	go func() { pumpErrs <- fw.copy(proc.Stdout(), FrameStdout) }()
	go func() { pumpErrs <- fw.copy(proc.Stderr(), FrameStderr) }()

	// A pump that cannot write is the client gone; cancel so the watcher kills
	// bd and the other pump's read ends rather than the call hanging.
	var writeErr, readErr error
	for i := 0; i < 2; i++ {
		err := <-pumpErrs
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
	<-stdinDone
	status, waitErr := proc.Wait()
	close(stopWatch)
	<-watchDone

	switch {
	case ctx.Err() != nil:
		writeEnding(frames, CodeCanceled, 124, "the request ended before bd did")
		return withKillErr(fmt.Errorf("%w: %v", ErrClientGone, ctx.Err()), killErr)
	case writeErr != nil:
		return withKillErr(fmt.Errorf("%w: %v", ErrClientGone, writeErr), killErr)
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		writeEnding(frames, CodeTimeout, 124, fmt.Sprintf("bd did not finish within %s", limit))
		return withKillErr(fmt.Errorf("%w: %s", ErrTimeout, limit), killErr)
	case readErr != nil:
		writeEnding(frames, CodeWaitFailed, 125, readErr.Error())
		return withKillErr(fmt.Errorf("bdgateway: reading bd's output: %w", readErr), killErr)
	case waitErr != nil:
		writeEnding(frames, CodeWaitFailed, 125, waitErr.Error())
		return withKillErr(fmt.Errorf("bdgateway: waiting for bd: %w", waitErr), killErr)
	case status < 0:
		writeEnding(frames, CodeSignaled, 125, "bd was killed by a signal")
		return withKillErr(ErrSignaled, killErr)
	}
	if err := fw.exit(status); err != nil {
		return fmt.Errorf("%w: writing the exit frame: %v", ErrClientGone, err)
	}
	return nil
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
		if !filepath.IsLocal(cwd) {
			return "", "", fmt.Errorf("%w: %q", ErrCwdOutsideWorkspace, cwd)
		}
		dir = filepath.Join(root, cwd)
		// IsLocal already refuses ".."; the Rel check is the second read of the
		// joined path, so a bug in one keeps the other between a caller and a
		// directory outside the workspace.
		rel, relErr := filepath.Rel(root, dir)
		if relErr != nil || !filepath.IsLocal(rel) {
			return "", "", fmt.Errorf("%w: %q", ErrCwdOutsideWorkspace, cwd)
		}
	}
	fi, err := os.Stat(beadsDir)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrNoWorkspace, err)
	}
	if !fi.IsDir() {
		return "", "", fmt.Errorf("%w: %s is not a directory", ErrNoWorkspace, beadsDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("bdgateway: creating the caller's directory: %w", err)
	}
	return dir, beadsDir, nil
}

// callerEnvKeys is the half of bd's environment the caller may set, the
// design's allowlist. Everything else bd needs is computed by buildEnv, so a
// caller cannot aim bd at another database, another home or another token;
// BD_ALLOW_NEW_DATABASE is refused by the same rule, since init is not served.
var callerEnvKeys = map[string]bool{
	"BD_ACTOR":            true,
	"BEADS_ACTOR":         true,
	"BD_MACHINE":          true,
	"BD_JSON_ENVELOPE":    true,
	"BD_READONLY":         true,
	"BD_DOLT_AUTO_COMMIT": true,
}

// buildEnv is bd's environment: the computed values first, then the caller's
// allowlisted ones, sorted so one call's environment is the same every run.
// A key outside the allowlist is refused rather than dropped, so the caller
// learns it sent something the gateway does not take.
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
		if !callerEnvKeys[k] {
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
}

func (f *frameWriter) write(kind FrameKind, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return WriteFrame(f.w, Frame{Kind: kind, Payload: payload})
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
			return fmt.Errorf("%w: %v", errRead, err)
		}
	}
}

func (f *frameWriter) exit(status int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return WriteFrame(f.w, ExitFrame(status))
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
func writeEnding(out io.Writer, code string, status int, message string) {
	payload, err := json.Marshal(ErrorPayload{Code: code, Message: message})
	if err != nil {
		return
	}
	fw := &frameWriter{w: out}
	if err := fw.write(FrameError, payload); err != nil {
		return
	}
	_ = fw.exit(status)
}
