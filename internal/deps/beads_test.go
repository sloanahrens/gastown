package deps

import (
	"errors"
	"os"
	"path/filepath"
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
		{"too old", "bd version 0.56.9", nil, BeadsTooOld, "0.56.9"},
		{"at minimum", "bd version " + MinBeadsVersion + " (dev: main@abc)", nil, BeadsOK, MinBeadsVersion},
		{"newer", "bd version 1.2.3", nil, BeadsOK, "1.2.3"},
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

func TestAppendGOBIN(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory (e.g. under env -i): the env is returned as-is.
		if got := appendGOBIN([]string{"PATH=/bin"}); len(got) != 1 || got[0] != "PATH=/bin" {
			t.Errorf("appendGOBIN without a home = %v, want it unchanged", got)
		}
		return
	}
	want := "GOBIN=" + filepath.Join(home, ".local", "bin")

	got := appendGOBIN([]string{"PATH=/bin"})
	if len(got) != 2 || got[0] != "PATH=/bin" || got[1] != want {
		t.Errorf("appendGOBIN without GOBIN = %v, want [PATH=/bin %s]", got, want)
	}
	got = appendGOBIN([]string{"GOBIN=/elsewhere", "PATH=/bin"})
	if len(got) != 2 || got[0] != want || got[1] != "PATH=/bin" {
		t.Errorf("appendGOBIN with GOBIN = %v, want [%s PATH=/bin]", got, want)
	}
}
