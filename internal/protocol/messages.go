package protocol

import (
	"fmt"
	"strings"
	"time"
)

// ParseFixNeededPayload parses a FIX_NEEDED message body into a payload.
// Returns an error if required fields (Branch, Polecat, Rig) are missing.
func ParseFixNeededPayload(body string) (*FixNeededPayload, error) {
	payload := &FixNeededPayload{
		Branch:       parseField(body, "Branch"),
		Issue:        parseField(body, "Issue"),
		Polecat:      parseField(body, "Polecat"),
		Rig:          parseField(body, "Rig"),
		TargetBranch: parseField(body, "Target"),
		FailureType:  parseField(body, "Failure-Type"),
		Error:        parseField(body, "Error"),
		MRBeadID:     parseField(body, "MR-Bead-ID"),
	}

	// Parse timestamp
	if ts := parseField(body, "Failed-At"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			payload.FailedAt = t
		}
	}

	// Parse attempt number
	if an := parseField(body, "Attempt-Number"); an != "" {
		fmt.Sscanf(an, "%d", &payload.AttemptNumber)
	}

	var errs []string
	if payload.Branch == "" {
		errs = append(errs, "Branch")
	}
	if payload.Polecat == "" {
		errs = append(errs, "Polecat")
	}
	if payload.Rig == "" {
		errs = append(errs, "Rig")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid FIX_NEEDED payload: missing required fields: %s", strings.Join(errs, ", "))
	}

	return payload, nil
}

// ParseConvoyNeedsFeedingPayload parses a CONVOY_NEEDS_FEEDING message body.
// Returns an error if required fields (ConvoyID, Rig) are missing.
func ParseConvoyNeedsFeedingPayload(body string) (*ConvoyNeedsFeedingPayload, error) {
	payload := &ConvoyNeedsFeedingPayload{
		ConvoyID:    parseField(body, "ConvoyID"),
		SourceIssue: parseField(body, "SourceIssue"),
		Rig:         parseField(body, "Rig"),
	}

	if ts := parseField(body, "Merged-At"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			payload.MergedAt = t
		}
	}

	var errs []string
	if payload.ConvoyID == "" {
		errs = append(errs, "ConvoyID")
	}
	if payload.Rig == "" {
		errs = append(errs, "Rig")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid CONVOY_NEEDS_FEEDING payload: missing required fields: %s", strings.Join(errs, ", "))
	}

	return payload, nil
}

// ParseMergeReadyPayload parses a MERGE_READY message body into a payload.
// Returns an error if required fields (Branch, Polecat, Rig) are missing.
func ParseMergeReadyPayload(body string) (*MergeReadyPayload, error) {
	payload := &MergeReadyPayload{
		Branch:    parseField(body, "Branch"),
		Issue:     parseField(body, "Issue"),
		Polecat:   parseField(body, "Polecat"),
		Rig:       parseField(body, "Rig"),
		Verified:  parseField(body, "Verified"),
		Timestamp: time.Now(), // Use current time if not parseable
	}

	var errs []string
	if payload.Branch == "" {
		errs = append(errs, "Branch")
	}
	if payload.Polecat == "" {
		errs = append(errs, "Polecat")
	}
	if payload.Rig == "" {
		errs = append(errs, "Rig")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid MERGE_READY payload: missing required fields: %s", strings.Join(errs, ", "))
	}

	return payload, nil
}

// ParseMergedPayload parses a MERGED message body into a payload.
// Returns an error if required fields (Branch, Polecat, Rig) are missing.
func ParseMergedPayload(body string) (*MergedPayload, error) {
	payload := &MergedPayload{
		Branch:       parseField(body, "Branch"),
		Issue:        parseField(body, "Issue"),
		Polecat:      parseField(body, "Polecat"),
		Rig:          parseField(body, "Rig"),
		TargetBranch: parseField(body, "Target"),
		MergeCommit:  parseField(body, "Merge-Commit"),
	}

	// Parse timestamp
	if ts := parseField(body, "Merged-At"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			payload.MergedAt = t
		}
	}

	var errs []string
	if payload.Branch == "" {
		errs = append(errs, "Branch")
	}
	if payload.Polecat == "" {
		errs = append(errs, "Polecat")
	}
	if payload.Rig == "" {
		errs = append(errs, "Rig")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid MERGED payload: missing required fields: %s", strings.Join(errs, ", "))
	}

	return payload, nil
}

// ParseMergeFailedPayload parses a MERGE_FAILED message body into a payload.
// Returns an error if required fields (Branch, Polecat, Rig) are missing.
func ParseMergeFailedPayload(body string) (*MergeFailedPayload, error) {
	payload := &MergeFailedPayload{
		Branch:       parseField(body, "Branch"),
		Issue:        parseField(body, "Issue"),
		Polecat:      parseField(body, "Polecat"),
		Rig:          parseField(body, "Rig"),
		TargetBranch: parseField(body, "Target"),
		FailureType:  parseField(body, "Failure-Type"),
		Error:        parseField(body, "Error"),
	}

	// Parse timestamp
	if ts := parseField(body, "Failed-At"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			payload.FailedAt = t
		}
	}

	var errs []string
	if payload.Branch == "" {
		errs = append(errs, "Branch")
	}
	if payload.Polecat == "" {
		errs = append(errs, "Polecat")
	}
	if payload.Rig == "" {
		errs = append(errs, "Rig")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid MERGE_FAILED payload: missing required fields: %s", strings.Join(errs, ", "))
	}

	return payload, nil
}

// ParseReworkRequestPayload parses a REWORK_REQUEST message body into a payload.
// Returns an error if required fields (Branch, Polecat, Rig) are missing.
func ParseReworkRequestPayload(body string) (*ReworkRequestPayload, error) {
	payload := &ReworkRequestPayload{
		Branch:       parseField(body, "Branch"),
		Issue:        parseField(body, "Issue"),
		Polecat:      parseField(body, "Polecat"),
		Rig:          parseField(body, "Rig"),
		TargetBranch: parseField(body, "Target"),
	}

	// Parse timestamp
	if ts := parseField(body, "Requested-At"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			payload.RequestedAt = t
		}
	}

	// Parse conflict files
	if files := parseField(body, "Conflict-Files"); files != "" {
		payload.ConflictFiles = strings.Split(files, ", ")
	}

	var errs []string
	if payload.Branch == "" {
		errs = append(errs, "Branch")
	}
	if payload.Polecat == "" {
		errs = append(errs, "Polecat")
	}
	if payload.Rig == "" {
		errs = append(errs, "Rig")
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid REWORK_REQUEST payload: missing required fields: %s", strings.Join(errs, ", "))
	}

	return payload, nil
}

// ParsePolecatDonePayload parses a POLECAT_DONE notification body.
// Unlike formal protocol messages, POLECAT_DONE is a mail convention — no
// required fields are enforced. Returns a best-effort parse of available fields.
func ParsePolecatDonePayload(polecatName, body string) *PolecatDonePayload {
	payload := &PolecatDonePayload{
		Polecat:       polecatName,
		ExitType:      parseField(body, "Exit"),
		Issue:         parseField(body, "Issue"),
		Branch:        parseField(body, "Branch"),
		MR:            parseField(body, "MR"),
		ConvoyID:      parseField(body, "ConvoyID"),
		MergeStrategy: parseField(body, "MergeStrategy"),
		Errors:        parseField(body, "Errors"),
	}

	if parseField(body, "ConvoyOwned") == "true" {
		payload.ConvoyOwned = true
	}

	return payload
}

// protocolFieldKeys are the line-leading field names that appear in the body
// of a protocol message: RECOVERED_BEAD (witness and refinery flavors),
// MERGE_READY, MERGED, MERGE_FAILED, FIX_NEEDED, and REWORK_REQUEST.
var protocolFieldKeys = []string{
	"Bead:", "Polecat:", "Rig:", "Branch:", "Issue:", "Target:",
	"MR:", "MR-Bead-ID:", "Failure-Type:", "Error:", "Attempt:",
	"Attempt-Number:", "Previous Status:", "Respawn Count:",
	"Rejection-Findings:", "Rejection-Summary:", "Rejection-Score:", "Rejection-Unresolved:",
	"ConvoyID:", "SourceIssue:",
	"Merged-At:", "Failed-At:", "Requested-At:", "Verified:",
}

// LooksLikeProtocolPayload reports whether body is structured as a protocol
// message payload rather than human prose.
//
// The test is deliberately structural, not semantic: a body looks like a
// payload when it carries at least two protocol field lines at column 0.
// Indented occurrences do not count, so a reply that quotes a payload (a
// common way to acknowledge one) still reads as prose.
//
// Callers use this to keep protocol payloads from being re-minted by a
// non-protocol path — see the guard in `gt mail reply`. Parsing a body is a
// separate concern (ParseFixNeededPayload and friends) and may still fail
// after this returns true; this only answers "is this shaped like a payload
// at all?".
func LooksLikeProtocolPayload(body string) bool {
	const minFieldLines = 2

	fieldLines := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		// Indented or quoted lines are prose about a payload, not the payload.
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
			strings.HasPrefix(line, ">") || strings.HasPrefix(line, "-") {
			continue
		}
		for _, key := range protocolFieldKeys {
			if strings.HasPrefix(line, key) {
				fieldLines++
				break
			}
		}
	}

	return fieldLines >= minFieldLines
}

// parseField extracts a field value from a key-value body format.
// Format: "Key: value"
func parseField(body, key string) string {
	lines := strings.Split(body, "\n")
	prefix := key + ": "

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}

	return ""
}
