package deps

import (
	"errors"
	"testing"
)

func TestParseDoltVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"dolt version 1.82.4", "1.82.4"},
		{"dolt version 1.82.4\n", "1.82.4"},
		{"dolt version 1.84.0", "1.84.0"},
		{"dolt version 2.0.3", "2.0.3"},
		{"dolt version 2.0.7", "2.0.7"},
		{"dolt version 1.84.0\nWarning: you are on an old version of Dolt. The newest version is 2.0.3.", "1.84.0"},
		{"dolt version 1.0.0", "1.0.0"},
		{"dolt version 10.20.30", "10.20.30"},
		{"some other output", ""},
		{"", ""},
	}

	for _, tt := range tests {
		result := parseDoltVersion(tt.input)
		if result != tt.expected {
			t.Errorf("parseDoltVersion(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestDoltStatusFromOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		output      string
		err         error
		wantStatus  DoltStatus
		wantVersion string
		wantDetail  string
	}{
		{"exec error with output", "permission denied\n", errors.New("exit status 1"), DoltExecFailed, "", "at /x/dolt: permission denied"},
		{"exec error without output", "", errors.New("exit status 2"), DoltExecFailed, "", "at /x/dolt: exit status 2"},
		{"unparseable", "  garbage\n", nil, DoltUnknown, "", "garbage"},
		{"too old", "dolt version 2.0.6", nil, DoltTooOld, "2.0.6", ""},
		{"at minimum", "dolt version " + MinDoltVersion + "\n", nil, DoltOK, MinDoltVersion, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, version, detail := doltStatusFromOutput("/x/dolt", []byte(tt.output), tt.err)
			if status != tt.wantStatus || version != tt.wantVersion || detail != tt.wantDetail {
				t.Errorf("doltStatusFromOutput(%q, %v) = %d, %q, %q; want %d, %q, %q", tt.output, tt.err, status, version, detail, tt.wantStatus, tt.wantVersion, tt.wantDetail)
			}
		})
	}
}

func TestMinDoltVersionBoundary(t *testing.T) {
	t.Parallel()
	if CompareVersions("2.0.6", MinDoltVersion) >= 0 {
		t.Fatalf("2.0.6 should be below MinDoltVersion %s", MinDoltVersion)
	}
	if CompareVersions("2.0.7", MinDoltVersion) != 0 {
		t.Fatalf("2.0.7 should equal MinDoltVersion %s", MinDoltVersion)
	}
	if CompareVersions("2.0.8", MinDoltVersion) <= 0 {
		t.Fatalf("2.0.8 should be above MinDoltVersion %s", MinDoltVersion)
	}
}
