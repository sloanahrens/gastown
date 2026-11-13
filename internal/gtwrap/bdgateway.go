package gtwrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// This file is the bdgateway target: it sends one bd call to the bd gateway over
// HTTP and passes the gateway's output and exit status back unchanged, so a
// caller runs bd without the binary or the town being in its own container. The
// design is in docs/design/bd-container.md, "The gateway protocol" and "Call path
// from each caller".

const (
	// gatewayExecPath is the one endpoint that runs a bd call.
	gatewayExecPath = "/v1/exec"
	// gatewayReadyPath is the authenticated health endpoint the doctor asks. The
	// design has it answer only when the gateway can run bd and read its
	// workspace, so it checks reachability and the token in one request.
	gatewayReadyPath = "/readyz"
	// gatewayContentType is the media type of the request head.
	gatewayContentType = "application/vnd.gt.bdexec+json"
	// gatewayDefaultCaller is X-Gt-Caller when the target config names none. The
	// header is free text the gateway logs, never a decision input.
	gatewayDefaultCaller = "gt-wrap"
	// gatewayDefaultTimeoutMS is the head's limit when the target config names
	// none: the design's largest class cap, so the gateway's own cap decides.
	gatewayDefaultTimeoutMS = 120_000
	// gatewayBodyLimit bounds a non-200 response body read for its refusal. The
	// gateway's refusals are one small JSON object.
	gatewayBodyLimit = 8 << 10
	// gatewayPermitVar is the one variable the gateway refuses with every class,
	// because init is not served. Screening it out silently would turn a caller's
	// "create the database" into a call it never made, so the call is refused.
	gatewayPermitVar = "BD_ALLOW_NEW_DATABASE"
)

// gatewayEnvNames are the only variables the gateway accepts in the head
// (design, "Environment allowlist"). The wrapper forwards just these: BEADS_DIR
// and the Dolt host are computed by the gateway from rig, so they never cross.
var gatewayEnvNames = []string{
	"BD_ACTOR", "BEADS_ACTOR", "BD_MACHINE", "BD_JSON_ENVELOPE", "BD_READONLY", "BD_DOLT_AUTO_COMMIT",
}

// HTTPDoer sends one request and returns its response. *http.Client is one; a
// test scripts the gateway through a fake, so a unit test opens no socket.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// BdGateway is the bdgateway target. It sends one bd call to the gateway and
// writes the gateway's stdout and stderr frames to Stdout and Stderr as they
// arrive, then returns the exit status the gateway's exit frame carries.
type BdGateway struct {
	// Town is the town root the caller runs in: BEADS_DIR and the working
	// directory are translated relative to it, so no absolute path is sent.
	Town string
	// URL is the gateway's base URL, such as http://host.docker.internal:7440.
	URL string
	// TokenFile is the file holding the caller's bearer token.
	TokenFile string
	// Caller is the X-Gt-Caller value the gateway logs.
	Caller string
	// TimeoutMS is the head's requested limit. Zero means gatewayDefaultTimeoutMS.
	TimeoutMS int
	// HTTP sends the requests. Nil uses an http.Client bounded by the call's
	// timeout, or by the probe bound for a doctor.
	HTTP HTTPDoer
	// Stdin is the caller's stdin, streamed after the head line. Nil sends none.
	Stdin io.Reader
	// Stdout and Stderr receive bd's own streams. Nil refuses the call, so the
	// tool's output is never dropped.
	Stdout io.Writer
	Stderr io.Writer
	// Note receives the explanation when the working directory is outside the
	// town and runs in the town root instead. Nil refuses the call, so a mapped
	// directory is never silent.
	Note io.Writer
}

// Launch runs call through the gateway and returns bd's exit status. An error
// means the call could not be made or the gateway refused it; when the gateway
// sent an error frame, the exit frame after it still passes its status (125)
// through, as the design says.
func (g BdGateway) Launch(call Call) (int, error) {
	if err := g.validate(); err != nil {
		return exitDown, err
	}
	if err := checkCall(call); err != nil {
		return exitDown, err
	}
	if call.Argv[0] != "bd" {
		return exitDown, fmt.Errorf("gtwrap: the bdgateway target runs bd, but the call names %q", call.Argv[0])
	}
	rig, err := g.beadRig(call.Env)
	if err != nil {
		return exitDown, err
	}
	env, err := gatewayEnv(call.Env)
	if err != nil {
		return exitDown, err
	}
	token, err := readToken(g.TokenFile)
	if err != nil {
		return exitDown, err
	}
	head, err := json.Marshal(bdgateway.Request{
		Argv:      call.Argv[1:],
		Env:       env,
		Rig:       rig,
		Cwd:       g.dirRelative(call.Dir),
		TimeoutMS: g.timeoutMS(),
	})
	if err != nil {
		return exitDown, fmt.Errorf("gtwrap: build the bd gateway request head: %w", err)
	}
	stdin := g.Stdin
	if call.Stdin != nil {
		stdin = call.Stdin
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	body := io.MultiReader(bytes.NewReader(append(head, '\n')), stdin)
	req, err := http.NewRequest(http.MethodPost, g.endpoint(gatewayExecPath), body)
	if err != nil {
		return exitDown, fmt.Errorf("gtwrap: build the bd gateway request for %s: %w", g.URL, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", gatewayContentType)
	req.Header.Set("X-Gt-Caller", g.caller())
	resp, err := g.doer(g.clientTimeout()).Do(req)
	if err != nil {
		return exitDown, fmt.Errorf("gtwrap: the bd gateway at %s did not answer, so the call did not run: %w", g.URL, err)
	}
	// The response is fully read by refusal or by readFrames, so a close error
	// cannot change the status the caller gets.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return exitDown, g.refusal(resp)
	}
	return g.readFrames(resp.Body)
}

// readFrames copies the response's stdout and stderr frames to the caller's
// streams and returns the exit frame's status. An error frame is remembered and
// the exit frame that follows it still passes its status through, so the caller
// sees the gateway's 125 rather than a wrapper guess.
func (g BdGateway) readFrames(r io.Reader) (int, error) {
	var refusal error
	for {
		frame, err := bdgateway.ReadFrame(r)
		switch {
		case errors.Is(err, io.EOF):
			return exitDown, errors.New("gtwrap: the bd gateway's answer ended before an exit status, so the call's result is unknown")
		case err != nil:
			return exitDown, fmt.Errorf("gtwrap: read the bd gateway's answer: %w", err)
		}
		switch frame.Kind {
		case bdgateway.FrameStdout:
			if _, err := g.Stdout.Write(frame.Payload); err != nil {
				return exitDown, fmt.Errorf("gtwrap: write bd's stdout: %w", err)
			}
		case bdgateway.FrameStderr:
			if _, err := g.Stderr.Write(frame.Payload); err != nil {
				return exitDown, fmt.Errorf("gtwrap: write bd's stderr: %w", err)
			}
		case bdgateway.FrameError:
			payload, err := frame.GatewayError()
			if err != nil {
				return exitDown, fmt.Errorf("gtwrap: the bd gateway sent an error frame: %w", err)
			}
			refusal = fmt.Errorf("gtwrap: the bd gateway refused the call: %s: %s", payload.Code, payload.Message)
		case bdgateway.FrameExit:
			status, err := frame.ExitStatus()
			if err != nil {
				return exitDown, fmt.Errorf("gtwrap: the bd gateway's exit frame: %w", err)
			}
			return status, refusal
		}
	}
}

// refusal turns an HTTP refusal into the one-line error the caller sees. The
// gateway's JSON code and message are quoted; a body that is not that shape is
// named by its status alone, since captured output can hold anything.
func (g BdGateway) refusal(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, gatewayBodyLimit))
	var payload bdgateway.ErrorPayload
	if err == nil && json.Unmarshal(data, &payload) == nil && payload.Code != "" {
		return fmt.Errorf("gtwrap: the bd gateway at %s refused the call with HTTP %d: %s: %s", g.URL, resp.StatusCode, payload.Code, payload.Message)
	}
	return fmt.Errorf("gtwrap: the bd gateway at %s refused the call with HTTP %d, so the call did not run", g.URL, resp.StatusCode)
}

// Doctor reports whether a call could run: the token file readable, and the
// gateway answering /readyz with that token. The first step that fails is the
// reason, and the URL and token file are named so the operator knows what was
// checked. A call that would run is the exec rung.
func (g BdGateway) Doctor() (Diagnosis, error) {
	if err := g.validate(); err != nil {
		return Diagnosis{}, err
	}
	diag := Diagnosis{Rung: RungDown, URL: g.URL, TokenFile: g.TokenFile}
	token, err := readToken(g.TokenFile)
	if err != nil {
		diag.Reason = err.Error()
		return diag, nil
	}
	req, err := http.NewRequest(http.MethodGet, g.endpoint(gatewayReadyPath), nil)
	if err != nil {
		return Diagnosis{}, fmt.Errorf("gtwrap: build the bd gateway readiness request for %s: %w", g.URL, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.doer(probeTimeout).Do(req)
	if err != nil {
		diag.Reason = fmt.Sprintf("the bd gateway at %s did not answer, so nothing can run through it: %v", g.URL, err)
		return diag, nil
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained so the connection can be reused. A read failure here
	// cannot change the verdict: the status was already read, and the body is
	// only a health answer.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, gatewayBodyLimit))
	switch {
	case resp.StatusCode == http.StatusOK:
		diag.Rung = RungExec
		diag.Reason = fmt.Sprintf("the bd gateway at %s answers %s with the token in %s", g.URL, gatewayReadyPath, g.TokenFile)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		diag.Reason = fmt.Sprintf("the bd gateway at %s refused the token in %s with HTTP %d; the token file must hold a current gateway token", g.URL, g.TokenFile, resp.StatusCode)
	default:
		diag.Reason = fmt.Sprintf("the bd gateway at %s answered %s with HTTP %d, so it is not ready to run bd", g.URL, gatewayReadyPath, resp.StatusCode)
	}
	return diag, nil
}

// validate refuses a target that is not fully set up. An unwired dependency is
// refused by name, since an unwired check is not a pass.
func (g BdGateway) validate() error {
	switch {
	case g.Town == "" || !filepath.IsAbs(g.Town):
		return fmt.Errorf("gtwrap: the bdgateway target's town root is %q; it must be an absolute path", g.Town)
	case g.TokenFile == "":
		return errors.New("gtwrap: the bdgateway target names no token file, so a call has no token")
	case g.Stdout == nil || g.Stderr == nil:
		return errors.New("gtwrap: no output writer is wired for the bdgateway target, so bd's output would be lost; the call is refused")
	case g.Note == nil:
		return errors.New("gtwrap: no note writer is wired for the bdgateway target, so a mapped directory would be silent; the call is refused")
	}
	u, err := url.Parse(g.URL)
	if err != nil {
		return fmt.Errorf("gtwrap: the bdgateway target's url %q does not parse: %v", g.URL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("gtwrap: the bdgateway target's url %q is not an http or https URL with a host", g.URL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("gtwrap: the bdgateway target's url %q carries a query or fragment, which the gateway endpoint does not take", g.URL)
	}
	return nil
}

// beadRig names the rig the gateway builds BEADS_DIR from: the caller's absolute
// BEADS_DIR relative to the town root with its ".beads" component removed, which
// is "" for the town's own beads. The absolute path never crosses; the gateway
// joins the rig onto its own workspace. A BEADS_DIR the workspace cannot hold is
// refused rather than sent, so the gateway never runs in the wrong database.
func (g BdGateway) beadRig(env []string) (string, error) {
	beadsDir, ok := envValue(env, "BEADS_DIR")
	if !ok || beadsDir == "" {
		return "", errors.New("gtwrap: the call sets no BEADS_DIR, so the bd gateway cannot be told which rig it runs in; set BEADS_DIR to the town's or a rig's .beads directory")
	}
	if !filepath.IsAbs(beadsDir) {
		return "", fmt.Errorf("gtwrap: BEADS_DIR is %q, which is not an absolute path, so the bd gateway cannot place it in its workspace", beadsDir)
	}
	rel, err := filepath.Rel(g.Town, beadsDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gtwrap: BEADS_DIR %q is outside the town %s, so the bd gateway cannot place it in its workspace", beadsDir, g.Town)
	}
	if filepath.Base(rel) != ".beads" {
		return "", fmt.Errorf("gtwrap: BEADS_DIR %q does not name a .beads directory, so the bd gateway cannot tell which rig it is", beadsDir)
	}
	rig := filepath.Dir(rel)
	if rig == "." {
		return "", nil
	}
	return filepath.ToSlash(rig), nil
}

// dirRelative maps the caller's working directory onto the gateway's workspace,
// which mirrors the town. A directory under the town keeps its path relative to
// it; any other directory runs in the town root, and the note says so, since the
// protocol has no way to name an absolute path.
func (g BdGateway) dirRelative(dir string) string {
	if dir == "" {
		return "."
	}
	rel, err := filepath.Rel(g.Town, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fmt.Fprintf(g.Note, "gt-wrap: %s is outside the town %s, which the bd gateway's workspace does not mirror, so bd runs in the town root\n", dir, g.Town)
		return "."
	}
	return filepath.ToSlash(rel)
}

// gatewayEnv screens the caller's environment down to the names the gateway
// accepts. Everything else is dropped, since the gateway computes what it needs
// from rig and refuses what it does not know; the one name it refuses outright
// is turned into a refusal here so the caller learns it before the request.
func gatewayEnv(env []string) (map[string]string, error) {
	out := make(map[string]string, len(gatewayEnvNames))
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if name == gatewayPermitVar && value != "" {
			return nil, fmt.Errorf("gtwrap: the call sets %s, which the bd gateway refuses with every class: init is not served over the gateway", name)
		}
		if slices.Contains(gatewayEnvNames, name) {
			out[name] = value
		}
	}
	return out, nil
}

// envValue returns the value of the entry naming name.
func envValue(env []string, name string) (string, bool) {
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok && key == name {
			return value, true
		}
	}
	return "", false
}

// readToken reads the bearer token from its file, trimming the trailing newline a
// file written by hand or by a secret mount carries. The value is never written
// to an error.
func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("gtwrap: read the bd gateway token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("gtwrap: the bd gateway token file %s is empty, so the call has no token", path)
	}
	return token, nil
}

// endpoint is the gateway URL for one path. A trailing slash on the base URL is
// not part of the endpoint, so the two forms name the same place.
func (g BdGateway) endpoint(path string) string {
	return strings.TrimRight(g.URL, "/") + path
}

// caller is the X-Gt-Caller value: the configured name, or the default.
func (g BdGateway) caller() string {
	if g.Caller != "" {
		return g.Caller
	}
	return gatewayDefaultCaller
}

// timeoutMS is the head's requested limit, with the default for an unset one.
func (g BdGateway) timeoutMS() int {
	if g.TimeoutMS > 0 {
		return g.TimeoutMS
	}
	return gatewayDefaultTimeoutMS
}

// clientTimeout bounds one call end to end: the caller's limit plus the probe
// bound, so a gateway that answers but never finishes cannot hang the caller.
func (g BdGateway) clientTimeout() time.Duration {
	return time.Duration(g.timeoutMS())*time.Millisecond + probeTimeout
}

// doer is the transport: the injected one, or a client bounded by limit.
func (g BdGateway) doer(limit time.Duration) HTTPDoer {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: limit}
}
