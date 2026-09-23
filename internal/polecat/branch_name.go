package polecat

import (
	"fmt"
	"strconv"
	"strings"
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
