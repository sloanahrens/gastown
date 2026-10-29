package templates

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// hostAgentGTPath and hostAgentTownRoot are what testdata/host-agent.golden.plist
// was rendered from.
const (
	hostAgentGTPath   = "/usr/local/bin/gt"
	hostAgentTownRoot = "/towns/live"
)

// TestHostAgentPlist_Golden pins the whole file byte for byte. The plist is the
// job — there is no shell wrapper and nothing else defines it — so a stray line
// here is a launchd key nobody reviewed.
func TestHostAgentPlist_Golden(t *testing.T) {
	t.Parallel()
	got, err := HostAgentPlist(hostAgentGTPath, hostAgentTownRoot)
	if err != nil {
		t.Fatalf("HostAgentPlist: %v", err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "host-agent.golden.plist"))
	if err != nil {
		t.Fatalf("reading the golden file: %v", err)
	}
	if got != string(golden) {
		t.Errorf("rendered plist differs from testdata/host-agent.golden.plist\ngot:\n%s\nwant:\n%s", got, golden)
	}
}

// TestHostAgentPlist_RunsTheAgentForItsTown checks the job the plist names:
// this label, this gt binary, the agent's own subcommand and the town it
// writes for.
func TestHostAgentPlist_RunsTheAgentForItsTown(t *testing.T) {
	t.Parallel()
	got, err := HostAgentPlist("/opt/gt/bin/gt", "/towns/scratch")
	if err != nil {
		t.Fatalf("HostAgentPlist: %v", err)
	}
	for _, want := range []string{
		"<string>" + HostAgentLabel + "</string>",
		"<string>/opt/gt/bin/gt</string>",
		"<string>host</string>",
		"<string>agent</string>",
		"<string>run</string>",
		"<key>WorkingDirectory</key>\n    <string>/towns/scratch</string>",
		"<key>GT_TOWN_ROOT</key>\n        <string>/towns/scratch</string>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered plist missing %q:\n%s", want, got)
		}
	}
}

// TestHostAgentPlist_EscapesTheValuesItWritesIntoXML covers a town path that
// holds XML syntax: the plist has to parse, carry the path as its text, and
// not grow a key the path's author wrote into it.
func TestHostAgentPlist_EscapesTheValuesItWritesIntoXML(t *testing.T) {
	t.Parallel()
	const townRoot = "/towns/a&b<c"
	got, err := HostAgentPlist(hostAgentGTPath, townRoot)
	if err != nil {
		t.Fatalf("HostAgentPlist: %v", err)
	}
	if !strings.Contains(got, "<string>/towns/a&amp;b&lt;c</string>") {
		t.Errorf("rendered plist does not carry the escaped town root:\n%s", got)
	}
	if strings.Contains(got, "<c</string>") {
		t.Errorf("rendered plist carries the raw town root, which XML would misread:\n%s", got)
	}
	assertWellFormedXML(t, got)

	// The value read back out of the file is the path that went in, so a town
	// whose path holds & still matches its own plist.
	escaped, err := HostAgentPlist(hostAgentGTPath, "/towns/a&b")
	if err != nil {
		t.Fatalf("HostAgentPlist: %v", err)
	}
	got, err = supervisorWorkingDirectory(escaped)
	if err != nil {
		t.Fatalf("supervisorWorkingDirectory read back an error: %v", err)
	}
	if got != "/towns/a&b" {
		t.Errorf("supervisorWorkingDirectory read back %q, want /towns/a&b", got)
	}
}

// assertWellFormedXML reads s to its end as XML, which is the property the
// escaping exists for: a plist launchd cannot parse starts nothing.
func assertWellFormedXML(t *testing.T, s string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		if _, err := dec.Token(); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("rendered plist is not well-formed XML: %v", err)
		}
	}
}

// TestHostAgentPlist_LogsBesideTheProbeFile pins the log path to the probe
// file's own directory: the two cannot drift apart because only hostprobe.Path
// spells that directory out.
func TestHostAgentPlist_LogsBesideTheProbeFile(t *testing.T) {
	t.Parallel()
	got, err := HostAgentPlist(hostAgentGTPath, hostAgentTownRoot)
	if err != nil {
		t.Fatalf("HostAgentPlist: %v", err)
	}
	wantLog := filepath.Join(filepath.Dir(hostprobe.Path(hostAgentTownRoot)), "agent.log")
	if !strings.Contains(got, "<string>"+wantLog+"</string>") {
		t.Errorf("rendered plist does not log to %q:\n%s", wantLog, got)
	}
}

// TestHostAgentPlist_RefusesAnEmptyInput drives both refusal branches: a job
// with no binary to run, and one that writes in no town.
func TestHostAgentPlist_RefusesAnEmptyInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		gtPath   string
		townRoot string
	}{
		{name: "no gt path", gtPath: "", townRoot: hostAgentTownRoot},
		{name: "no town root", gtPath: hostAgentGTPath, townRoot: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := HostAgentPlist(tc.gtPath, tc.townRoot); err == nil {
				t.Errorf("HostAgentPlist(%q, %q) rendered a plist, want a refusal:\n%s", tc.gtPath, tc.townRoot, got)
			}
		})
	}
}

// TestRenderHostAgentPlist_Failures drives the three ways a render can fail
// against the embedded file: the template it names is missing, the template
// does not parse, and the data does not fit it. The town and binary both
// refused above are the fourth and fifth, and the golden file is the path
// where nothing fails.
func TestRenderHostAgentPlist_Failures(t *testing.T) {
	t.Parallel()
	data := HostAgentData{GTPath: hostAgentGTPath, TownRoot: hostAgentTownRoot}
	for _, tc := range []struct {
		name string
		fsys fstest.MapFS
	}{
		{name: "template missing", fsys: fstest.MapFS{}},
		{name: "template does not parse", fsys: fstest.MapFS{
			hostAgentTemplate: {Data: []byte("<plist>{{")},
		}},
		{name: "template wants a field the data lacks", fsys: fstest.MapFS{
			hostAgentTemplate: {Data: []byte("{{.NoSuchField}}")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := renderHostAgentPlist(tc.fsys, data); err == nil {
				t.Errorf("renderHostAgentPlist = %q, want an error", got)
			}
		})
	}
}

// TestRenderHostAgentPlist_RendersTheGivenTemplate is the success path of the
// seam the failure cases go through, so a renderer that read nothing would not
// pass on the golden file alone.
func TestRenderHostAgentPlist_RendersTheGivenTemplate(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{hostAgentTemplate: {Data: []byte("{{.GTPath}} in {{.TownRoot}}")}}
	got, err := renderHostAgentPlist(fsys, HostAgentData{GTPath: "/opt/gt", TownRoot: "/towns/live"})
	if err != nil {
		t.Fatalf("renderHostAgentPlist: %v", err)
	}
	if want := "/opt/gt in /towns/live"; got != want {
		t.Errorf("renderHostAgentPlist = %q, want %q", got, want)
	}
}

// TestHostAgentPlistPath_IsTheLaunchAgentsJob pins where install writes: the
// label's own file under the user's LaunchAgents directory. The home is the
// seam's, so the test does not read the machine it runs on.
func TestHostAgentPlistPath_IsTheLaunchAgentsJob(t *testing.T) {
	t.Parallel()
	h := supervisorHost{homeDir: func() (string, error) { return "/Users/toast", nil }}
	path, err := h.hostAgentPlistPath()
	if err != nil {
		t.Fatalf("hostAgentPlistPath: %v", err)
	}
	want := filepath.Join("/Users/toast", "Library", "LaunchAgents", HostAgentLabel+".plist")
	if path != want {
		t.Errorf("hostAgentPlistPath() = %q, want %q", path, want)
	}
}

// TestHostAgentPlistPath_RefusesNoHome covers the host seam: a host whose home
// directory cannot be found has no LaunchAgents directory to name.
func TestHostAgentPlistPath_RefusesNoHome(t *testing.T) {
	t.Parallel()
	h := supervisorHost{homeDir: func() (string, error) { return "", errors.New("no home") }}
	if got, err := h.hostAgentPlistPath(); err == nil {
		t.Errorf("hostAgentPlistPath = %q with no home, want an error", got)
	}
}
