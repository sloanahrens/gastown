package deployerinbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
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

// answerStatuses is the set of statuses an answer file may carry.
var answerStatuses = map[string]bool{
	StatusProposed:   true,
	StatusApplied:    true,
	StatusRefused:    true,
	StatusFailed:     true,
	StatusRolledBack: true,
}

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
	// WriteFile writes data to name with perm.
	WriteFile(name string, data []byte, perm fs.FileMode) error
	// Rename renames oldpath to newpath.
	Rename(oldpath, newpath string) error
	// Stat reports name's information, and fs.ErrNotExist when name is absent.
	Stat(name string) (fs.FileInfo, error)
}

// WriteAnswer publishes a as <dir>/<id>.json by writing a temp file and
// renaming it, and never replaces an existing answer (gt-r2nhus).
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

	target := filepath.Join(dir, a.ID+".json")

	// Only one runner writes an inbox's answers, so this check keeps a second
	// answer for the same request out; it is not a lock against a concurrent
	// writer.
	if _, err := fsys.Stat(target); err == nil {
		return fmt.Errorf("the answer %s exists already, and an answer is written once", target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check the answer %s: %w", target, err)
	}

	data, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode the answer %s: %w", target, err)
	}
	tmp := target + ".tmp"
	if err := fsys.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write the answer %s: %w", target, err)
	}
	if err := fsys.Rename(tmp, target); err != nil {
		return fmt.Errorf("publish the answer %s: %w", target, err)
	}
	return nil
}
