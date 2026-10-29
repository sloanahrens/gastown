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
		{name: "CDATA", in: "/towns/<![CDATA[a&b]]>", wantErr: true},
		{name: "a comment", in: "/towns/<!-- x -->", wantErr: true},
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

// TestSupervisorWorkingDirectory reads both forms the two templates render and
// returns the value as written, with whether it came from a plist (whose text a
// renderer XML-escapes) or from a unit (a literal path).
func TestSupervisorWorkingDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		body    string
		want    string
		wantXML bool
	}{
		{name: "launchd plist", body: "<key>WorkingDirectory</key>\n    <string>/towns/live</string>\n", want: "/towns/live", wantXML: true},
		{name: "escaped plist value, as written", body: "<key>WorkingDirectory</key>\n    <string>/towns/a&amp;b</string>\n", want: "/towns/a&amp;b", wantXML: true},
		{name: "systemd unit", body: "[Service]\nWorkingDirectory=/towns/live\n", want: "/towns/live"},
		{name: "a unit's literal entity text", body: "[Service]\nWorkingDirectory=/towns/a&amp;b\n", want: "/towns/a&amp;b"},
		{name: "neither form", body: "<plist><dict></dict></plist>\n", want: ""},
		{name: "a plist value holding markup, as written", body: "<key>WorkingDirectory</key>\n    <string>/towns/<i>x</i></string>\n", want: "/towns/<i>x</i>", wantXML: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, xmlText := supervisorWorkingDirectory(tc.body)
			if got != tc.want || xmlText != tc.wantXML {
				t.Errorf("supervisorWorkingDirectory = (%q, %v), want (%q, %v)", got, xmlText, tc.want, tc.wantXML)
			}
		})
	}
}

// TestSupervisorFileIsFor is the guard install, status and the upgrade repair
// read a town out of an installed supervisor file with: the town the file was
// rendered for matches, another does not, and a file that names no town is an
// error. The answer for an existing file never becomes an error because of how
// its value is spelled: a plist written with the path unescaped still matches
// its town (the repair exists to rewrite it), an escaped path matches too, and
// a value that is neither the town nor decodable is a plain no.
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
		// wantErr is the reason the file is refused: no town named.
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
			name:     "a path holding & matches its escaped plist",
			path:     func(t *testing.T) string { return fileOf(t, rendered(t, "/towns/a&b")) },
			townRoot: "/towns/a&b",
			want:     true,
		},
		{
			name:     "an escaped plist is not another town's",
			path:     func(t *testing.T) string { return fileOf(t, rendered(t, "/towns/a&b")) },
			townRoot: "/towns/a",
			want:     false,
		},
		{
			// A file an older renderer wrote with the path raw. Its answer was
			// true before decoding existed and must stay true: the upgrade
			// repair finds the file through this guard to rewrite it.
			name: "an existing plist with the path unescaped still matches",
			path: func(t *testing.T) string {
				return fileOf(t, "<key>WorkingDirectory</key>\n<string>/towns/a&b</string>\n")
			},
			townRoot: "/towns/a&b",
			want:     true,
		},
		{
			name: "a value that does not decode is a plain no, not an error",
			path: func(t *testing.T) string {
				return fileOf(t, "<key>WorkingDirectory</key>\n<string>/towns/<i>x</i></string>\n")
			},
			townRoot: live,
			want:     false,
		},
		{
			// Only a plist's text is XML-escaped: a unit's path is literal, so
			// decoding it would match a town the unit does not serve.
			name:     "a unit's literal entity text is not decoded",
			path:     func(t *testing.T) string { return fileOf(t, "[Service]\nWorkingDirectory=/towns/a&amp;b\n") },
			townRoot: "/towns/a&b",
			want:     false,
		},
		{
			name:     "a unit's literal entity text matches its own spelling",
			path:     func(t *testing.T) string { return fileOf(t, "[Service]\nWorkingDirectory=/towns/a&amp;b\n") },
			townRoot: "/towns/a&amp;b",
			want:     true,
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
