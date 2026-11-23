package land

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// checkExemptionFile returns the first fault in the exemption file's text: a
// line parseExemptions rejects, or a `<pkg> <TestName>` key that does not sort
// after the key on the line before it.
func checkExemptionFile(text string) error {
	if _, err := parseExemptions(text); err != nil {
		return err
	}
	prev := ""
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _ := strings.Cut(line, "#")
		key = strings.Join(strings.Fields(key), " ")
		if prev != "" && key <= prev {
			return fmt.Errorf("exemption line %d %q is out of order: it should sort after %q", i+1, line, prev)
		}
		prev = key
	}
	return nil
}

func TestExemptionFileParses(t *testing.T) {
	t.Parallel()
	rows := []struct{ name, text, want string }{
		{"sorted", "# <pkg> <TestName> # <reason>\ninternal/land TestA # walks one fixture\ninternal/land TestB # walks another\n", ""},
		{"no reason", "internal/land TestA # walks one fixture\ninternal/land TestB\ninternal/land TestC # walks a third\n", "internal/land TestB"},
		{"out of order", "internal/land TestB # walks another\ninternal/land TestA # walks one fixture\ninternal/land TestC # walks a third\n", "internal/land TestA"},
	}
	for _, row := range rows {
		err := checkExemptionFile(row.text)
		if row.want == "" && err != nil {
			t.Errorf("%s: checkExemptionFile = %v, want nil", row.name, err)
		} else if row.want != "" && (err == nil || !strings.Contains(err.Error(), row.want)) {
			t.Errorf("%s: checkExemptionFile = %v, want an error naming %q", row.name, err, row.want)
		}
	}
	text, err := os.ReadFile("testdata/unregistered-scanners.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkExemptionFile(string(text)); err != nil {
		t.Errorf("testdata/unregistered-scanners.txt: %v", err)
	}
}
