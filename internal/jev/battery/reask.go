package battery

import (
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ReaskPercent is the share of a battery's groups asked again with the
	// identical state, so the spread of the answers measures how far Jev moves
	// when nothing changes. The operator approved 2 percent on 2026-10-30.
	ReaskPercent = 2

	// ReaskRepeats is how many times one sampled group is asked again. The
	// operator approved 5 on 2026-10-30.
	ReaskRepeats = 5
)

// reaskBuckets is the number of hash buckets ReaskPercent of the keys fall in.
const reaskBuckets = 100 / ReaskPercent

// Sampled reports whether the group key at version is asked again. The pair is
// hashed, so a group samples the same way on every pass and a changed question
// set resamples from scratch. The version's eight bytes end the hashed input,
// which keeps every pair distinct.
func Sampled(key string, version int) bool {
	h := sha256.New()
	_, _ = h.Write([]byte(key))
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], uint64(version))
	_, _ = h.Write(v[:])
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])%reaskBuckets == 0
}

// ReaskLine is one line of a battery's repeat log: what one repeat of a
// sampled group asked and what came back.
type ReaskLine struct {
	// TS is when the repeat's answer was logged.
	TS time.Time `json:"ts"`
	// Key is the sampled group's pattern key.
	Key string `json:"key"`
	// Repeat is the repeat's number, 1 through ReaskRepeats.
	Repeat int `json:"repeat"`
	// QuestionVersion is the question set the repeat asked.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned Jev model that answered.
	Model string `json:"model"`
	// Answers maps each question id to its answer, in the battery log's own
	// shape.
	Answers any `json:"answers"`
}

// ReaskPath is the repeat log that belongs to the battery log at logPath: the
// same name with .reask before its extension, beside it.
func ReaskPath(logPath string) string {
	ext := filepath.Ext(logPath)
	return strings.TrimSuffix(logPath, ext) + ".reask" + ext
}

// AppendReask appends one repeat line to the repeat log beside the battery log
// at logPath, under the log's own directory and file modes.
func AppendReask(logPath string, line ReaskLine) error {
	return Append(ReaskPath(logPath), line)
}
