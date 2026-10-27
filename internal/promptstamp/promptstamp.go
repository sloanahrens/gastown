// Package promptstamp is the one rule for the comment a sandbox spawn posts on
// its work bead naming the stored prompt its polecat ran (gt-2lu00): the spawn
// writes it when it stores the prompt, and the landing worker reads it back
// when it records the review (gt-b9j3o.16). A writer and a reader over the
// same literal share Format and Parse rather than a second copy of it.
package promptstamp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// HashLen is how much of the prompt's sha256 a stamp names: the first 12 hex
// digits.
const HashLen = 12

// stampRE matches a whole comment Format wrote, and nothing a note says about
// a prompt: a rejection note quoting a stamp names no prompt. It is anchored
// and built from HashLen, so the reader cannot drift from the writer.
var stampRE = regexp.MustCompile(`^prompt ([0-9a-f]{` + strconv.Itoa(HashLen) + `}) \([0-9]+ bytes\)$`)

// Format returns the comment text for a stored prompt: its sha256, truncated
// to HashLen hex digits, and its size in bytes.
func Format(hash string, size int) string {
	if len(hash) > HashLen {
		hash = hash[:HashLen]
	}
	return fmt.Sprintf("prompt %s (%d bytes)", hash, size)
}

// Author returns the comment author a spawn stamps under: the actor of the
// polecat seat it spawns, which is the seat the work's branch names.
func Author(rig, seat string) string {
	return rig + "/polecats/" + seat
}

// Parse returns the sha256 a stamp names, and false when text is not a stamp
// Format wrote. Surrounding whitespace is tolerated, since a bead's store may
// keep a trailing newline; text with anything else around the stamp is not a
// stamp.
func Parse(text string) (string, bool) {
	m := stampRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", false
	}
	return m[1], true
}
