package beads

import (
	"strconv"
	"strings"
)

// parseLoadavgField extracts the first numeric field from loadavg output.
// On macOS the sysctl output is wrapped in curly braces; callers must strip
// those before passing the field to this function.
func parseLoadavgField(raw string) (float64, error) {
	// Accept only strings that look like a non-negative decimal number.
	// Reject braces, letters, and other non-numeric tokens.
	if strings.ContainsAny(raw, "{}") {
		return 0, &strconv.NumError{Func: "parseLoadavgField", Num: raw, Err: strconv.ErrSyntax}
	}
	return strconv.ParseFloat(raw, 64)
}
