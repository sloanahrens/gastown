package bdgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// The gateway's HTTP surface, the one the design's "The gateway protocol"
// section names: POST /v1/exec runs one bd call, /healthz and /readyz answer
// health, PUT /v1/workspace takes the workspace tree and GET /v1/stats the
// counters. The command in cmd/bdgw builds a Gateway from the pieces and serves
// it; exec mode reuses the same policy, executor and call log without HTTP.
const (
	execPath      = "/v1/exec"
	healthPath    = "/healthz"
	readyPath     = "/readyz"
	workspacePath = "/v1/workspace"
	statsPath     = "/v1/stats"
)

// The refusal codes a JSON refusal carries. The forwarder shows them to the
// caller, so they are stable names, not sentences.
const (
	codeUnauthorized  = "unauthorized"
	codeForbidden     = "forbidden"
	codeBadRequest    = "bad_request"
	codeStdinTooLarge = "stdin_too_large"
	codeTooManyCalls  = "too_many_calls"
	codeUnavailable   = "unavailable"
	codeMethod        = "method_not_allowed"
	codeNotFound      = "not_found"
	codeInternal      = "gateway_failed"
)

const (
	// readTimeoutCap is the design's maximum for a read verb.
	readTimeoutCap = 30 * time.Second
	// writeTimeoutCap is the maximum for a write or admin verb.
	writeTimeoutCap = 60 * time.Second
	// largeReadTimeoutCap is the maximum for the two reads that grow with a
	// rig: list and ready on a large rig take longer than a show.
	largeReadTimeoutCap = 120 * time.Second
	// statusClientClosed is nginx's 499: the caller ended the request before
	// the gateway answered. It is only sent when the executor refused before
	// starting bd because the request was already over; nobody reads it.
	statusClientClosed = 499
)

// CallRunner runs one authorized bd call. *Executor is the production one; a
// test injects a stand-in, so it can return each refusal or hold a call in
// flight without a process starting.
type CallRunner interface {
	Exec(ctx context.Context, req Request, stdin io.Reader, frames io.Writer) error
}

// GatewayOptions is what a Gateway is built from. Every field is required: a
// gateway missing one refuses to start rather than serving with the check it
// stands for silently off.
type GatewayOptions struct {
	// Auth loads and checks the token directory.
	Auth *Authenticator
	// Policy is the loaded verbs file.
	Policy *Policy
	// Workspaces is the workspace tree PUT /v1/workspace swaps.
	Workspaces *WorkspaceStore
	// Log records one line per bd call that ran.
	Log *CallLog
	// Runner runs the calls. The production value is New's *Executor.
	Runner CallRunner
}

// Gateway is the HTTP handler: it authenticates a request, applies the policy,
// runs the call through Runner, logs it, and answers health, the workspace and
// the counters. It refuses new calls once BeginShutdown is called and lets the
// ones already running finish.
type Gateway struct {
	auth       *Authenticator
	policy     *Policy
	workspaces *WorkspaceStore
	log        *CallLog
	runner     CallRunner
	closing    atomic.Bool
}

// NewGateway builds a Gateway. A missing piece is refused by name: an unwired
// dependency is not a pass, and the refusal names it so the operator fixes the
// wiring instead of watching every call fail.
func NewGateway(opts GatewayOptions) (*Gateway, error) {
	for _, f := range []struct {
		name  string
		empty bool
	}{
		{"authenticator", opts.Auth == nil},
		{"policy", opts.Policy == nil},
		{"workspace store", opts.Workspaces == nil},
		{"call log", opts.Log == nil},
		{"call runner", opts.Runner == nil},
	} {
		if f.empty {
			return nil, fmt.Errorf("%w: no %s", ErrUnconfigured, f.name)
		}
	}
	return &Gateway{
		auth:       opts.Auth,
		policy:     opts.Policy,
		workspaces: opts.Workspaces,
		log:        opts.Log,
		runner:     opts.Runner,
	}, nil
}

// BeginShutdown refuses every call that has not started yet. A call already in
// Exec keeps its slot and finishes; the caller of BeginShutdown waits for the
// HTTP server's drain to see them return.
func (g *Gateway) BeginShutdown() { g.closing.Store(true) }

// Ready reports the first reason the gateway cannot serve a call. The token
// table and the verbs file are loaded before a Gateway is built, since a failed
// load stops the process, so the state that changes while it serves is the
// workspace: a fresh volume has none until the first push, and readiness is
// false until then. A dependency that is missing entirely is still a reason, so
// a Gateway built without one never reports ready. The bd binary is not probed:
// readiness never starts bd, so a missing binary surfaces on the first call.
func (g *Gateway) Ready() error {
	switch {
	case g.auth == nil:
		return errors.New("bdgateway: no token loader is configured")
	case !g.auth.Loaded():
		return errors.New("bdgateway: no token table is loaded")
	case g.policy == nil:
		return errors.New("bdgateway: no verbs file is loaded")
	case g.workspaces == nil || !g.workspaces.Loaded():
		return errors.New("bdgateway: no workspace is loaded")
	}
	return nil
}

// ServeHTTP routes one request. Every response, refusal included, carries the
// live workspace hash, which the forwarder and the doctor compare with the
// files they believe current.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.workspaces != nil {
		g.workspaces.AddWorkspaceHeader(w.Header())
	}
	switch r.URL.Path {
	case healthPath:
		g.serveHealth(w, r)
	case readyPath:
		g.serveReady(w, r)
	case execPath:
		g.serveExec(w, r)
	case workspacePath:
		g.serveWorkspace(w, r)
	case statsPath:
		g.serveStats(w, r)
	default:
		refuse(w, http.StatusNotFound, codeNotFound, "no such endpoint")
	}
}

// serveHealth answers 200 whenever the process is serving. It takes no token:
// a health check that needed one would be the readiness check.
func (g *Gateway) serveHealth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// The status is already sent; a body that cannot be written is the caller
	// having gone, which needs no second answer.
	_, _ = io.WriteString(w, "ok\n")
}

// serveReady answers 200 only when the gateway can serve a call: a read-class
// token authenticates, the token table, the verbs file and a workspace are
// loaded, and no shutdown is under way.
func (g *Gateway) serveReady(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ident, err := g.auth.Authenticate(r.Header.Get("Authorization"))
	if err != nil {
		refuseAuth(w, err)
		return
	}
	if err := ident.Require(ClassRead); err != nil {
		refuse(w, http.StatusForbidden, codeForbidden, err.Error())
		return
	}
	if g.closing.Load() {
		refuse(w, http.StatusServiceUnavailable, codeUnavailable, "the gateway is shutting down")
		return
	}
	if err := g.Ready(); err != nil {
		refuse(w, http.StatusServiceUnavailable, codeUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// serveExec is POST /v1/exec: authenticate, apply the policy, run the call and
// stream its frames. The HTTP status is decided before anything is written: a
// refusal the executor makes before it starts bd is still an HTTP status, and
// the 200 goes out with the first frame.
func (g *Gateway) serveExec(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if g.closing.Load() {
		refuse(w, http.StatusServiceUnavailable, codeUnavailable, "the gateway is shutting down")
		return
	}
	start := time.Now()
	req, stdin, err := ReadRequest(r.Body)
	if err != nil {
		refuse(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ident, err := g.auth.Authenticate(r.Header.Get("Authorization"))
	if err != nil {
		refuseAuth(w, err)
		return
	}
	verb, err := g.policy.Authorize(ident.Class, req.Argv)
	if err != nil {
		refuse(w, http.StatusForbidden, codeForbidden, err.Error())
		return
	}
	req.TimeoutMS = CapTimeout(req.TimeoutMS, verb)

	frames := &httpFrames{w: w}
	if err := g.runner.Exec(r.Context(), req, stdin, frames); !frames.Started() {
		status, code, message := execRefusal(err)
		refuse(w, status, code, message)
		return
	}
	// A call that started is answered by its own frames: the executor writes the
	// error frame and the exit frame that pairs with any ending it caused, so
	// nothing is left for this handler to say to the caller.
	status, seen := frames.ExitStatus()
	if !seen {
		// The stream began but no exit frame reached the caller: it stopped
		// reading, and the executor's ending is the reason. Count it as the
		// gateway's failed call rather than a status bd chose.
		status = 125
	}
	g.log.Log(CallRecord{
		Class:    string(ident.Class),
		Caller:   r.Header.Get("X-Gt-Caller"),
		Argv:     req.Argv,
		ExitCode: status,
		Duration: time.Since(start),
	})
}

// serveWorkspace is PUT /v1/workspace. Only a class that may write may push:
// the workspace is what every call's BEADS_DIR resolves under, so a read token
// changing it would let a reader aim a writer's calls.
func (g *Gateway) serveWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	if g.closing.Load() {
		refuse(w, http.StatusServiceUnavailable, codeUnavailable, "the gateway is shutting down")
		return
	}
	ident, err := g.auth.Authenticate(r.Header.Get("Authorization"))
	if err != nil {
		refuseAuth(w, err)
		return
	}
	if err := ident.Require(ClassWrite); err != nil {
		refuse(w, http.StatusForbidden, codeForbidden, err.Error())
		return
	}
	g.workspaces.ServeHTTP(w, r)
}

// serveStats is GET /v1/stats: the counter snapshot the call log keeps.
func (g *Gateway) serveStats(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ident, err := g.auth.Authenticate(r.Header.Get("Authorization"))
	if err != nil {
		refuseAuth(w, err)
		return
	}
	if err := ident.Require(ClassRead); err != nil {
		refuse(w, http.StatusForbidden, codeForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, g.log.Stats())
}

// requireMethod answers 405 when the request's method is not want, and reports
// whether the handler should go on.
func requireMethod(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	refuse(w, http.StatusMethodNotAllowed, codeMethod, "this endpoint takes "+want)
	return false
}

// refuseAuth answers an authentication refusal with the status HTTPStatus maps
// it to: 401 for no or unknown token, 500 for a failure this package does not
// know, never a 200.
func refuseAuth(w http.ResponseWriter, err error) {
	status := HTTPStatus(err)
	code := codeUnauthorized
	if status != http.StatusUnauthorized {
		code = codeInternal
	}
	refuse(w, status, code, err.Error())
}

// refuse answers with a JSON error body. The status line goes out with the
// headers, so an encode failure after it has no second answer to give.
func refuse(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorPayload{Code: code, Message: message})
}

// writeJSON answers with a JSON body. The status line goes out with the
// headers, so an encode failure after it has no second answer to give.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// execRefusal maps a refusal the executor made before it started bd to the HTTP
// status the design lists. An error this function does not know is a 500, never
// a 200: a run that did not start must not read as one that did.
func execRefusal(err error) (status int, code, message string) {
	switch {
	case err == nil:
		// Started and returned no error without a frame cannot happen: every
		// path that starts bd writes an exit frame. A nil here is a runner that
		// broke that contract, and it is not read as success.
		return http.StatusInternalServerError, codeInternal, "the call returned without a frame"
	case errors.Is(err, ErrStdinTooLarge):
		return http.StatusRequestEntityTooLarge, codeStdinTooLarge, err.Error()
	case errors.Is(err, ErrTooManyCalls):
		return http.StatusTooManyRequests, codeTooManyCalls, err.Error()
	case errors.Is(err, ErrBadRig), errors.Is(err, ErrCwdOutsideWorkspace), errors.Is(err, ErrNoWorkspace),
		errors.Is(err, ErrBadTimeout), errors.Is(err, ErrEnvNotAllowed):
		return http.StatusBadRequest, codeBadRequest, err.Error()
	case errors.Is(err, ErrClientGone):
		return statusClientClosed, codeUnavailable, err.Error()
	case errors.Is(err, ErrUnconfigured):
		return http.StatusInternalServerError, codeInternal, err.Error()
	default:
		return http.StatusInternalServerError, codeInternal, err.Error()
	}
}

// httpFrames forwards the executor's frames to an HTTP response. It reads each
// frame the executor writes (WriteFrame writes a whole frame in one Write) so it
// knows whether the stream began and what exit status it carried. The 200 and
// the workspace header go out with the first frame, which is what keeps a
// pre-start refusal an HTTP status.
type httpFrames struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	started bool
	status  int
	exited  bool
}

// Write forwards one frame. A frame it cannot read is returned as a write
// failure, so the executor kills bd and ends the call rather than streaming
// bytes this gateway does not understand.
func (f *httpFrames) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	frame, err := ReadFrame(bytes.NewReader(p))
	if err != nil {
		return 0, err
	}
	if !f.started {
		f.w.WriteHeader(http.StatusOK)
		f.started = true
	}
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	if frame.Kind == FrameExit {
		if status, err := frame.ExitStatus(); err == nil {
			f.status, f.exited = status, true
		}
	}
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return len(p), nil
}

// Started reports whether any frame reached the caller, which is whether bd
// started and the stream is a 200 rather than a refusal.
func (f *httpFrames) Started() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

// ExitStatus returns the status the exit frame carried, if one did.
func (f *httpFrames) ExitStatus() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.exited
}

// CapTimeout clamps a head's timeout_ms to the design's class maximum. A zero
// is left to the executor's default, and a negative one to its refusal, so this
// only narrows a limit the caller named. Both transports apply it, so a local
// bdgw exec call runs under the same class bounds as an HTTP one.
func CapTimeout(ms int, verb Verb) int {
	if ms <= 0 {
		return ms
	}
	limit := timeoutCap(verb).Milliseconds()
	if int64(ms) > limit {
		return int(limit)
	}
	return ms
}

// timeoutCap is the longest one verb may run: reads 30 s, with list and ready
// 120 s on a large rig, and writes and admin verbs 60 s (the design's class
// maximums).
func timeoutCap(verb Verb) time.Duration {
	if verb.Class != ClassRead {
		return writeTimeoutCap
	}
	switch verb.Name() {
	case "list", "ready":
		return largeReadTimeoutCap
	}
	return readTimeoutCap
}
