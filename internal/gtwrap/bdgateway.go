package gtwrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// gatewayRetryTotal is how long a call keeps waiting for the gateway after a
	// refused dial, so a call made while the gateway rolls reaches it once it is
	// back instead of failing on the first refusal.
	gatewayRetryTotal = 45 * time.Second
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
	// Stdin is the caller's stdin, streamed after the head line only when the
	// call reads stdin, and never otherwise. Nil sends none.
	Stdin io.Reader
	// Stdout and Stderr receive bd's own streams. Nil refuses the call, so the
	// tool's output is never dropped.
	Stdout io.Writer
	Stderr io.Writer
	// Note receives the explanation when the working directory is outside the
	// town and runs in the town root instead. Nil refuses the call, so a mapped
	// directory is never silent.
	Note io.Writer
	// Stat checks the directory a call's leading -C names, and the .beads entries
	// a call without BEADS_DIR walks. Nil uses os.Stat. A .beads entry that cannot
	// be read refuses the call, so an unreadable candidate is never read as absent
	// and routed to another rig; a -C directory that cannot be read is refused
	// too.
	Stat func(string) (fs.FileInfo, error)
	// ReadFile reads the redirect file a walked .beads directory may hold. Nil
	// uses os.ReadFile. A redirect that exists but cannot be read refuses the
	// call, since read as absent it would name the worktree's own .beads.
	ReadFile func(string) ([]byte, error)
	// Sleep waits between attempts after a refused dial. Nil uses time.Sleep, so
	// a unit test records the waits instead of spending them.
	Sleep func(time.Duration)
	// Now reads the clock the time spent retrying is measured with. Nil uses
	// time.Now.
	Now func() time.Time
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
	if err := g.settleDirectory(&call); err != nil {
		return exitDown, err
	}
	if err := g.settleBeadsDir(&call); err != nil {
		return exitDown, err
	}
	rig, err := beadRig(g.Town, call.Env, call.Argv)
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
	// The head holds only strings, a slice and a map, so marshaling it cannot
	// fail; a branch for an error that cannot happen would be dead.
	head, _ := json.Marshal(bdgateway.Request{
		Argv:      call.Argv[1:],
		Env:       env,
		Rig:       rig,
		Cwd:       dirRelative(g.Town, call.Dir, g.Note),
		TimeoutMS: g.timeoutMS(),
	})
	// The body after the head is the caller's stdin only when the call reads it: a
	// caller whose stdin is a pipe that never closes would otherwise hold the
	// request open until the gateway's read deadline (gt-vojyy8). A stdin the call
	// itself carries is always sent, since the caller that supplied it means bd to
	// read it.
	var stdin io.Reader
	sendsStdin := false
	switch {
	case call.Stdin != nil:
		stdin, sendsStdin = call.Stdin, true
	case g.Stdin != nil && bdReadsStdin(call.Argv[1:]):
		stdin, sendsStdin = g.Stdin, true
	default:
		stdin = strings.NewReader("")
	}
	// The head is marshaled once, so every attempt sends the same bytes, and a
	// fresh request is built for each: the request whose dial failed was closed by
	// the client, so it cannot be sent again.
	headLine := append(head, '\n')
	newRequest := func() *http.Request {
		body := io.MultiReader(bytes.NewReader(headLine), stdin)
		// validate parsed the URL and the method is fixed, so building the request
		// cannot fail.
		req, _ := http.NewRequest(http.MethodPost, g.endpoint(gatewayExecPath), body)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", gatewayContentType)
		req.Header.Set("X-Gt-Caller", g.caller())
		return req
	}
	// A gateway roll restarts the gateway, so a dial can be refused for a moment
	// while a call is on its way. A refused dial never reached the gateway, so the
	// call cannot have run: it is tried again on RetrySchedule's waits. A read
	// call whose transport failed mid-call may have reached the gateway, so it is
	// sent again on the same waits when RetryableRead allows it: it sent no stdin
	// and wrote no output, so a second run repeats nothing. Both kinds draw on one
	// budget. A refused attempt never read the body, so the caller's stdin is still
	// whole for the attempt that connects, and it is delivered once. A mid-call
	// failure of a write call, or of a read call that sent stdin, is not retried;
	// nor is a non-retryable error or an answer, whatever its status.
	delays := RetrySchedule(gatewayRetryTotal)
	doer := g.doer(g.clientTimeout())
	started := g.now()
	attempts := 0
	var resp *http.Response
	for {
		attempts++
		resp, err = doer.Do(newRequest())
		// wroteOutput is false: the answer's frames are read only after Do returns,
		// so a Do that failed has written nothing to the caller's streams.
		if err == nil || attempts > len(delays) ||
			!(RetryableDial(err) || RetryableRead(err, call.Argv[1:], sendsStdin, false)) {
			break
		}
		g.sleep(delays[attempts-1])
	}
	if err != nil {
		if attempts > 1 {
			return exitDown, fmt.Errorf("gtwrap: the bd gateway at %s did not answer, so the call did not run: %w (after %d attempts in %v)", g.URL, err, attempts, g.now().Sub(started))
		}
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
			// ReadFrame validated the payload, so decoding it cannot fail.
			payload, _ := frame.GatewayError()
			refusal = fmt.Errorf("gtwrap: the bd gateway refused the call: %s: %s", payload.Code, payload.Message)
		case bdgateway.FrameExit:
			// ReadFrame validated the exit frame's payload the same way.
			status, _ := frame.ExitStatus()
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
	// validate parsed the URL, so building the readiness request cannot fail.
	req, _ := http.NewRequest(http.MethodGet, g.endpoint(gatewayReadyPath), nil)
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

// settleDirectory turns a call that begins with bd's root flag "-C <dir>" into
// the call run in that directory. The gateway takes the working directory from
// the call, not from bd's flag, so the flag becomes call.Dir. A relative
// directory resolves against the call's working directory, or against the town
// root when the call has none. The directory must exist and lie inside the town
// once symbolic links are resolved, so a link cannot send the call outside it.
// An empty name, a path outside the town, a missing directory, one that is not
// a directory, and one that cannot be read are each refused before the call is
// sent; the refusal names the path, or says the flag named none. An explicit
// BEADS_DIR still decides the rig.
//
// Only the leading pair is read. A "-C" anywhere else, and the attached forms
// "-C<dir>" and "-C=<dir>", are left for the gateway's flag refusal.
func (g BdGateway) settleDirectory(call *Call) error {
	if len(call.Argv) < 3 || call.Argv[1] != "-C" {
		return nil
	}
	named := call.Argv[2]
	if named == "" {
		return errors.New("gtwrap: the bd call's leading -C names no directory, so the call is refused")
	}
	dir := named
	if !filepath.IsAbs(dir) {
		base := call.Dir
		if base == "" {
			base = g.Town
		}
		dir = filepath.Join(base, dir)
	}
	dir = filepath.Clean(dir)
	described := named
	if described != dir {
		described = fmt.Sprintf("%s (resolved to %s)", named, dir)
	}
	rel, err := filepath.Rel(g.Town, dir)
	if err != nil {
		return fmt.Errorf("gtwrap: compare the bd call's -C directory %s with the town %s, so the call cannot be told which rig it runs in: %w", described, g.Town, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("gtwrap: the bd call's -C directory %s is outside the town %s, so it cannot be the call's working directory", described, g.Town)
	}
	info, err := statDir(g.Stat)(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("gtwrap: the bd call's -C directory %s does not exist, so it cannot be the call's working directory", described)
	case err != nil:
		return fmt.Errorf("gtwrap: read the bd call's -C directory %s: %w", described, err)
	case info == nil || !info.IsDir():
		return fmt.Errorf("gtwrap: the bd call's -C directory %s is not a directory, so it cannot be the call's working directory", described)
	}
	// The lexical check above cannot see a symbolic link: a link inside the town
	// may point outside it, and the gateway would run the call there. The
	// containment is checked again on the resolved paths. A failed resolution
	// refuses too, since an unresolved path cannot be shown to be inside.
	realTown, err := filepath.EvalSymlinks(g.Town)
	if err != nil {
		return fmt.Errorf("gtwrap: resolve the town %s, so the bd call's -C directory %s cannot be checked against it: %w", g.Town, described, err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("gtwrap: resolve the bd call's -C directory %s: %w", described, err)
	}
	realRel, err := filepath.Rel(realTown, realDir)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("gtwrap: the bd call's -C directory %s is outside the town %s, so it cannot be the call's working directory", described, g.Town)
	}
	call.Argv = append([]string{call.Argv[0]}, call.Argv[3:]...)
	call.Dir = dir
	return nil
}

// settleBeadsDir gives a bd call that sets no BEADS_DIR the .beads directory
// nearest at or above its working directory inside the town, the walk bd itself
// makes and the Mac wrapper already makes before the pod runs the call
// (gt-6ac6li). The gateway needs the variable to tell which rig the call runs
// in. A call that reads no database (bd's version command and its root help
// probe) needs none, and an explicit BEADS_DIR is used unchanged. A working
// directory outside the town, or one with no .beads above it up to the town
// root, is left alone, so beadRig's refusal stands.
func (g BdGateway) settleBeadsDir(call *Call) error {
	if readsNoDatabase(call.Argv[1:]) {
		return nil
	}
	if _, ok := envValue(call.Env, beadsDirVar); ok {
		return nil
	}
	// A directory that is not absolute cannot be compared with the town, so it is
	// left alone and beadRig's refusal stands, as it does for one outside the town.
	if !filepath.IsAbs(call.Dir) {
		return nil
	}
	rel, _, err := nearestBeadsRel(g.Town, call.Dir, statDir(g.Stat), readFileOf(g.ReadFile))
	if err != nil || rel == "" {
		return err
	}
	call.Env = append(call.Env, beadsDirVar+"="+filepath.Join(g.Town, rel))
	return nil
}

// beadRig names the rig the bd gateway builds BEADS_DIR from: the caller's
// absolute BEADS_DIR relative to the town root with its ".beads" component
// removed, which is "" for the town's own beads. The absolute path never crosses;
// the gateway joins the rig onto its own workspace. A BEADS_DIR the workspace
// cannot hold is refused rather than sent, so the gateway never runs in the wrong
// database. A call that sets no BEADS_DIR gets the town workspace ("") when it
// reads no database (bd's version command or its root help probe). The gateway
// settles a missing BEADS_DIR to the nearest .beads above the call's directory
// before this runs (settleBeadsDir), so a call that still has none here has no
// .beads at or above a working directory inside the town, and is refused, since
// the gateway cannot otherwise be told which rig it runs in. argv is the call's
// argv, whose first word the caller has checked is "bd". The bdgateway and
// bddocker targets share the naming, so the two cannot name a rig differently;
// bddocker settles nothing, so it refuses a call that reads a database and sets
// no BEADS_DIR.
func beadRig(town string, env, argv []string) (string, error) {
	beadsDir, ok := envValue(env, "BEADS_DIR")
	if !ok || beadsDir == "" {
		if readsNoDatabase(argv[1:]) {
			return "", nil
		}
		return "", errors.New("gtwrap: the call sets no BEADS_DIR, so the bd gateway cannot be told which rig it runs in; set BEADS_DIR to the town's or a rig's .beads directory")
	}
	if !filepath.IsAbs(beadsDir) {
		return "", fmt.Errorf("gtwrap: BEADS_DIR is %q, which is not an absolute path, so the bd gateway cannot place it in its workspace", beadsDir)
	}
	rel, err := filepath.Rel(town, beadsDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gtwrap: BEADS_DIR %q is outside the town %s, so the bd gateway cannot place it in its workspace", beadsDir, town)
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

// readsNoDatabase reports whether a bd call reads no database, so it may run in
// the town workspace when the caller sets no BEADS_DIR: bd's version command and
// its root help probe. words is the call's argv after "bd". The match is exact, so
// a version or help request carrying any other word, which may reach a database,
// is not one of these calls.
func readsNoDatabase(words []string) bool {
	if slices.Equal(words, []string{"version"}) || slices.Equal(words, []string{"version", "--json"}) {
		return true
	}
	return len(words) == 1 && (words[0] == "--help" || words[0] == "help")
}

// bdReadsStdin reports whether a bd call reads the caller's stdin, so its bytes
// may follow the head line. bd takes stdin as a "-" value of a file flag: the
// four flags in bdStdinFlags under any command, and -f/--file under a command
// whose -f reads stdin (bdFileCommands), either as the next token ("bd update
// gt-x --body-file -", "bd comments add gt-x -f -") or attached ("--body-file=-",
// "-f=-", "-f-"), and through its --stdin flag. A file the wrapper carries
// reaches bd as one of those "-" forms, since filearg.go rewrites the flag's
// value. words is the call's argv after "bd", and nothing after "--" is a flag.
//
// A "-" value of any other flag, and a positional "-", read no stdin: sending
// one would hold the call to the gateway's read deadline for a caller whose
// stdin is a pipe that never closes (gt-vojyy8).
func bdReadsStdin(words []string) bool {
	command, sub := commandWords(words)
	// readsStdin reports whether flag's "-" value makes bd read stdin.
	readsStdin := func(flag string) bool {
		return classifyBd(command, sub, flag, "-") == argStdin
	}
	for i, word := range words {
		switch {
		case word == "--":
			return false
		case word == "--stdin":
			return true
		case strings.HasPrefix(word, "--"):
			flag, value, attached := strings.Cut(word, "=")
			if attached && value == "-" && readsStdin(flag) {
				return true
			}
		case len(word) > 2 && shorthandGroup(word):
			// An attached shorthand value: -f- or -f=-. A group such as -xf-
			// is refused by ScanFileArgs, so only the flag's own letter is read.
			flag, value := word[:2], word[2:]
			if rest, ok := strings.CutPrefix(value, "="); ok {
				value = rest
			}
			if value == "-" && readsStdin(flag) {
				return true
			}
		case word == "-":
			// The separate value form: the token before it is the flag.
			if i > 0 && readsStdin(words[i-1]) {
				return true
			}
		}
	}
	return false
}

// dirRelative maps the caller's working directory onto the bd gateway's
// workspace, which mirrors the town. A directory under the town keeps its path
// relative to it; any other directory runs in the town root, and the note says
// so, since the protocol has no way to name an absolute path. The bdgateway and
// bddocker targets share it.
func dirRelative(town, dir string, note io.Writer) string {
	if dir == "" {
		return "."
	}
	rel, err := filepath.Rel(town, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fmt.Fprintf(note, "gt-wrap: %s is outside the town %s, which the bd gateway's workspace does not mirror, so bd runs in the town root\n", dir, town)
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

// sleep waits d between attempts: the injected sleep, or the real one.
func (g BdGateway) sleep(d time.Duration) {
	if g.Sleep != nil {
		g.Sleep(d)
		return
	}
	time.Sleep(d)
}

// now reads the clock: the injected one, or the real one.
func (g BdGateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}
