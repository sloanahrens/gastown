package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// Verdict values om writes.
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request_changes"
	// VerdictSkipped is recorded when review is disabled for the landing
	// worker (patrols.landing_worker.review=false). Land treats it as a pass.
	VerdictSkipped = "skipped"
	// VerdictErrorPrefix opens the recorded verdict of a landing whose om
	// review could not run and that landed anyway (Lander.ReviewErrorLands).
	VerdictErrorPrefix = "error:"
	// VerdictOverseerPrefix opens the recorded verdict of a landing whose
	// head the overseer reviewed in place of om (gt-g8t3m), followed by the
	// short head sha the review covered.
	VerdictOverseerPrefix = "overseer:"
)

// OMStatusContext is the required commit status a cut-over rig's landing posts
// om's verdict as, beside CI's gate context. main's branch protection requires
// both, so the merge the worker asks for is the review it posted (design, "om
// review and the om / review status").
const OMStatusContext = "om / review"

// omStatusDescriptionMax bounds the description a status carries: the field is
// a short line for the branch-protection UI, not the verdict's reasoning.
const omStatusDescriptionMax = 140

// OMVerdictStatus renders verdict as the commit status the landing posts on
// the candidate. Every verdict a merge path can carry is a success, because
// the status is what satisfies branch protection and the description keeps the
// audit trail of why the merge was allowed: an approve and a skipped review
// are clean, an overseer review stood in for om, and an om that could not run
// lands with its reason recorded as the waiver it is (design: "The overseer
// waiver ... becomes a success status with the waiver recorded"). A request
// for changes maps to a failure, though the landing rejects on it before any
// PR exists, so the merge path never posts that arm.
func OMVerdictStatus(v Verdict) forgejo.StatusRequest {
	req := forgejo.StatusRequest{Context: OMStatusContext}
	switch {
	case v.Verdict == VerdictRequestChanges:
		req.State = forgejo.StateFailure
		req.Description = elideMiddle(fmt.Sprintf("request_changes (score %.2f)", v.Score), omStatusDescriptionMax)
	case strings.HasPrefix(v.Verdict, VerdictOverseerPrefix):
		req.State = forgejo.StateSuccess
		req.Description = elideMiddle("overseer-reviewed waiver "+strings.TrimPrefix(v.Verdict, VerdictOverseerPrefix), omStatusDescriptionMax)
	case strings.HasPrefix(v.Verdict, VerdictErrorPrefix):
		req.State = forgejo.StateSuccess
		req.Description = elideMiddle("om did not run: "+strings.TrimPrefix(v.Verdict, VerdictErrorPrefix), omStatusDescriptionMax)
	case v.Verdict == VerdictSkipped:
		req.State = forgejo.StateSuccess
		req.Description = "om review disabled"
	default:
		req.State = forgejo.StateSuccess
		req.Description = elideMiddle(fmt.Sprintf("approved (score %.2f)", v.Score), omStatusDescriptionMax)
	}
	return req
}

// Verdict is om's decision on one range.
type Verdict struct {
	Verdict  string    `json:"verdict"`
	Score    float64   `json:"score"`
	Summary  string    `json:"summary,omitempty"`
	Findings []Finding `json:"findings"`
}

// Approved reports whether the verdict lets the work land.
func (v Verdict) Approved() bool { return v.Verdict == VerdictApprove }

// Finding is one om finding, in om's own JSON shape.
type Finding struct {
	ID       string `json:"id,omitempty"`
	Severity string `json:"severity,omitempty"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Reviewer reviews the diff base..head in the tree at dir. An error means no
// verdict exists; it is never read as approval.
type Reviewer interface {
	Review(ctx context.Context, dir, base, head string) (Verdict, error)
}

// SkipReviewer reviews nothing and records VerdictSkipped.
type SkipReviewer struct{}

// Review returns the skipped verdict.
func (SkipReviewer) Review(context.Context, string, string, string) (Verdict, error) {
	return Verdict{Verdict: VerdictSkipped}, nil
}

// DefaultOMTimeout bounds one om attempt when OMReviewer.Timeout is zero. om's
// own .om.json timeout (900s for gastown) is per backend call; this bounds the
// attempt so a hung om never holds a landing. A normal review takes 2-4 min; a
// model request the provider queues and never answers (DeepSeek under load
// sends keep-alives for up to 10 min) took 13 min under the old 20m bound and
// held the serial queue the whole time (gt-b4w3y).
const DefaultOMTimeout = 5 * time.Minute

// omUnsetEnv is removed from om's environment. om drops CLAUDE_CONFIG_DIR
// itself, and a value inherited from the caller makes its backend probe
// report a false "Not logged in".
var omUnsetEnv = []string{"CLAUDE_CONFIG_DIR"}

// OMThreshold reads "threshold" from the .om.json in dir (the tree under
// review). It returns 0 when the file or the key is absent.
func OMThreshold(dir string) (float64, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".om.json")) //nolint:gosec // G304: the reviewed tree's own config
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading .om.json: %w", err)
	}
	var cfg struct {
		Threshold *float64 `json:"threshold"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return 0, fmt.Errorf("parsing .om.json: %w", err)
	}
	if cfg.Threshold == nil {
		return 0, nil
	}
	return *cfg.Threshold, nil
}

// ErrOMTimeout is an om attempt that outlived its timeout.
var ErrOMTimeout = errors.New("om review timed out")

// ErrOMExecution is an om run that exited with neither verdict om can write (0
// approve, 1 request_changes): it never judged the diff. Land reports the
// merged tree's size alongside it, the one cause a reader can act on (gt-hhid7).
var ErrOMExecution = errors.New("om review execution error")

// OMReviewer runs `om review -C <dir> --base <base> --head <head> --out <file>`.
// om exits 0 for approve, 1 for request_changes and 2 for an execution error.
// The exit code decides; the verdict file must agree with it and supplies the
// score and findings.
type OMReviewer struct {
	// Path is the om binary; "" means "om" on PATH.
	Path string
	// OutDir holds verdict files. It is created 0700; never a fixed /tmp name.
	OutDir string
	// Out, when set, receives om's own output.
	Out io.Writer
	// Timeout bounds each attempt; 0 means DefaultOMTimeout.
	Timeout time.Duration

	run runFunc // nil means realRun
}

// Review runs om once and returns its verdict. A run that produces none
// (timeout, execution error, missing or malformed verdict) is not retried here:
// it fails, and Land decides (gt-is0ep). Land retries an execution error once
// on a tree under the size bound (gt-q241r); every other failure goes to a
// human who reviews instead.
func (r OMReviewer) Review(ctx context.Context, dir, base, head string) (Verdict, error) {
	return r.reviewOnce(ctx, dir, base, head)
}

// reviewOnce runs om once, bounded by Timeout, and returns its verdict.
func (r OMReviewer) reviewOnce(ctx context.Context, dir, base, head string) (Verdict, error) {
	path := r.Path
	if path == "" {
		path = "om"
	}
	run := r.run
	if run == nil {
		run = realRun
	}
	if r.OutDir == "" {
		return Verdict{}, fmt.Errorf("om review: no verdict directory configured")
	}
	if err := os.MkdirAll(r.OutDir, 0o700); err != nil {
		return Verdict{}, fmt.Errorf("om review: creating verdict dir: %w", err)
	}
	f, err := os.CreateTemp(r.OutDir, "om-verdict-*.json")
	if err != nil {
		return Verdict{}, fmt.Errorf("om review: creating verdict file: %w", err)
	}
	outPath := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(outPath) }()

	var buf bytes.Buffer
	var w io.Writer = &buf
	if r.Out != nil {
		w = io.MultiWriter(&buf, r.Out)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultOMTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := []string{path, "review", "-C", dir, "--base", base, "--head", head, "--out", outPath}
	code, runErr := run(rctx, dir, omUnsetEnv, argv, w)
	if runErr != nil && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return Verdict{}, fmt.Errorf("%w after %s", ErrOMTimeout, timeout)
	}
	if runErr != nil {
		return Verdict{}, fmt.Errorf("om review did not run: %w", runErr)
	}
	if code != 0 && code != 1 {
		return Verdict{}, fmt.Errorf("%w: exited %d: %s", ErrOMExecution, code, strings.TrimSpace(lastLines(buf.String(), 10)))
	}
	data, err := os.ReadFile(outPath) //nolint:gosec // G304: path is the temp file created above
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return Verdict{}, fmt.Errorf("om review exited %d but wrote no verdict", code)
	}
	var v Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return Verdict{}, fmt.Errorf("om review verdict is not JSON: %w", err)
	}
	want := VerdictApprove
	if code == 1 {
		want = VerdictRequestChanges
	}
	if v.Verdict != want {
		return Verdict{}, fmt.Errorf("om review exited %d but its verdict file says %q", code, v.Verdict)
	}
	// The rig's own threshold decides too: an approve scored below it is a
	// request for changes, whatever om's exit code said.
	threshold, err := OMThreshold(dir)
	if err != nil {
		return Verdict{}, fmt.Errorf("om review: %w", err)
	}
	if v.Verdict == VerdictApprove && v.Score < threshold {
		v.Verdict = VerdictRequestChanges
		v.Summary = strings.TrimSpace(fmt.Sprintf("score %.2f is below the rig's .om.json threshold %.2f. %s", v.Score, threshold, v.Summary))
	}
	return v, nil
}
