package deps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBeadsVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"bd version 0.55.4 (dev: main@3e1378e122c6)", "0.55.4"},
		{"bd version 0.55.4", "0.55.4"},
		{"bd version 1.2.3", "1.2.3"},
		{"bd version 10.20.30 (release)", "10.20.30"},
		{"some other output", ""},
		{"", ""},
	}

	for _, tt := range tests {
		result := parseBeadsVersion(tt.input)
		if result != tt.expected {
			t.Errorf("parseBeadsVersion(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b     string
		expected int
	}{
		{"0.55.4", "0.55.4", 0},
		{"0.55.4", "0.54.0", 1},
		{"0.54.0", "0.55.4", -1},
		{"1.0.0", "0.99.99", 1},
		{"0.55.5", "0.55.4", 1},
		{"0.55.4", "0.55.5", -1},
	}

	for _, tt := range tests {
		result := CompareVersions(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestBeadsStatusFromOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		output      string
		err         error
		wantStatus  BeadsStatus
		wantVersion string
	}{
		{"exec error", "bd version 0.60.0", errors.New("exit status 1"), BeadsUnknown, ""},
		{"unparseable", "garbage", nil, BeadsUnknown, ""},
		// No semver floor: an old number is not evidence of anything. The
		// schema/contract handshake decides whether the town may run.
		{"old semver", "bd version 0.1.0", nil, BeadsOK, "0.1.0"},
		{"fork with schema suffix", "bd version 1.2.2 (da4983e: da4983e) schema<=66", nil, BeadsOK, "1.2.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, version := beadsStatusFromOutput([]byte(tt.output), tt.err)
			if status != tt.wantStatus || version != tt.wantVersion {
				t.Errorf("beadsStatusFromOutput(%q, %v) = %d, %q; want %d, %q", tt.output, tt.err, status, version, tt.wantStatus, tt.wantVersion)
			}
		})
	}
}

// TestBeadsErrorForStatus pins that gt never installs bd and never passes
// a bd it could not identify (G3-07, G5-03, B5-03).
func TestBeadsErrorForStatus(t *testing.T) {
	t.Parallel()
	if err := beadsErrorForStatus(BeadsOK); err != nil {
		t.Errorf("BeadsOK: %v", err)
	}
	for _, status := range []BeadsStatus{BeadsNotFound, BeadsUnknown} {
		err := beadsErrorForStatus(status)
		if err == nil {
			t.Errorf("status %d: want error, got nil", status)
			continue
		}
		if !strings.Contains(err.Error(), "make safe-install") {
			t.Errorf("status %d: error lacks the safe-install hint: %v", status, err)
		}
		if strings.Contains(err.Error(), "go install") {
			t.Errorf("status %d: error suggests go install: %v", status, err)
		}
	}
}

// TestNoBDGoInstallAnywhere fails on any non-test source that would install
// or tell someone to install bd with go install: upstream bd@latest over
// the fork migrates or skews production.
func TestNoBDGoInstallAnywhere(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var hits []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "beads/cmd/bd") && (strings.Contains(line, "go install") || strings.Contains(line, "@latest")) {
					hits = append(hits, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("bd go-install path in source:\n%s", strings.Join(hits, "\n"))
	}
}
