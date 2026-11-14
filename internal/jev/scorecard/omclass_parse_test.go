package scorecard

import "testing"

// A line that is not JSON at all is skipped, and the adapter says so.
func TestOMClassParseRefusesANonJSONLine(t *testing.T) {
	t.Parallel()
	a := newOMClassAdapter("unused")
	if _, err := a.Parse([]byte("{not json")); err == nil {
		t.Error("Parse accepted a line that is not JSON")
	}
}
