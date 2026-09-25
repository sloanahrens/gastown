package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
)

const (
	// bdMolTimeout is the timeout for bd molecule operations.
	bdMolTimeout = 15 * time.Second

	// dogCloseMaxAttempts / dogCloseRetryDelay bound the retry on `bd close` for
	// dog wisps. A transient Dolt slowdown (the connection-churn window) can make
	// a single close fail, and without a retry the wisp stays OPEN forever — a
	// root cause of the dog wisp flood (gt-ye21). Retrying turns a transient
	// failure back into a clean close instead of a permanent orphan.
	dogCloseMaxAttempts = 3
	dogCloseRetryDelay  = 500 * time.Millisecond

	// dogPourMaxAttempts bounds the retry of a single molecule pour. Only a pour
	// that never reached Dolt is retried (pourRetryable): the Dolt circuit
	// breaker being open or the server being briefly unreachable are the
	// transient states that clear within seconds (gt-i3rpw).
	dogPourMaxAttempts = 3

	// dogPourRetryDelay is the base backoff before retrying a pour, multiplied by
	// the attempt number, so retrying a fast-failing pour costs at most a few
	// seconds of the cycle it is already not accomplishing anything in.
	dogPourRetryDelay = 2 * time.Second

	// dogPourEscalateAfter is how many consecutive cycles one dog's molecule may
	// fail to pour before the daemon escalates. At that point the dog has stopped
	// being supervised, and nothing downstream would otherwise learn it
	// (gt-i3rpw).
	dogPourEscalateAfter = 3
)

// dogCycleOutcome is how a dog's patrol cycle ended. A cycle that was skipped
// must never read as a cycle that ran and found nothing: a supervisor which can
// silently skip is not supervising (gt-i3rpw).
type dogCycleOutcome string

const (
	// dogCycleRan: the molecule was poured and the cycle's steps are accounted
	// for. The only outcome that also leaves a receipt of its own — the root wisp
	// with its closed steps.
	dogCycleRan dogCycleOutcome = "ran"

	// dogCycleSkipped: the cycle did not run. The reason says why.
	dogCycleSkipped dogCycleOutcome = "skipped"

	// dogCycleFailed: the cycle had a molecule to run and its receipt is broken —
	// the wisp is there but cannot be addressed or read back, so the cycle
	// demonstrates nothing either way.
	dogCycleFailed dogCycleOutcome = "failed"
)

// closeWisp runs `bd close <id>` (plus any extra args) with bounded retries so a
// transient Dolt error does not leave the wisp open. Returns the final error if
// every attempt fails.
//
// A "blocked by open issues" error short-circuits the retry loop: it means a
// dependency hasn't closed yet, which sleeping and retrying the identical
// close cannot fix. closeRemainingSteps' multi-pass drain already re-queries
// and retries this same wisp on its next pass once a sibling closes and
// unblocks it, so retrying here would only burn dogCloseRetryDelay per
// attempt for a wait that this call alone can never resolve.
func (dm *dogMol) closeWisp(id string, extra ...string) error {
	args := append([]string{"close", id}, extra...)
	var err error
	for attempt := 1; attempt <= dogCloseMaxAttempts; attempt++ {
		if _, err = dm.runBd(args...); err == nil {
			return nil
		}
		if strings.Contains(err.Error(), "blocked by open issues") {
			return err
		}
		if attempt < dogCloseMaxAttempts {
			time.Sleep(time.Duration(attempt) * dogCloseRetryDelay)
		}
	}
	return err
}

// dogMol tracks a molecule (wisp) lifecycle for a daemon dog patrol.
// Graceful degradation: if bd fails, the dog still does its work — molecule
// tracking is observability, not control flow.
type dogMol struct {
	rootID   string            // Root wisp ID (e.g., "gt-wisp-abc123"), empty if pour failed.
	stepIDs  map[string]string // step slug -> wisp issue ID
	bdPath   string
	townRoot string
	logger   interface{ Printf(string, ...interface{}) }

	// formula is the dog formula this molecule was poured from, used to name the
	// dog in the cycle outcome record.
	formula string

	// outcome and outcomeReason are how this dog's cycle ended. pourDogMolecule
	// sets them on every path, and close reports them exactly once, so no cycle
	// can end without saying what it was (gt-i3rpw).
	outcome       dogCycleOutcome
	outcomeReason string

	// runBdFn overrides runBd's subprocess call when set. Tests use this to
	// simulate `bd` responses (including dependency-blocked closes) without a
	// real bd binary or Dolt server.
	runBdFn func(args ...string) (string, error)

	// waitFn overrides the pour retry backoff when set, so tests exercise the
	// retry path without spending wall-clock time.
	waitFn func(time.Duration)
}

// pourDogMolecule creates an ephemeral wisp molecule from a formula.
// Returns a dogMol handle for closing steps. If bd fails, returns a no-op
// handle so the caller can proceed without error checking.
//
// A failed pour is retried with bounded backoff, counted per dog, and escalated
// once the dog has failed dogPourEscalateAfter cycles in a row: the dog's whole
// patrol is being skipped, and a non-fatal log line per cycle is not something
// anyone learns from (gt-i3rpw).
func (d *Daemon) pourDogMolecule(formulaName string, vars map[string]string) *dogMol {
	dm := &dogMol{
		stepIDs:  make(map[string]string),
		formula:  formulaName,
		bdPath:   d.bdPath,
		townRoot: d.config.TownRoot,
		logger:   d.logger,
		runBdFn:  d.dogPourBdFn,
		waitFn:   d.dogPourWaitFn,
	}

	// Build args: bd mol wisp <formula> --var k=v ...
	args := []string{"mol", "wisp", formulaName}
	for k, v := range vars {
		args = append(args, "--var", fmt.Sprintf("%s=%s", k, v))
	}

	out, attempts, err := dm.pourWithRetry(args)
	if err != nil {
		dm.setOutcome(dogCycleSkipped, fmt.Sprintf("pour failed after %d attempt(s): %v", attempts, err))
		d.logger.Printf("dog_molecule: pour %s failed after %d attempt(s), cycle skipped: %v", formulaName, attempts, err)
		d.recordDogPourFailure(formulaName, dogCycleSkipped, dm.outcomeReason)
		return dm
	}

	// Parse root ID from output. bd mol wisp prints the root ID on the first line.
	// Example output: "✓ Spawned wisp: gt-wisp-abc123 — Reap stale wisps..."
	dm.rootID = parseWispID(out)
	if dm.rootID == "" {
		// The wisp exists but cannot be addressed, so every closeStep and close
		// below is a no-op and the cycle leaves no receipt. That is a broken
		// receipt, not a clean run.
		dm.setOutcome(dogCycleFailed, fmt.Sprintf("poured but no root ID in output: %.200s", out))
		d.logger.Printf("dog_molecule: pour %s: could not parse root ID from output: %s", formulaName, out)
		d.recordDogPourFailure(formulaName, dogCycleFailed, dm.outcomeReason)
		return dm
	}

	// Discover step IDs by listing children of the root wisp.
	if err := dm.discoverSteps(); err != nil {
		dm.setOutcome(dogCycleFailed, fmt.Sprintf("poured %s but its steps could not be read back: %v", dm.rootID, err))
		d.recordDogPourFailure(formulaName, dogCycleFailed, dm.outcomeReason)
		return dm
	}

	d.recordDogPourSuccess(formulaName)

	dm.setOutcome(dogCycleRan, "")
	d.logger.Printf("dog_molecule: poured %s → %s (%d steps)", formulaName, dm.rootID, len(dm.stepIDs))
	return dm
}

// setOutcome records how the cycle this handle belongs to ended.
func (dm *dogMol) setOutcome(outcome dogCycleOutcome, reason string) {
	dm.outcome = outcome
	dm.outcomeReason = reason
}

// pourWithRetry runs the pour with bounded backoff and returns the successful
// output plus the number of attempts it took. It returns the last error
// unwrapped so the caller's message carries the real cause (a Dolt circuit
// breaker, an unreachable server, a timeout) rather than "exit status 1".
func (dm *dogMol) pourWithRetry(args []string) (string, int, error) {
	var err error
	var out string
	attempts := 0
	for attempt := 1; attempt <= dogPourMaxAttempts; attempt++ {
		attempts = attempt
		if out, err = dm.runBd(args...); err == nil {
			return out, attempt, nil
		}
		if !pourRetryable(err) || attempt == dogPourMaxAttempts {
			break
		}
		dm.wait(time.Duration(attempt) * dogPourRetryDelay)
	}
	return "", attempts, err
}

// pourRetryable reports whether a failed pour is safe to repeat.
//
// Only failures that provably never reached Dolt are retried. A pour killed by
// its own deadline is excluded even though timeouts are the most common failure
// in the log: the client cannot tell "never committed" from "committed, and the
// answer was lost", and re-pouring the second case strands a root wisp plus its
// step wisps that nothing will ever close — the flood gt-ye21 built
// closeRemainingSteps to prevent. That cycle is skipped and counted instead,
// which the alarm covers.
func pourRetryable(err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"circuit breaker is open", // Dolt refused the request outright
		"unreachable",
		"connection refused",
		"connection reset",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// wait sleeps for the pour retry backoff, unless a test replaced it.
func (dm *dogMol) wait(d time.Duration) {
	if dm.waitFn != nil {
		dm.waitFn(d)
		return
	}
	time.Sleep(d)
}

// closeStep marks a molecule step as closed.
func (dm *dogMol) closeStep(stepSlug string) {
	if dm.rootID == "" {
		return // No molecule — graceful degradation.
	}

	stepID, ok := dm.stepIDs[stepSlug]
	if !ok {
		dm.logger.Printf("dog_molecule: closeStep %q: unknown step (known: %v)", stepSlug, dm.knownSteps())
		return
	}

	if err := dm.closeWisp(stepID); err != nil {
		dm.logger.Printf("dog_molecule: close step %s (%s) failed after %d attempts (non-fatal): %v", stepSlug, stepID, dogCloseMaxAttempts, err)
		return
	}
}

// skipStep closes a molecule step that ran and decided not to act, recording
// why. A skipped step is not a failure — a patrol whose correct outcome is
// silence has to be able to say so, or its receipt reads as a run that never
// happened (gt-59o9).
func (dm *dogMol) skipStep(stepSlug, reason string) {
	if dm.rootID == "" {
		return
	}

	stepID, ok := dm.stepIDs[stepSlug]
	if !ok {
		dm.logger.Printf("dog_molecule: skipStep %q: unknown step (known: %v)", stepSlug, dm.knownSteps())
		return
	}

	if err := dm.closeWisp(stepID, "--reason", "skipped: "+reason); err != nil {
		dm.logger.Printf("dog_molecule: skip step %s (%s) failed after %d attempts (non-fatal): %v", stepSlug, stepID, dogCloseMaxAttempts, err)
	}
}

// failStep marks a molecule step as failed with a reason.
func (dm *dogMol) failStep(stepSlug, reason string) {
	if dm.rootID == "" {
		return
	}

	stepID, ok := dm.stepIDs[stepSlug]
	if !ok {
		dm.logger.Printf("dog_molecule: failStep %q: unknown step", stepSlug)
		return
	}

	if err := dm.closeWisp(stepID, "--reason", reason); err != nil {
		dm.logger.Printf("dog_molecule: fail step %s (%s) failed after %d attempts (non-fatal): %v", stepSlug, stepID, dogCloseMaxAttempts, err)
	}
}

// close closes all remaining open child step wisps, then closes the root molecule wisp.
// This prevents orphan step wisps from accumulating when callers forget to
// explicitly close individual steps (the root cause of gt-3o59).
func (dm *dogMol) close() {
	// Report before the rootID guard: the cycles this matters most for are the
	// ones with no root at all.
	dm.reportOutcome()

	if dm.rootID == "" {
		return
	}

	// Close any step wisps that were never explicitly closed/failed.
	dm.closeRemainingSteps()

	if err := dm.closeWisp(dm.rootID); err != nil {
		dm.logger.Printf("dog_molecule: close root %s failed after %d attempts (non-fatal): %v", dm.rootID, dogCloseMaxAttempts, err)
	}
}

// reportOutcome writes the cycle's one outcome record: a machine-readable line
// in the daemon log, plus a feed event for anything that is not a clean run.
//
// Every dog defers close(), so every poured cycle leaves exactly one of these
// and a skipped cycle can never be read as a cycle that ran and found nothing
// (gt-i3rpw). The log line covers every outcome so a grep for `outcome=` finds
// each cycle; the feed gets only the outcomes with no receipt of their own,
// which is what a reader who never sees the daemon log has to go on.
func (dm *dogMol) reportOutcome() {
	if dm.outcome == "" {
		// A dogMol that never went through pourDogMolecule (tests build one
		// directly). It has no formula to name and no cycle to report.
		return
	}

	dm.logger.Printf("dog_molecule: cycle %s outcome=%s reason=%q", dm.formula, dm.outcome, dm.outcomeReason)

	if dm.outcome == dogCycleRan {
		return
	}

	payload := map[string]interface{}{
		"formula": dm.formula,
		"outcome": string(dm.outcome),
		"reason":  dm.outcomeReason,
	}
	if err := events.LogFeedTo(dm.townRoot, events.TypeDogCycleOutcome, events.ActorDaemon, payload); err != nil {
		dm.logger.Printf("dog_molecule: recording cycle outcome for %s failed: %v", dm.formula, err)
	}
}

// closeRemainingSteps queries all children of the root wisp and closes any that
// are still open. This is the backstop that prevents step wisp leaks regardless
// of whether individual callers remembered to close each step.
//
// Step wisps are typically chained by formula order (step N depends on step
// N-1), so `bd close` on a not-yet-unblocked step fails with "blocked by open
// issues". A single pass over children in list order therefore only succeeds
// when that order happens to be leaf-first; any other order strands the whole
// dependency tail after 3 retries each (gt-bygj). Closing repeatedly in
// passes fixes this without needing the dependency graph: each pass closes
// whatever is currently closable, which unblocks the next tier for the
// following pass, so the tail drains in at most len(children) passes. Once a
// pass makes no progress (a real cycle, or a blocker outside this root's
// children), the remainder is force-closed so the daemon-owned root does not
// orphan its tail forever.
func (dm *dogMol) closeRemainingSteps() {
	if dm.rootID == "" {
		return
	}

	closed := 0
	forced := 0
	maxPasses := -1 // set from the initial child count once the first pass lists them

	for pass := 1; ; pass++ {
		out, err := dm.runBd("show", dm.rootID, "--children", "--json")
		if err != nil {
			dm.logger.Printf("dog_molecule: closeRemainingSteps: list children of %s failed: %v", dm.rootID, err)
			break
		}

		children, parseErr := parseChildrenJSON(out)
		if parseErr != nil {
			dm.logger.Printf("dog_molecule: closeRemainingSteps: parse children JSON for %s failed: %v", dm.rootID, parseErr)
			break
		}

		if maxPasses < 0 {
			// A real dependency chain drains in at most len(children) passes
			// (each successful pass closes at least one leaf); +1 covers this
			// first listing pass. Without a cap, a bd bug that reports
			// "progress" without actually closing anything would spin the
			// daemon forever instead of falling through to force-close.
			maxPasses = len(children) + 1
		}

		var remaining []childInfo
		for _, child := range children {
			if child.ID == "" || child.Status == "" {
				continue
			}
			if child.Status == "open" || child.Status == "hooked" || child.Status == "in_progress" {
				remaining = append(remaining, child)
			}
		}
		if len(remaining) == 0 {
			break
		}

		if pass > maxPasses {
			dm.logger.Printf("dog_molecule: closeRemainingSteps: pass cap (%d) reached for %s, force-closing %d remaining", maxPasses, dm.rootID, len(remaining))
			for _, child := range remaining {
				if err := dm.closeWisp(child.ID, "--force", "--reason", "abandoned: dependency-blocked tail under closed dog molecule root"); err != nil {
					dm.logger.Printf("dog_molecule: closeRemainingSteps: force-close %s failed after %d attempts: %v", child.ID, dogCloseMaxAttempts, err)
				} else {
					forced++
				}
			}
			break
		}

		var stillOpen []childInfo
		progressed := false
		for _, child := range remaining {
			if err := dm.closeWisp(child.ID); err != nil {
				stillOpen = append(stillOpen, child)
			} else {
				closed++
				progressed = true
			}
		}

		if progressed {
			continue // Re-query: closes this pass may have unblocked others.
		}

		// No child closed this pass — remaining closes are blocked on each
		// other with no leaf left to start from (a cycle, or a blocker
		// outside this root's own children). Force-close the tail rather
		// than leaving it HOOKED/open forever.
		for _, child := range stillOpen {
			if err := dm.closeWisp(child.ID, "--force", "--reason", "abandoned: dependency-blocked tail under closed dog molecule root"); err != nil {
				dm.logger.Printf("dog_molecule: closeRemainingSteps: force-close %s failed after %d attempts: %v", child.ID, dogCloseMaxAttempts, err)
			} else {
				forced++
			}
		}
		break
	}

	if closed > 0 || forced > 0 {
		dm.logger.Printf("dog_molecule: closeRemainingSteps: closed %d orphan step wisp(s) (%d forced) under %s", closed, forced, dm.rootID)
	}
}

// discoverSteps lists children of the root wisp and maps step slugs to IDs.
//
// The slug for a child is the formula step ID it instantiates, and the child's
// title is the only handle back to it: a poured step is a wisp with a fresh
// random ID and the formula's step title, and nothing else of the step survives
// the pour. So the map comes from the formula's own step list, asked of bd —
// the same resolver, from the same working directory, that poured the molecule.
// A step the map misses is closed by closeRemainingSteps' sweep instead of by
// the code that ran it (gt-i9la).
//
// A failure to list or parse the children is returned, not just logged: the
// molecule exists but cannot be read back, so every closeStep this cycle makes
// is a no-op and the cycle has no receipt. The caller records that as
// dogCycleFailed, because a receipt that was never written is not the same
// observable as a clean run (gt-i3rpw).
func (dm *dogMol) discoverSteps() error {
	if dm.rootID == "" {
		return nil
	}

	out, err := dm.runBd("show", dm.rootID, "--children", "--json")
	if err != nil {
		dm.logger.Printf("dog_molecule: discover steps for %s failed: %v", dm.rootID, err)
		return fmt.Errorf("list children of %s: %w", dm.rootID, err)
	}

	children, parseErr := parseChildrenJSON(out)
	if parseErr != nil {
		dm.logger.Printf("dog_molecule: discover steps: parse children JSON for %s failed: %v", dm.rootID, parseErr)
		return fmt.Errorf("parse children of %s: %w", dm.rootID, parseErr)
	}

	if len(children) == 0 {
		// Nothing was poured to map. Asking bd for a formula whose children do
		// not exist would only add a subprocess and a warning.
		return nil
	}

	slugsByTitle, err := dm.stepSlugsByTitle()
	if err != nil {
		// Not fatal, and not returned: the molecule is readable, so the cycle
		// still runs and every step is still closed — by closeRemainingSteps'
		// sweep rather than by the step that ran it. That is a worse receipt,
		// not a broken one, and the log line names the cause.
		dm.logger.Printf("dog_molecule: cannot read the steps of %s (%v); this cycle's steps will be closed by the sweep", dm.formula, err)
	}

	for _, child := range children {
		if child.ID == "" || child.Title == "" {
			continue
		}

		slug, ok := slugsByTitle[stepTitleKey(child.Title)]
		if !ok {
			dm.logger.Printf("dog_molecule: %s: child %s (%q) matches no step of the formula — it will be closed by the sweep", dm.formula, child.ID, child.Title)
			continue
		}
		if other, dup := dm.stepIDs[slug]; dup {
			dm.logger.Printf("dog_molecule: %s: step %q is already mapped to %s; %s left to the sweep", dm.formula, slug, other, child.ID)
			continue
		}
		dm.stepIDs[slug] = child.ID
	}

	return nil
}

// stepSlugsByTitle returns the formula's step IDs keyed by normalized step
// title, so a child wisp can be traced back to the step it instantiates.
func (dm *dogMol) stepSlugsByTitle() (map[string]string, error) {
	out, err := dm.runBd("formula", "show", dm.formula, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd formula show %s: %w", dm.formula, err)
	}

	steps, err := parseFormulaStepsJSON(out)
	if err != nil {
		return nil, fmt.Errorf("bd formula show %s: %w", dm.formula, err)
	}

	byTitle := make(map[string]string, len(steps))
	for _, step := range steps {
		if step.ID == "" || step.Title == "" {
			continue
		}
		key := stepTitleKey(step.Title)
		if existing, dup := byTitle[key]; dup {
			// Two steps with one title are indistinguishable from their children,
			// so the first keeps the title and the other's child falls to the
			// sweep. Naming the shadowed step is the only way to see it (gt-i9la).
			dm.logger.Printf("dog_molecule: %s: steps %q and %q share the title %q; only %q is matched", dm.formula, existing, step.ID, step.Title, existing)
			continue
		}
		byTitle[key] = step.ID
	}

	return byTitle, nil
}

// stepTitleKey normalizes a step title for matching a child wisp against the
// formula. bd pours a child with the step's title verbatim, so folding case and
// whitespace is what keeps a stray extra space from unmapping the step.
func stepTitleKey(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(title)), " ")
}

// formulaStep is the part of `bd formula show --json` this package reads: the
// formula's step IDs and their titles.
type formulaStep struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// parseFormulaStepsJSON extracts the steps from `bd formula show <name> --json`.
// The envelope carries the whole formula (description, vars, composition rules);
// only the steps matter here.
func parseFormulaStepsJSON(raw string) ([]formulaStep, error) {
	var envelope struct {
		Steps []formulaStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, fmt.Errorf("parse formula steps JSON: %w", err)
	}
	if len(envelope.Steps) == 0 {
		return nil, fmt.Errorf("formula has no steps")
	}
	return envelope.Steps, nil
}

// childInfo holds fields from child wisp JSON used by discoverSteps and
// closeRemainingSteps.
type childInfo struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// parseChildrenJSON parses the output of `bd show <id> --children --json`.
// bd returns a map keyed by parent ID plus envelope metadata:
// {"hq-wisp-abc": [{...}, ...], "schema_version": 1}.
// For legacy compatibility, a bare array is also accepted.
func parseChildrenJSON(raw string) ([]childInfo, error) {
	data := bytes.TrimSpace([]byte(raw))
	if len(data) == 0 {
		return nil, fmt.Errorf("empty children JSON")
	}

	var arr []childInfo
	if data[0] == '[' {
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}

	if data[0] != '{' {
		return nil, fmt.Errorf("unrecognized JSON shape: %.200s", raw)
	}

	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(wrapped))
	for key := range wrapped {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var children []childInfo
	sawChildArray := false
	for _, key := range keys {
		if key == "schema_version" {
			continue
		}

		value := bytes.TrimSpace(wrapped[key])
		if len(value) == 0 {
			return nil, fmt.Errorf("empty child payload for key %q", key)
		}
		if value[0] != '[' {
			return nil, fmt.Errorf("non-array child payload for key %q", key)
		}

		var group []childInfo
		if err := json.Unmarshal(value, &group); err != nil {
			return nil, fmt.Errorf("parse child array for key %q: %w", key, err)
		}
		children = append(children, group...)
		sawChildArray = true
	}

	if !sawChildArray {
		return nil, fmt.Errorf("children JSON object has no child arrays")
	}

	return children, nil
}

// knownSteps returns the list of known step slugs for debugging.
func (dm *dogMol) knownSteps() []string {
	var steps []string
	for k := range dm.stepIDs {
		steps = append(steps, k)
	}
	return steps
}

// runBd executes a bd command and returns stdout.
func (dm *dogMol) runBd(args ...string) (string, error) {
	if dm.runBdFn != nil {
		return dm.runBdFn(args...)
	}

	bdPath := dm.bdPath
	if bdPath == "" {
		bdPath = "bd"
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdMolTimeout)
	defer cancel()

	cmd := beads.CommandContextWithBin(ctx, bdPath, dm.townRoot, filepath.Join(dm.townRoot, ".beads"), beads.SubprocessModeForArgs(args), args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Name the deadline instead of passing on the "signal: killed" the kill
		// leaves behind, so the retry policy below can tell a pour that never
		// reached Dolt from one that was still running when its clock ran out.
		err = beads.SubprocessFailureError(ctx, bdMolTimeout, err)
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("%s: %s", err, errMsg)
		}
		return "", err
	}

	return strings.TrimSpace(stdout.String()), nil
}

// parseWispID extracts a wisp ID from bd mol wisp output.
// Looks for patterns like "gt-wisp-abc123" or any ID containing "-wisp-".
func parseWispID(output string) string {
	for _, word := range strings.Fields(output) {
		// Strip ANSI codes and punctuation.
		cleaned := stripANSI(word)
		cleaned = strings.TrimRight(cleaned, ".,;:!?")
		if strings.Contains(cleaned, "-wisp-") {
			return cleaned
		}
	}
	// Fallback: look for any bead-like ID (prefix-xxxx pattern).
	for _, word := range strings.Fields(output) {
		cleaned := stripANSI(word)
		cleaned = strings.TrimRight(cleaned, ".,;:!?")
		if len(cleaned) > 3 && strings.Contains(cleaned, "-") && !strings.HasPrefix(cleaned, "--") {
			// Could be a bead ID like "gt-abc123".
			return cleaned
		}
	}
	return ""
}

// dogPourHealth is one dog's molecule-pour health across patrol cycles: how
// many cycles in a row it could not pour, and whether that is currently
// escalated. The count is consecutive, not cumulative — a single success means
// the outage is over and the next failure starts a new streak.
type dogPourHealth struct {
	consecutiveFailures int
	escalated           bool
}

// dogPourAlertKey returns the escalation fingerprint for one dog's pour
// failures. Per dog rather than per town: each dog owns its own alarm and
// recovers from it independently, and the fingerprint keeps a persisting outage
// to one open escalation instead of one per cycle (gt-vwry).
//
// The key is the formula, not the call site, so mol-dog-doctor's periodic pour
// and its anomaly-triggered one share a streak. They are the same dog asking for
// the same molecule, so a pour from either means the dog was supervised, and a
// count that sums both is the count of "cycles this dog had no receipt" — which
// is what the escalation says.
func dogPourAlertKey(formulaName string) string {
	return "dog_molecule:pour:" + formulaName
}

// recordDogPourFailure counts a cycle that could not establish its molecule and
// escalates once the count reaches dogPourEscalateAfter. Without the alarm, a
// pour that fails forever costs one non-fatal log line per cycle while the dog
// skips its whole patrol — how gt-ecqx0's self-probe verdict went unread for a
// day (gt-i3rpw).
//
// One alert per failure streak, not one per cycle: the open escalation is the
// standing signal until recordDogPourSuccess closes it, so an outage lasting
// days does not mint a comment every five minutes (gt-vwry). The latch is
// reserved before the send and released if the send fails, so a dropped
// escalation is retried on the next failed cycle rather than closing the streak
// silently.
//
// The message leads with the dog's name and carries the same outcome token the
// cycle record uses, because only the first maxEscalationTitleLen runes of it
// become the escalation's title; the rest lives in the body.
func (d *Daemon) recordDogPourFailure(formulaName string, outcome dogCycleOutcome, detail string) {
	d.dogPourMu.Lock()
	if d.dogPour == nil {
		d.dogPour = make(map[string]dogPourHealth)
	}
	health := d.dogPour[formulaName]
	health.consecutiveFailures++
	shouldEscalate := health.consecutiveFailures >= dogPourEscalateAfter && !health.escalated
	if shouldEscalate {
		health.escalated = true
	}
	d.dogPour[formulaName] = health
	consecutive := health.consecutiveFailures
	d.dogPourMu.Unlock()

	// Escalate outside the lock: escalateAlert shells out to `gt escalate` with
	// retries, and holding dogPourMu across it would serialize every other dog's
	// pour bookkeeping behind one slow escalation.
	if !shouldEscalate {
		return
	}

	d.logger.Printf("dog_molecule: ESCALATION: %s has no molecule receipt for %d consecutive cycles (outcome=%s)", formulaName, consecutive, outcome)
	err := d.escalateAlertErr(dogPourAlertKey(formulaName), "dog_molecule", fmt.Sprintf(
		"%s: %d consecutive cycles have no molecule receipt (outcome=%s): %s. The dog's whole patrol is being dropped and its verdicts reach nobody, so this is not a clean run. Check bd/Dolt health: gt dolt status",
		formulaName, consecutive, outcome, detail))
	if err == nil {
		return
	}

	// The latch was reserved above, not earned: gt escalate writes a bead, so it
	// fails for exactly the Dolt outage that caused this streak. Releasing it lets
	// the next failing cycle try again — an alarm that never landed is the one
	// outcome that must not read as done.
	d.logger.Printf("dog_molecule: escalation for %s did not send (%v); will retry on the next failed cycle", formulaName, err)
	d.dogPourMu.Lock()
	if health, ok := d.dogPour[formulaName]; ok && health.escalated {
		health.escalated = false
		d.dogPour[formulaName] = health
	}
	d.dogPourMu.Unlock()
}

// recordDogPourSuccess ends a failure streak for one dog and closes the
// escalation it raised, if any.
//
// clearAlerts is skipped when no alert was raised, so the ordinary case — a dog
// that has been pouring fine all along — costs no subprocess.
func (d *Daemon) recordDogPourSuccess(formulaName string) {
	d.dogPourMu.Lock()
	health, tracked := d.dogPour[formulaName]
	if tracked {
		delete(d.dogPour, formulaName)
	}
	d.dogPourMu.Unlock()

	if tracked && health.escalated {
		d.clearAlerts(fmt.Sprintf("%s is pouring again", formulaName), dogPourAlertKey(formulaName))
	}
}

// stripANSI removes ANSI escape codes from a string.
func stripANSI(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			// Skip escape sequence.
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
					i++
				}
				if i < len(s) {
					i++ // Skip the terminating letter.
				}
			}
		} else {
			result.WriteByte(s[i])
			i++
		}
	}
	return result.String()
}
