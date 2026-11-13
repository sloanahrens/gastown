package polecat

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	polecatBranchPrefix           = "polecat/"
	generatedIssueBranchSeparator = "+"
	legacyIssueBranchSeparator    = "@"
)

// BranchNameMeta is the structured identity encoded in a polecat branch name.
type BranchNameMeta struct {
	Polecat   string
	Issue     string
	Suffix    string
	Generated bool
}

// GeneratedAt decodes Suffix as the timestamp FormatGeneratedBranchName
// encoded (base36 UnixMilli). ok=false covers both a non-generated branch and
// a suffix that isn't a generated timestamp at all — e.g. the "backup-<sha>"
// form preserve-then-nuke writes, which contains a "-" and so is never a
// valid base36 numeral. Callers must treat ok=false as "age unknown", never
// as "old enough": this is the only source of age for the gt-527j remote
// prune guard, which fails closed when it cannot establish a branch's age.
func (m BranchNameMeta) GeneratedAt() (time.Time, bool) {
	if !m.Generated || m.Suffix == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(m.Suffix, 36, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// branchStampClock hands out the base-36 UnixMilli stamp that generated branch
// names carry. Each stamp is max(now, last+1), so two names built in one
// millisecond never share a stamp: a repair that follows an add inside the
// same millisecond would otherwise collide on an existing branch.
type branchStampClock struct {
	mu   sync.Mutex
	last int64 // last stamp handed out, in UnixMilli
}

// processBranchStamps is the sequence every Manager in this process draws
// from, so Managers built separately still never repeat a stamp.
var processBranchStamps = &branchStampClock{}

// next returns the stamp for the time now reports, bumped past the last one
// handed out. A bumped stamp runs ahead of the clock until the clock catches
// up, so GeneratedAt can report a time later than the real one.
func (c *branchStampClock) next(now func() time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ms := now().UnixMilli()
	if ms <= c.last {
		ms = c.last + 1
	}
	c.last = ms
	return strconv.FormatInt(ms, 36)
}

// FormatGeneratedBranchName returns the canonical generated polecat branch.
func FormatGeneratedBranchName(polecatName, issue, suffix string) string {
	if issue != "" {
		return fmt.Sprintf("%s%s/%s%s%s", polecatBranchPrefix, polecatName, issue, generatedIssueBranchSeparator, suffix)
	}
	return fmt.Sprintf("%s%s-%s", polecatBranchPrefix, polecatName, suffix)
}

// ParseBranchName decodes polecat branch names without guessing at dashed issue IDs.
func ParseBranchName(branch string) (BranchNameMeta, bool) {
	if !strings.HasPrefix(branch, polecatBranchPrefix) {
		return BranchNameMeta{}, false
	}

	rest := branch[len(polecatBranchPrefix):]
	if rest == "" {
		return BranchNameMeta{}, false
	}

	if slash := strings.Index(rest, "/"); slash >= 0 {
		if slash == 0 {
			return BranchNameMeta{}, false
		}
		polecatName := rest[:slash]
		issueTail := rest[slash+1:]
		if issueTail == "" || strings.Contains(issueTail, "/") {
			return BranchNameMeta{}, false
		}
		issue, suffix, generated, ok := parseIssueTail(issueTail)
		if !ok {
			return BranchNameMeta{}, false
		}
		return BranchNameMeta{Polecat: polecatName, Issue: issue, Suffix: suffix, Generated: generated}, true
	}

	dash := strings.LastIndex(rest, "-")
	if dash <= 0 || dash == len(rest)-1 {
		return BranchNameMeta{}, false
	}
	return BranchNameMeta{Polecat: rest[:dash], Suffix: rest[dash+1:], Generated: true}, true
}

// ParseGeneratedBranchName decodes only branch names emitted by FormatGeneratedBranchName
// and the legacy @ issue-suffix form kept for in-flight branches.
func ParseGeneratedBranchName(branch string) (BranchNameMeta, bool) {
	meta, ok := ParseBranchName(branch)
	if !ok || !meta.Generated {
		return BranchNameMeta{}, false
	}
	return meta, true
}

func parseIssueTail(issueTail string) (issue, suffix string, generated, ok bool) {
	delim := -1
	for _, sep := range []string{generatedIssueBranchSeparator, legacyIssueBranchSeparator} {
		if idx := strings.Index(issueTail, sep); idx >= 0 && (delim == -1 || idx < delim) {
			delim = idx
		}
	}
	if delim >= 0 {
		if delim == 0 || delim == len(issueTail)-1 {
			return "", "", false, false
		}
		return issueTail[:delim], issueTail[delim+1:], true, true
	}
	return issueTail, "", false, true
}
