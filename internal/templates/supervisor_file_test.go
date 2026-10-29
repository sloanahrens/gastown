package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDecodeXMLEntities covers the decoder's two answers: the text of a value
// a renderer escaped, and an error for anything a renderer cannot have
// written. Stripping markup instead of refusing it would let a value that
// never named a town compare as though it did.
func TestDecodeXMLEntities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain text", in: "/towns/live", want: "/towns/live"},
		{name: "escaped specials", in: "/towns/a&amp;b&lt;c&gt;&quot;&apos;", want: `/towns/a&b<c>"'`},
		{name: "numeric reference", in: "/towns/a&#38;b", want: "/towns/a&b"},
		{name: "no text at all", in: "", want: ""},
		{name: "markup", in: "/towns/<i>x</i>", wantErr: true},
		{name: "undefined entity", in: "/towns/&nope;", wantErr: true},
		{name: "truncated entity", in: "/towns/&amp", wantErr: true},
		{name: "a raw ampersand", in: "/towns/a&b", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeXMLEntities(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decodeXMLEntities(%q) = %q, want an error", tc.in, got)
				}
				if got != "" {
					t.Errorf("decodeXMLEntities(%q) = %q with its error, want no value", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeXMLEntities(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("decodeXMLEntities(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSupervisorWorkingDirectory reads both forms the two templates render,
// and reports a value it cannot decode rather than passing the raw text on.
func TestSupervisorWorkingDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{name: "launchd plist", body: "<key>WorkingDirectory</key>\n    <string>/towns/live</string>\n", want: "/towns/live"},
		{name: "escaped plist value", body: "<key>WorkingDirectory</key>\n    <string>/towns/a&amp;b</string>\n", want: "/towns/a&b"},
		{name: "systemd unit", body: "[Service]\nWorkingDirectory=/towns/live\n", want: "/towns/live"},
		{name: "neither form", body: "<plist><dict></dict></plist>\n", want: ""},
		{name: "a plist value holding markup", body: "<key>WorkingDirectory</key>\n    <string>/towns/<i>x</i></string>\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := supervisorWorkingDirectory(tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("supervisorWorkingDirectory = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("supervisorWorkingDirectory: %v", err)
			}
			if got != tc.want {
				t.Errorf("supervisorWorkingDirectory = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSupervisorFileIsFor is the guard install and status read a town out of an
// installed supervisor file with: the town the file was rendered for matches,
// another does not, and a file whose value cannot be read back is an error
// rather than a guess.
func TestSupervisorFileIsFor(t *testing.T) {
	t.Parallel()
	live, other := t.TempDir(), t.TempDir()

	// fileOf writes content to a supervisor file under a temporary directory.
	fileOf := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing the supervisor file: %v", err)
		}
		return path
	}
	// rendered is the plist a host agent install would leave for townRoot.
	rendered := func(t *testing.T, townRoot string) string {
		t.Helper()
		content, err := HostAgentPlist(hostAgentGTPath, townRoot)
		if err != nil {
			t.Fatalf("rendering the plist: %v", err)
		}
		return content
	}

	for _, tc := range []struct {
		name     string
		path     func(t *testing.T) string
		townRoot string
		want     bool
		// wantErr is the reason the file is refused. The two refusals are
		// different answers — no town named, and a value that does not decode —
		// so each test pins the one it drives.
		wantErr string
	}{
		{
			name:     "the town it was rendered for",
			path:     func(t *testing.T) string { return fileOf(t, rendered(t, live)) },
			townRoot: live,
			want:     true,
		},
		{
			name:     "another town",
			path:     func(t *testing.T) string { return fileOf(t, rendered(t, live)) },
			townRoot: other,
			want:     false,
		},
		{
			name:     "a systemd unit",
			path:     func(t *testing.T) string { return fileOf(t, "[Service]\nWorkingDirectory="+live+"\n") },
			townRoot: live,
			want:     true,
		},
		{
			name:     "an absent file",
			path:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.plist") },
			townRoot: live,
			want:     false,
		},
		{
			name:     "a file naming no town",
			path:     func(t *testing.T) string { return fileOf(t, "<plist><dict></dict></plist>\n") },
			townRoot: live,
			wantErr:  "no WorkingDirectory",
		},
		{
			name: "a value that does not decode",
			path: func(t *testing.T) string {
				return fileOf(t, "<key>WorkingDirectory</key>\n<string>/towns/<i>x</i></string>\n")
			},
			townRoot: live,
			wantErr:  "holds markup",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := SupervisorFileIsFor(tc.path(t), tc.townRoot)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("SupervisorFileIsFor = %v, want an error", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SupervisorFileIsFor error = %v, want it to name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SupervisorFileIsFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("SupervisorFileIsFor = %v, want %v", got, tc.want)
			}
		})
	}
}
