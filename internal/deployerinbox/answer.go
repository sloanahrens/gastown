package deployerinbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// The statuses an answer reports for the request it answers.
const (
	// StatusProposed means the plan exists and the operator has not approved
	// it.
	StatusProposed = "proposed"
	// StatusApplied means the plan's changes are live.
	StatusApplied = "applied"
	// StatusRefused means the request failed a check and nothing was applied.
	StatusRefused = "refused"
	// StatusFailed means the apply started and did not complete.
	StatusFailed = "failed"
	// StatusRolledBack means the applied plan was undone.
	StatusRolledBack = "rolled-back"
)

// answerMode is the permission a published answer carries. A create's
// requested mode is cut by the process umask, so WriteAnswer chmods the temp
// file to this mode through the seam before it publishes the answer.
const answerMode os.FileMode = 0o644

// answerStatuses is the set of statuses an answer file may carry.
var answerStatuses = map[string]bool{
	StatusProposed:   true,
	StatusApplied:    true,
	StatusRefused:    true,
	StatusFailed:     true,
	StatusRolledBack: true,
}

// planSHA256 is the digest of the plan an answer reports on.
var planSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Answer is one file the deployer writes back for a request.
type Answer struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	PlanSHA256 string    `json:"plan_sha256"`
	Detail     string    `json:"detail"`
	At         time.Time `json:"at"`
}

// AnswerFS is the filesystem surface WriteAnswer writes through, so a test
// drives every refusal with an in-memory fake.
type AnswerFS interface {
	// CreateExclusive writes data to path with perm and fails when path
	// exists.
	CreateExclusive(path string, data []byte, perm os.FileMode) error
	// Chmod sets path's mode.
	Chmod(path string, mode os.FileMode) error
	// Link makes newpath a second name for oldpath and fails when newpath
	// exists.
	Link(oldpath, newpath string) error
	// Remove removes path.
	Remove(path string) error
}

// WriteAnswer publishes a as <dir>/<id>.json from a temp file it links into
// place, and never replaces an existing answer (gt-r2nhus).
func WriteAnswer(fsys AnswerFS, dir string, a Answer) error {
	if !requestID.MatchString(a.ID) {
		return fmt.Errorf("the answer id %q is not the id of a request", a.ID)
	}
	if !answerStatuses[a.Status] {
		return fmt.Errorf("the answer status %q is not proposed, applied, refused, failed or rolled-back", a.Status)
	}
	if len(a.Detail) > MaxDetailBytes {
		return fmt.Errorf("the answer detail is %d bytes, over the %d-byte limit", len(a.Detail), MaxDetailBytes)
	}
	if err := checkPlanSHA256(a.Status, a.PlanSHA256); err != nil {
		return err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode the answer %s: %w", a.ID, err)
	}

	target := filepath.Join(dir, a.ID+".json")
	tmp := target + ".tmp"
	if err := fsys.CreateExclusive(tmp, data, answerMode); err != nil {
		return fmt.Errorf("create the answer %s: %w", target, err)
	}
	if err := fsys.Chmod(tmp, answerMode); err != nil {
		return discardTemp(fsys, tmp, fmt.Errorf("set the mode of the answer %s: %w", target, err))
	}
	if err := fsys.Link(tmp, target); err != nil {
		return discardTemp(fsys, tmp, fmt.Errorf("publish the answer %s: %w", target, err))
	}
	if err := fsys.Remove(tmp); err != nil {
		return fmt.Errorf("remove the temp file %s after publishing the answer %s: %w", tmp, target, err)
	}
	return nil
}

// checkPlanSHA256 refuses a digest that is not 64 lower-case hex digits; an
// empty digest is allowed only for a refused or failed answer, which may name
// no plan.
func checkPlanSHA256(status, digest string) error {
	if digest == "" {
		if status == StatusRefused || status == StatusFailed {
			return nil
		}
		return fmt.Errorf("the answer has no plan_sha256, and only a refused or failed answer may omit it")
	}
	if !planSHA256.MatchString(digest) {
		return fmt.Errorf("the answer plan_sha256 %q is not 64 lower-case hex digits", digest)
	}
	return nil
}

// discardTemp removes tmp after a failed step and returns cause, joined with
// the removal failure when the temp file could not be removed.
func discardTemp(fsys AnswerFS, tmp string, cause error) error {
	if err := fsys.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(cause, fmt.Errorf("remove the temp file %s: %w", tmp, err))
	}
	return cause
}
