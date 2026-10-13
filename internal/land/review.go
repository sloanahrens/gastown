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
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

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
	// VerdictUnrecorded is recorded for a landing this lander merged but never
	// wrote a record for: the merge on the target is the evidence, and the
	// verdict om reached was not kept (gt-mdet3).
	VerdictUnrecorded = "unrecorded"
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
	Verdict string  `json:"verdict"`
	Score   float64 `json:"score"`
	Summary string  `json:"summary,omitempty"`
	// Bead is the work bead om echoed back when it was told which bead the
	// diff belongs to (OMReviewer.ReviewBead). It is read for the mismatch
	// check and stored nowhere (gt-pwvv4).
	Bead     string    `json:"bead,omitempty"`
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

// ReviewInput is one review that names the work bead the diff belongs to.
type ReviewInput struct {
	// Dir is the tree under review; Base and Head are the range reviewed in it.
	Dir, Base, Head string
	// BeadID is the work bead the landed diff belongs to.
	BeadID string
	// CriteriaText is the work bead's acceptance text (CriteriaText), which om
	// is given through --criteria-file when it accepts the flag. Empty means
	// the bead carries none and the flag is omitted (gt-yl7n4).
	CriteriaText string
	// Logf receives the reviewer's warning lines; nil drops them.
	Logf func(format string, args ...any)
}

// BeadReviewer is a Reviewer that can be told which work bead the diff belongs
// to, so om can echo the id back in its verdict. Land calls it when its
// Reviewer implements it and Reviewer.Review otherwise, so SkipReviewer and
// test fakes need no change (gt-pwvv4).
type BeadReviewer interface {
	ReviewBead(ctx context.Context, in ReviewInput) (Verdict, error)
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
//
// ReviewBead adds `--bead <id>` and `--criteria-file <path>` when the
// installed om accepts them (see omReviewHelp), so om knows which bead it is
// reviewing and what that bead asked for.
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
	// criteriaWriter writes the acceptance text and returns its path; nil
	// means writeCriteria. The verdict directory's permissions cannot fail this
	// write and not the verdict file's, so this is the seam a test reaching the
	// fail-soft path uses (gt-yl7n4).
	criteriaWriter func(in ReviewInput) (string, error)
}

// Review runs om once and returns its verdict. A run that produces none
// (timeout, execution error, missing or malformed verdict) is not retried here:
// it fails, and Land decides (gt-is0ep). Land retries an execution error once
// on a tree under the size bound (gt-q241r); every other failure goes to a
// human who reviews instead.
func (r OMReviewer) Review(ctx context.Context, dir, base, head string) (Verdict, error) {
	v, _, err := r.reviewOnce(ctx, ReviewInput{Dir: dir, Base: base, Head: head})
	return v, err
}

// ReviewBead runs om once with the work bead named, and checks the id om echoes
// back. A verdict naming another bead, or none at all, is logged and otherwise
// ignored: the landing's bead is the one the queue dispatched, and a reviewer
// that doubts it does not decide a landing (gt-pwvv4).
//
// A bead carrying acceptance text has it written to a temp file in OutDir and
// named on the argv as --criteria-file, so om reads the criteria the bead was
// dispatched with rather than inferring them (gt-yl7n4).
func (r OMReviewer) ReviewBead(ctx context.Context, in ReviewInput) (Verdict, error) {
	v, told, err := r.reviewOnce(ctx, in)
	if err != nil {
		return v, err
	}
	switch {
	case !told:
	case v.Bead == "":
		r.warnf(in, "om did not echo the work bead though it was passed --bead %s; the verdict stands", in.BeadID)
	case v.Bead != in.BeadID:
		r.warnf(in, "WARNING om's verdict names bead %s but the landing is %s; the verdict stands", v.Bead, in.BeadID)
	}
	return v, nil
}

// reviewOnce runs om once, bounded by Timeout, and returns its verdict and
// whether om was told which bead it reviewed.
func (r OMReviewer) reviewOnce(ctx context.Context, in ReviewInput) (Verdict, bool, error) {
	dir, base, head := in.Dir, in.Base, in.Head
	path := r.Path
	if path == "" {
		path = "om"
	}
	run := r.run
	if run == nil {
		run = realRun
	}
	if r.OutDir == "" {
		return Verdict{}, false, fmt.Errorf("om review: no verdict directory configured")
	}
	if err := os.MkdirAll(r.OutDir, 0o700); err != nil {
		return Verdict{}, false, fmt.Errorf("om review: creating verdict dir: %w", err)
	}
	f, err := os.CreateTemp(r.OutDir, "om-verdict-*.json")
	if err != nil {
		return Verdict{}, false, fmt.Errorf("om review: creating verdict file: %w", err)
	}
	outPath := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(outPath) }()

	var buf bytes.Buffer
	var w io.Writer = &buf
	if r.Out != nil {
		w = io.MultiWriter(&buf, r.Out)
	}
	// The probe is bounded by its own timeout, before the attempt clock starts:
	// an om that cannot describe its own flags must not spend the review's.
	// One probe answers every flag question this review has (gt-yl7n4).
	var help string
	var probed bool
	if in.BeadID != "" || in.CriteriaText != "" {
		help, probed = r.omReviewHelp(ctx, in, path, run)
	}
	told := probed && in.BeadID != "" && omHelpListsFlag(help, "--bead")
	criteriaPath := ""
	if probed && in.CriteriaText != "" && omHelpListsFlag(help, "--criteria-file") {
		write := r.criteriaWriter
		if write == nil {
			write = r.writeCriteria
		}
		p, err := write(in)
		if err != nil {
			// The review is worth running without the criteria; the bead's
			// acceptance text is a help to om, not a precondition.
			r.warnf(in, "could not write the acceptance criteria file (%v); om runs without --criteria-file", err)
		} else {
			criteriaPath = p
			defer func() { _ = os.Remove(p) }()
		}
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultOMTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := []string{path, "review", "-C", dir, "--base", base, "--head", head, "--out", outPath}
	if told {
		argv = append(argv, "--bead", in.BeadID)
	}
	if criteriaPath != "" {
		argv = append(argv, "--criteria-file", criteriaPath)
	}
	code, runErr := run(rctx, dir, omUnsetEnv, argv, w)
	if runErr != nil && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return Verdict{}, told, fmt.Errorf("%w after %s", ErrOMTimeout, timeout)
	}
	if runErr != nil {
		return Verdict{}, told, fmt.Errorf("om review did not run: %w", runErr)
	}
	if code != 0 && code != 1 {
		return Verdict{}, told, fmt.Errorf("%w: exited %d: %s", ErrOMExecution, code, strings.TrimSpace(lastLines(buf.String(), 10)))
	}
	data, err := os.ReadFile(outPath) //nolint:gosec // G304: path is the temp file created above
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return Verdict{}, told, fmt.Errorf("om review exited %d but wrote no verdict", code)
	}
	var v Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return Verdict{}, told, fmt.Errorf("om review verdict is not JSON: %w", err)
	}
	want := VerdictApprove
	if code == 1 {
		want = VerdictRequestChanges
	}
	if v.Verdict != want {
		return Verdict{}, told, fmt.Errorf("om review exited %d but its verdict file says %q", code, v.Verdict)
	}
	// The rig's own threshold decides too: an approve scored below it is a
	// request for changes, whatever om's exit code said.
	threshold, err := OMThreshold(dir)
	if err != nil {
		return Verdict{}, told, fmt.Errorf("om review: %w", err)
	}
	if v.Verdict == VerdictApprove && v.Score < threshold {
		v.Verdict = VerdictRequestChanges
		v.Summary = strings.TrimSpace(fmt.Sprintf("score %.2f is below the rig's .om.json threshold %.2f. %s", v.Score, threshold, v.Summary))
	}
	return v, told, nil
}

// omHelpTimeout bounds one `om review --help` probe: om that cannot answer a
// help request in ten seconds is treated as one without the flag.
const omHelpTimeout = 10 * time.Second

// omReviewHelp returns the help text of `om review --help` for the binary at
// path, read once per binary identity per process, and whether the probe
// produced any help. One read answers every flag question a review has, so a
// second flag costs no second probe (gt-yl7n4).
//
// A failed probe — a run error, a timeout, or an exit with no help text —
// reports no help and is retried on the next review rather than cached, so a
// transient failure never becomes "this om has no such flag".
//
// om exits 2 for --help and prints its usage to stderr, so the exit code alone
// decides nothing; only help text listing a flag counts.
func (r OMReviewer) omReviewHelp(ctx context.Context, in ReviewInput, path string, run runFunc) (string, bool) {
	id, identifiable := omBinaryIdentity(path)
	if identifiable {
		// The lock is held for the probe itself, so landings that ask at the
		// same moment share one probe rather than racing two.
		omHelpProbe.mu.Lock()
		defer omHelpProbe.mu.Unlock()
		if help, cached := omHelpProbe.help[id]; cached {
			return help, true
		}
	}
	var buf bytes.Buffer
	pctx, cancel := context.WithTimeout(ctx, omHelpTimeout)
	defer cancel()
	code, runErr := run(pctx, in.Dir, omUnsetEnv, []string{path, "review", "--help"}, &buf)
	help := buf.String()
	if runErr != nil || strings.TrimSpace(help) == "" {
		r.warnf(in, "could not probe %s for the flags this review needs (%s); om runs without them", path, probeFailure(runErr, pctx, code))
		return "", false
	}
	if identifiable {
		omHelpProbe.help[id] = help
	}
	return help, true
}

// writeCriteria writes the review's acceptance text to a 0600 temp file in
// OutDir, beside the verdict file, and returns its path. The caller removes it
// after the run; a text that cannot be written is not a reason to skip the
// review.
func (r OMReviewer) writeCriteria(in ReviewInput) (string, error) {
	f, err := os.CreateTemp(r.OutDir, "om-criteria-*.txt")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.WriteString(in.CriteriaText); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// probeFailure names why a help probe reported no help text.
func probeFailure(runErr error, pctx context.Context, code int) string {
	switch {
	case runErr == nil:
		return fmt.Sprintf("exited %d with no help text", code)
	case errors.Is(pctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("timed out after %s", omHelpTimeout)
	default:
		return runErr.Error()
	}
}

// omHelpListsFlag reports whether help lists flag as a word of its own, so
// --bead-id and --beads never read as --bead.
func omHelpListsFlag(help, flag string) bool {
	notFlagRune := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	}
	return slices.ContainsFunc(strings.FieldsFunc(help, notFlagRune), func(f string) bool { return f == flag })
}

// warnf writes one reviewer warning to the landing log Land supplied, falling
// back to this reviewer's own output writer.
func (r OMReviewer) warnf(in ReviewInput, format string, args ...any) {
	line := fmt.Sprintf("om review: "+format, args...)
	switch {
	case in.Logf != nil:
		in.Logf("%s", line)
	case r.Out != nil:
		_, _ = fmt.Fprintln(r.Out, line)
	}
}

// omHelpProbe caches each installed om's `review --help` text: the one read
// both --bead and --criteria-file are answered from.
var omHelpProbe = struct {
	mu   sync.Mutex
	help map[omBinaryID]string
}{help: map[omBinaryID]string{}}

// omBinaryID identifies one installed om the way a reinstall changes it, so an
// om replaced under a running daemon is probed again.
type omBinaryID struct {
	path string
	size int64
	mod  time.Time
}

// omBinaryIdentity identifies the om at path. The second result is false for a
// path that cannot be stat'ed — a bare "om" resolved on PATH, say — whose
// answer can then be used but not cached.
func omBinaryIdentity(path string) (omBinaryID, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return omBinaryID{}, false
	}
	return omBinaryID{path: path, size: fi.Size(), mod: fi.ModTime()}, true
}
