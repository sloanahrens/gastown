// Package protocol recognizes the legacy inter-agent protocol payloads.
//
// The witness, refinery and deacon that exchanged MERGE_READY, MERGED,
// MERGE_FAILED, FIX_NEEDED, REWORK_REQUEST, CONVOY_NEEDS_FEEDING and
// RECOVERED_BEAD mail were deleted, and nothing sends or parses those messages
// now. What remains is the structural check `gt mail reply` uses so a reply on
// an old protocol thread cannot re-mint the payload (gt-8sex).
package protocol

import "strings"

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
// non-protocol path — see the guard in `gt mail reply`.
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
