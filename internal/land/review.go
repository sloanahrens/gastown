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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/git"
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
	// Version is the om that reached the verdict, read from `om version` and
	// never from om's verdict file. A landing records it on the work bead
	// beside the verdict, so a bead names the reviewer that judged it
	// (gt-uebmq).
	Version string `json:"-"`
	// Rescore is set when a borderline rejection was re-scored, and nil on
	// every verdict om reached in one run (gt-cz07b). It is the landing's own
	// record, not a field om wrote.
	Rescore *Rescore `json:"-"`
}

// Rescore is the two om scores of a borderline rejection that was re-scored
// once: the score the landing first stood on and the score the rerun reached
// (gt-cz07b).
//
// It rides the verdict the landing decides on, which is the higher-scoring of
// the two, so the verdict comment names both and a reader can see the review
// ran twice. An approval leaves no rejection note, so the rejected run's score
// has no other record.
type Rescore struct {
	First float64
	Rerun float64
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
	// Detail is om's longer account of the finding (gt-q52sv).
	Detail string `json:"detail,omitempty"`
}

// The severities a Finding's Severity carries, as the review reads them. A
// severity outside this list blocks nothing and counts as nothing (gt-oi4je).
const (
	omSeverityCritical = "critical"
	omSeverityMajor    = "major"
	omSeverityMinor    = "minor"
)

// omSeverity is a finding's severity as every reader of it compares the field:
// om pads it and shifts its case, and readers that disagreed about that would
// disagree about which findings refuse a landing.
func omSeverity(severity string) string {
	return strings.ToLower(strings.TrimSpace(severity))
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
	// Repo is the landing's repository, which holds Base. The review bar is the
	// threshold in the .om.json at Base, read through it (OMThresholdAt), never
	// from the tree under review: a diff that edits .om.json cannot set the bar
	// it is judged against (gt-uhuw9). Nil fails the review closed.
	Repo Repo
	// BeadID is the work bead the landed diff belongs to.
	BeadID string
	// CriteriaText is the work bead's acceptance text (CriteriaText), which om
	// is given through --criteria-file when it accepts the flag. Empty means
	// the bead carries none and the flag is omitted (gt-yl7n4).
	CriteriaText string
	// AcceptedRisksText is the accepted-risk record the bead's comments carry
	// (AcceptedRisksText), which om is given through --context-file when it
	// accepts the flag: a finding that only restates one is info, so the review
	// stops re-raising what an overseer or a human already decided. Empty means
	// the bead records none and the flag is omitted (gt-lauly).
	AcceptedRisksText string
	// PriorFindings are the findings of the previous round's refusal, which om
	// is given through --prior-findings so it classifies each as resolved,
	// unresolved or regressed instead of raising a fresh set of minors on a
	// diff that has not changed since the refusal (gt-cz07b). Empty means this
	// is the landing's first review, or the round before it refused on
	// something other than findings.
	PriorFindings []Finding
	// BriefText is the work-item file (BeadBrief), which om reads through
	// --bead-file when it accepts the flag (gt-3l5qy). Empty means the flag is
	// omitted.
	BriefText string
	// PolicyText is the fixed town-policy block (policy.PolicyBlock), which om
	// reads through --policy-file when it accepts the flag (gt-3l5qy). Empty
	// means the flag is omitted.
	PolicyText string
	// Round is the landing attempt this review belongs to (reviewAttempt), passed
	// as --round when the installed om accepts it. Zero means no round.
	Round int
	// RoundsText is the rounds ledger as the JSON array om reads through
	// --rounds-file when it accepts the flag (gt-3l5qy). Empty means the flag is
	// omitted: no history, or a ledger that could not be read.
	RoundsText string
	// Logf receives the reviewer's warning lines; nil drops them.
	Logf func(format string, args ...any)

	// omVer is the om ReviewBead read before the run, for the verdict store's
	// record (gt-uxqb9).
	omVer string
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

// omConfigFile is the om config a rig keeps at the root of its tree.
const omConfigFile = ".om.json"

// OMThreshold reads "threshold" from the .om.json in dir, the checkout the spec
// dispatcher's overrun band reads. It returns 0 when the file or the key is
// absent. It is not the bar a landing is reviewed against: that is read at the
// base commit (OMThresholdAt), because a diff under review can edit the tree's
// .om.json (gt-uhuw9).
func OMThreshold(dir string) (float64, error) {
	data, err := os.ReadFile(filepath.Join(dir, omConfigFile)) //nolint:gosec // G304: the rig checkout's own config
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading .om.json: %w", err)
	}
	return omThresholdOf(data)
}

// OMThresholdAt reads "threshold" from the .om.json at base, the commit a
// landing is judged against, through g. It returns 0 when the file or the key
// is absent at base. Any other read failure is an error, never 0: an
// unreadable base must not read as no bar (gt-uhuw9).
func OMThresholdAt(g Repo, base string) (float64, error) {
	if g == nil {
		return 0, fmt.Errorf("reading .om.json at %s: no repository to read it from", base)
	}
	text, err := g.ShowFileAtRev(base, omConfigFile)
	if errors.Is(err, git.ErrNotAtRef) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading .om.json at %s: %w", base, err)
	}
	return omThresholdOf([]byte(text))
}

// omThresholdOf parses the threshold out of one .om.json's bytes.
func omThresholdOf(data []byte) (float64, error) {
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
// ReviewBead always names the work bead with `--bead <id>`, so that every
// review om runs carries the id of the work it judged, and adds
// `--criteria-file <path>` when the installed om accepts that flag (see
// omReviewHelp), so om knows what the bead asked for.
type OMReviewer struct {
	// Path is the om binary; "" means "om" on PATH.
	Path string
	// OutDir holds verdict files. It is created 0700; never a fixed /tmp name.
	OutDir string
	// Out, when set, receives om's own output.
	Out io.Writer
	// Timeout bounds each attempt; 0 means DefaultOMTimeout.
	Timeout time.Duration
	// Verdicts, when set, keeps the raw JSON of every verdict the review
	// reaches, where the summary and finding details the bead notes drop
	// survive a landing (gt-uxqb9). Nil means off.
	Verdicts *VerdictStore

	run runFunc // nil means realRun
	// criteriaWriter writes the acceptance text and returns its path; nil
	// means writeCriteria. The verdict directory's permissions cannot fail this
	// write and not the verdict file's, so this is the seam a test reaching the
	// fail-soft path uses (gt-yl7n4).
	criteriaWriter func(in ReviewInput) (string, error)
	// acceptedRisksWriter is the same seam for the context file; nil means
	// writeAcceptedRisks (gt-lauly).
	acceptedRisksWriter func(in ReviewInput) (string, error)
}

// Review runs om once and returns its verdict. A run that produces none
// (timeout, execution error, missing or malformed verdict) is not retried here:
// it fails, and Land decides (gt-is0ep). Land retries an execution error once
// on a tree under the size bound (gt-q241r); every other failure goes to a
// human who reviews instead.
//
// Review carries no repository, so it cannot read the review bar at base and
// fails closed. Land reviews through ReviewBead, which is given one.
func (r OMReviewer) Review(ctx context.Context, dir, base, head string) (Verdict, error) {
	return r.reviewOnce(ctx, ReviewInput{Dir: dir, Base: base, Head: head})
}

// ReviewBead runs om once with the work bead named, and checks the id om echoes
// back. A verdict naming another bead, or none at all, is logged and otherwise
// ignored: the landing's bead is the one the queue dispatched, and a reviewer
// that doubts it does not decide a landing (gt-pwvv4).
//
// Every review names its work bead (gt-r3fg9): `--bead <id>` is what makes om
// put "This change is tracked as bead <id>" in the review prompt, the line
// usage attribution matches to tie a review to the work it judged. It rides the
// argv whatever the installed om's help says, because a review that carries no
// bead is a review nothing can be attributed to.
//
// A bead carrying acceptance text has it written to a temp file in OutDir and
// named on the argv as --criteria-file when the installed om takes the flag, so
// om reads the criteria the bead was dispatched with rather than inferring them
// (gt-yl7n4). A bead recording accepted risks has them written the same way and
// named as --context-file, so a finding that only restates a settled decision
// does not refuse the landing a second time (gt-lauly).
//
// A rework round — one a refusal sent back to the author — carries that
// refusal's findings, written to a temp file in OutDir and named on the argv as
// --prior-findings, so om scores the round on what is still unresolved rather
// than on a fresh set of minors (gt-cz07b).
func (r OMReviewer) ReviewBead(ctx context.Context, in ReviewInput) (Verdict, error) {
	// The verdict names the om behind it, and a review that produced none is
	// exactly when the binary is worth naming, so this is read whatever the
	// review does (gt-uebmq). It is read before the review runs: an install
	// that renames a new om over the path mid-review would otherwise have the
	// verdict name a binary that did not reach it (gt-f3da3).
	version := r.omVersion(ctx, in)
	in.omVer = version
	v, err := r.reviewOnce(ctx, in)
	v.Version = version
	if err != nil {
		return v, err
	}
	switch {
	case in.BeadID == "":
	case v.Bead == "":
		r.warnf(in, "om did not echo the work bead though it was passed --bead %s; the verdict stands", in.BeadID)
	case v.Bead != in.BeadID:
		r.warnf(in, "WARNING om's verdict names bead %s but the landing is %s; the verdict stands", v.Bead, in.BeadID)
	}
	return v, nil
}

// reviewOnce runs om once, bounded by Timeout, and returns its verdict.
func (r OMReviewer) reviewOnce(ctx context.Context, in ReviewInput) (Verdict, error) {
	dir, base, head := in.Dir, in.Base, in.Head
	path, run := r.omInvocation()
	if r.OutDir == "" {
		return Verdict{}, fmt.Errorf("om review: no verdict directory configured")
	}
	// The bar is the base commit's, read before om runs: om is given it on the
	// argv, and the verdict below is held to it (gt-uhuw9). A bar that cannot
	// be read fails the review, not the landing's reading of it as 0.
	bar, err := OMThresholdAt(in.Repo, base)
	if err != nil {
		return Verdict{}, fmt.Errorf("om review: %w", err)
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
	// --criteria-file and --context-file are the flag questions the help probe
	// answers, bounded by its own timeout, before the attempt clock starts: an
	// om that cannot describe its own flags must not spend the review's
	// (gt-yl7n4). One probe answers both, so a bead carrying both texts pays for
	// one help read.
	// The brief, policy and rounds flags (gt-3l5qy) ride the same one probe, and
	// so does --threshold (gt-uhuw9).
	newFlags := in.BriefText != "" || in.PolicyText != "" || in.RoundsText != "" || in.Round > 0
	criteriaPath, contextPath := "", ""
	briefPath, policyPath, roundsPath := "", "", ""
	help, probed := "", false
	if in.CriteriaText != "" || in.AcceptedRisksText != "" || newFlags || bar > 0 {
		help, probed = r.omReviewHelp(ctx, in, path, run)
	}
	if in.CriteriaText != "" {
		criteriaPath = r.optionalFile(in, probed, help, "--criteria-file",
			"the acceptance criteria file", r.criteriaFile)
	}
	if in.AcceptedRisksText != "" {
		contextPath = r.optionalFile(in, probed, help, "--context-file",
			"the accepted risks file", r.acceptedRisksFile)
	}
	if in.BriefText != "" {
		briefPath = r.optionalFile(in, probed, help, "--bead-file",
			"the work-item file", r.briefFile)
	}
	if in.PolicyText != "" {
		policyPath = r.optionalFile(in, probed, help, "--policy-file",
			"the policy file", r.policyFile)
	}
	if in.RoundsText != "" {
		roundsPath = r.optionalFile(in, probed, help, "--rounds-file",
			"the rounds file", r.roundsFile)
	}
	roundFlag := in.Round > 0 && probed && omHelpListsFlag(help, "--round")
	if in.Round > 0 && probed && !roundFlag {
		r.warnf(in, "om does not take --round; review runs without the round number")
	}
	// Every file is a temp file on every path out of the attempt, not only a
	// successful one.
	defer func() {
		for _, p := range []string{criteriaPath, contextPath, briefPath, policyPath, roundsPath} {
			if p != "" {
				_ = os.Remove(p)
			}
		}
	}()
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultOMTimeout
	}
	// The previous round's findings, written before the attempt clock starts
	// like the criteria file beside them: om reads them from a path, and a file
	// that cannot be written costs the round its classification rather than the
	// review.
	priorPath := ""
	if len(in.PriorFindings) > 0 {
		p, err := r.writePriorFindings(in)
		if err != nil {
			r.warnf(in, "could not write the previous round's findings (%v); om reviews this round cold", err)
		} else {
			priorPath = p
			defer func() { _ = os.Remove(p) }()
		}
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := []string{path, "review", "-C", dir, "--base", base, "--head", head, "--out", outPath}
	// The bead is named unconditionally, never through a help probe (gt-r3fg9):
	// a review om runs without the id is one usage attribution cannot match,
	// and an om too old to take the flag answers with an execution error the
	// landing already handles — a loud failure, where a review that silently
	// carried no bead read as a normal one. An om-skew alert names a stale
	// install for the operator.
	if in.BeadID != "" {
		argv = append(argv, "--bead", in.BeadID)
	}
	// om decides against the base commit's bar, not the threshold in the tree
	// it reviews (gt-uhuw9). A base with no bar adds no flag, and an om whose
	// help does not list --threshold gets none: the landing's own check below
	// holds the verdict to the bar either way.
	thresholdFlag := bar > 0 && probed && omHelpListsFlag(help, "--threshold")
	if bar > 0 && probed && !thresholdFlag {
		r.warnf(in, "om does not take --threshold; the landing holds the verdict to the base bar %.2f", bar)
	}
	if thresholdFlag {
		argv = append(argv, "--threshold", strconv.FormatFloat(bar, 'f', -1, 64))
	}
	if criteriaPath != "" {
		argv = append(argv, "--criteria-file", criteriaPath)
	}
	if contextPath != "" {
		argv = append(argv, "--context-file", contextPath)
	}
	// --prior-findings rides the argv whatever the installed om's help says,
	// as --bead does (gt-r3fg9): the classification is what makes a rework
	// round converge, and an om too old for the flag fails loudly where a
	// review that quietly carried no findings would be indistinguishable from
	// a round om had nothing to classify. It rides alone: --prior-head names
	// the last APPROVED review, and the round these findings come from was
	// refused, so naming it as approved would tell om the diff already passed
	// (gt-cz07b).
	if priorPath != "" {
		argv = append(argv, "--prior-findings", priorPath)
	}
	if briefPath != "" {
		argv = append(argv, "--bead-file", briefPath)
	}
	if policyPath != "" {
		argv = append(argv, "--policy-file", policyPath)
	}
	if roundsPath != "" {
		argv = append(argv, "--rounds-file", roundsPath)
	}
	if roundFlag {
		argv = append(argv, "--round", strconv.Itoa(in.Round))
	}
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
	// om's JSON is kept before the threshold below rewrites the verdict: the
	// store records what om said, whatever the landing decides on it.
	r.storeVerdict(in, data, argv)
	// The rig's own threshold at base decides too: an approve scored below it is
	// a request for changes, whatever om's exit code said.
	if v.Verdict == VerdictApprove && v.Score < bar {
		v.Verdict = VerdictRequestChanges
		v.Summary = strings.TrimSpace(fmt.Sprintf("score %.2f is below the rig's .om.json threshold %.2f. %s", v.Score, bar, v.Summary))
	}
	return v, nil
}

// omHelpTimeout bounds one `om review --help` probe: om that cannot answer a
// help request in ten seconds is treated as one without the flag.
const omHelpTimeout = 10 * time.Second

// omReviewHelp returns the help text of `om review --help` for the binary at
// path, read once per binary identity per process, and whether the probe
// produced any help. It answers whether the installed om takes --criteria-file
// and --context-file; --bead is not asked here, because a review names its bead
// whatever the help says (gt-r3fg9), so one read answers both flag questions
// (gt-yl7n4, gt-lauly).
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

// omVersionTimeout bounds one `om version` probe, like the help read's: an om
// that cannot name its version in ten seconds has none to report.
const omVersionTimeout = 10 * time.Second

// omVersionUnknown is the version a verdict carries when the om behind it
// could not be read. It is a value rather than an empty string so the work
// bead's verdict comment always names a version.
const omVersionUnknown = "unknown"

// omVersion returns the version of the om at path, the sha `om version`
// prints, read once per binary identity per process from the same cache the
// help probe fills: both are properties of one installed binary (gt-uebmq).
//
// A probe that fails — a run error, a timeout, an exit with no version line —
// reports omVersionUnknown and is not cached, so a transient failure never
// becomes "this om has no version".
func (r OMReviewer) omVersion(ctx context.Context, in ReviewInput) string {
	path, run := r.omInvocation()
	id, identifiable := omBinaryIdentity(path)
	if identifiable {
		omHelpProbe.mu.Lock()
		defer omHelpProbe.mu.Unlock()
		if version, cached := omHelpProbe.version[id]; cached {
			return version
		}
	}
	var buf bytes.Buffer
	pctx, cancel := context.WithTimeout(ctx, omVersionTimeout)
	defer cancel()
	code, runErr := run(pctx, in.Dir, omUnsetEnv, []string{path, "version"}, &buf)
	version := parseOMVersion(buf.String())
	if runErr != nil || code != 0 || version == "" {
		r.warnf(in, "could not read the version of %s (%s); the verdict comment records it as %s",
			path, probeFailure(runErr, pctx, code), omVersionUnknown)
		return omVersionUnknown
	}
	if identifiable {
		omHelpProbe.version[id] = version
	}
	return version
}

// omInvocation is how this reviewer runs om: the binary the review and its
// probes are run through, and the process runner they share.
func (r OMReviewer) omInvocation() (string, runFunc) {
	path := r.Path
	if path == "" {
		path = "om"
	}
	run := r.run
	if run == nil {
		run = realRun
	}
	return path, omRestartWhileBusy(run)
}

// omBusyAttempts bounds the starts of one om exec that is refused because the
// executable is momentarily busy, and omBusyWait is the first pause between
// them, doubling each time: seven pauses of 1+2+4+...+64 ms, an eighth of a
// second in all, against an ETXTBSY that clears in a moment.
const (
	omBusyAttempts = 8
	omBusyWait     = time.Millisecond
)

// omRestartWhileBusy starts argv again while the kernel refuses it with
// ETXTBSY, and reports anything else at once.
//
// A file that was just written is "busy" until every descriptor open on it for
// writing is closed, including the ones a forked child is still holding before
// its own exec: fork inherits a descriptor, and O_CLOEXEC closes it only at
// that child's execve, so a fork by any goroutine in the window between the
// write and the close keeps the file unwritable for a moment
// (go.dev/issue/22315; internal/testpolicy/cmd/budget retries the same error
// when it starts a test binary that `go test` has just written). It is a
// property of the moment, not of the binary, so a restart is the whole answer.
//
// The om execs are where it lands: a landing that probes a just-written or
// just-installed om reads no version and no help without it, and the review's
// own probes are what a landing is judged on (gt-0pt32). The bound is what
// keeps a genuinely unstoppable om from holding a review: past it the error is
// the answer, as it was before. The pause is a timered select rather than a
// sleep, like every other wait in this package, so a canceled attempt stops
// waiting the moment it is canceled and the failing probe still starts the
// review's clock no later than it did before (internal/testpolicy, prod-no-sleep).
func omRestartWhileBusy(run runFunc) runFunc {
	return func(ctx context.Context, dir string, env, argv []string, out io.Writer) (int, error) {
		wait := omBusyWait
		for attempt := 0; ; attempt++ {
			code, err := run(ctx, dir, env, argv, out)
			if err == nil || !errors.Is(err, syscall.ETXTBSY) || attempt == omBusyAttempts-1 {
				return code, err
			}
			select {
			case <-ctx.Done():
				return code, err
			case <-time.After(wait):
			}
			wait *= 2
		}
	}
}

// parseOMVersion reads the version out of `om version`'s output, the line "om
// version <sha>", and "" when it carries none.
func parseOMVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "om" && fields[1] == "version" {
			return fields[2]
		}
	}
	return ""
}

// optionalFile writes one optional flag's text and returns the path to name it
// by, or "" when the installed om does not list the flag or the write fails. A
// text that cannot be written is a help to om rather than a precondition of the
// review, so the failure costs one warning line and the flag.
func (r OMReviewer) optionalFile(in ReviewInput, probed bool, help, flag, what string,
	write func(ReviewInput) (string, error)) string {
	if !probed {
		return ""
	}
	if !omHelpListsFlag(help, flag) {
		r.warnf(in, "om does not take %s; review runs without %s", flag, what)
		return ""
	}
	p, err := write(in)
	if err != nil {
		r.warnf(in, "could not write %s (%v); om runs without %s", what, err, flag)
		return ""
	}
	return p
}

// criteriaFile is how this reviewer writes its criteria file: the test seam
// when one is set, writeCriteria otherwise.
func (r OMReviewer) criteriaFile(in ReviewInput) (string, error) {
	if r.criteriaWriter != nil {
		return r.criteriaWriter(in)
	}
	return r.writeCriteria(in)
}

// acceptedRisksFile is criteriaFile's counterpart for the context file.
func (r OMReviewer) acceptedRisksFile(in ReviewInput) (string, error) {
	if r.acceptedRisksWriter != nil {
		return r.acceptedRisksWriter(in)
	}
	return r.writeAcceptedRisks(in)
}

// writeCriteria writes the review's acceptance text to a 0600 temp file in
// OutDir, beside the verdict file, and returns its path. The caller removes it
// after the run; a text that cannot be written is not a reason to skip the
// review.
// briefFile writes the work-item file to a 0600 temp file in OutDir and returns
// its path; the caller removes it after the run (gt-3l5qy).
func (r OMReviewer) briefFile(in ReviewInput) (string, error) {
	return writeTemp(r.OutDir, "om-brief-*.txt", []byte(in.BriefText))
}

// policyFile writes the town-policy block to a temp file in OutDir (gt-3l5qy).
func (r OMReviewer) policyFile(in ReviewInput) (string, error) {
	return writeTemp(r.OutDir, "om-policy-*.txt", []byte(in.PolicyText))
}

// roundsFile writes the rounds ledger to a temp file in OutDir (gt-3l5qy).
func (r OMReviewer) roundsFile(in ReviewInput) (string, error) {
	return writeTemp(r.OutDir, "om-rounds-*.json", []byte(in.RoundsText))
}

func (r OMReviewer) writeCriteria(in ReviewInput) (string, error) {
	return writeTemp(r.OutDir, "om-criteria-*.txt", []byte(in.CriteriaText))
}

// writeAcceptedRisks writes the review's accepted-risk record to a 0600 temp
// file in OutDir, beside the criteria file, and returns its path. The caller
// removes it after the run; a text that cannot be written is not a reason to
// skip the review (gt-lauly).
func (r OMReviewer) writeAcceptedRisks(in ReviewInput) (string, error) {
	return writeTemp(r.OutDir, "om-context-*.txt", []byte(in.AcceptedRisksText))
}

// writePriorFindings writes the previous round's findings to a 0600 temp file
// in OutDir, as the JSON array om's --prior-findings reads, and returns its
// path. The caller removes it after the run; findings that cannot be written
// are not a reason to skip the review.
func (r OMReviewer) writePriorFindings(in ReviewInput) (string, error) {
	data, err := json.Marshal(in.PriorFindings)
	if err != nil {
		return "", err
	}
	return writeTemp(r.OutDir, "om-prior-findings-*.json", data)
}

// writeTemp writes data to a 0600 temp file named pattern in dir and returns
// its path. A write or a close that fails removes it: the caller runs without
// the file it asked for rather than with a partial one.
func writeTemp(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
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

// probeFailure names why an om probe reported nothing: no output, no answer in
// time, or a process that would not start.
func probeFailure(runErr error, pctx context.Context, code int) string {
	switch {
	case runErr == nil:
		return fmt.Sprintf("exited %d with no output", code)
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

// storeVerdict keeps om's raw JSON for this review, when one is configured and
// the review names its work bead. The store is a record for a later reader,
// never a precondition of the landing: a write that fails is a warning line and
// the verdict stands (gt-uxqb9).
func (r OMReviewer) storeVerdict(in ReviewInput, raw []byte, argv []string) {
	if r.Verdicts == nil || in.BeadID == "" {
		return
	}
	rec := VerdictRecord{Bead: in.BeadID, Head: in.Head, OMVersion: in.omVer,
		Flags: flagNames(argv), Verdict: raw}
	if err := r.Verdicts.Write(rec, in.Logf); err != nil {
		r.warnf(in, "could not store the verdict (%v); the review stands", err)
	}
}

// flagNames is the flags argv carries, in order: what the store records so a
// reader knows what om was given.
func flagNames(argv []string) []string {
	var flags []string
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		}
	}
	return flags
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

// omHelpProbe caches what one installed om is: the `review --help` text the
// --criteria-file question is answered from, and the `version` a verdict
// comment records.
var omHelpProbe = struct {
	mu      sync.Mutex
	help    map[omBinaryID]string
	version map[omBinaryID]string
}{help: map[omBinaryID]string{}, version: map[omBinaryID]string{}}

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
