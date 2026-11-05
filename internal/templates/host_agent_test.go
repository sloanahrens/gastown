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
	written, xmlText := supervisorWorkingDirectory(escaped)
	if !xmlText {
		t.Errorf("supervisorWorkingDirectory read a plist as a unit's literal text")
	}
	got, err = decodeXMLEntities(written)
	if err != nil {
		t.Fatalf("decodeXMLEntities read back an error: %v", err)
	}
	if got != "/towns/a&b" {
		t.Errorf("the value read back and decoded is %q, want /towns/a&b", got)
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

// macMaintJob returns the table's row for a label suffix, failing the test
// when the table has none: a test that indexes the table would pass on the
// wrong row after a row is added.
func macMaintJob(t *testing.T, suffix string) MacMaintJob {
	t.Helper()
	for _, job := range MacMaintJobs() {
		if job.LabelSuffix == suffix {
			return job
		}
	}
	t.Fatalf("MacMaintJobs() has no %q row", suffix)
	return MacMaintJob{}
}

// TestMacMaintJobPlistMatchesTheGolden pins the whole file byte for byte, as
// the host agent's golden does: the plist is the job, so a stray line here is
// a launchd key nobody reviewed.
func TestMacMaintJobPlistMatchesTheGolden(t *testing.T) {
	t.Parallel()
	got, err := MacMaintJobPlist(hostAgentGTPath, hostAgentTownRoot, macMaintJob(t, "nodes"))
	if err != nil {
		t.Fatalf("MacMaintJobPlist: %v", err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "host-maint-job.golden.plist"))
	if err != nil {
		t.Fatalf("reading the golden file: %v", err)
	}
	if got != string(golden) {
		t.Errorf("rendered plist differs from testdata/host-maint-job.golden.plist\ngot:\n%s\nwant:\n%s", got, golden)
	}
}

// TestMacMaintJobPlist_RunsTheJobForItsTarget checks what the row names: this
// binary, the maint apply subcommand, this target, the row's local time and a
// log under the town's host directory. A daily job is not a daemon, so it has
// neither RunAtLoad nor KeepAlive.
func TestMacMaintJobPlist_RunsTheJobForItsTarget(t *testing.T) {
	t.Parallel()
	got, err := MacMaintJobPlist("/opt/gt/bin/gt", "/towns/scratch", macMaintJob(t, "nodes"))
	if err != nil {
		t.Fatalf("MacMaintJobPlist: %v", err)
	}
	for _, want := range []string{
		"<string>" + MacMaintLabelPrefix + "nodes</string>",
		"<string>/opt/gt/bin/gt</string>",
		"<string>maint</string>\n        <string>apply</string>\n        <string>nodes</string>",
		"<key>WorkingDirectory</key>\n    <string>/towns/scratch</string>",
		"<key>StartCalendarInterval</key>",
		"<key>Hour</key>\n        <integer>5</integer>",
		"<key>Minute</key>\n        <integer>30</integer>",
		"<string>/towns/scratch/.runtime/host/maint-nodes.log</string>",
		"<key>GT_TOWN_ROOT</key>\n        <string>/towns/scratch</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered plist missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"<key>RunAtLoad</key>", "<key>KeepAlive</key>"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("daily plist carries %s, which makes it a daemon:\n%s", unwanted, got)
		}
	}
}

// TestMacMaintJobPlist_SecondRowRendersItsOwnLabelAndTarget is the table's
// contract for the next row: the same template gives it its own label, target
// and time rather than the first row's.
func TestMacMaintJobPlist_SecondRowRendersItsOwnLabelAndTarget(t *testing.T) {
	t.Parallel()
	job := MacMaintJob{LabelSuffix: "registry", Target: "colima", Hour: 6, Minute: 5}
	got, err := MacMaintJobPlist(hostAgentGTPath, hostAgentTownRoot, job)
	if err != nil {
		t.Fatalf("MacMaintJobPlist: %v", err)
	}
	for _, want := range []string{
		"<string>" + MacMaintLabelPrefix + "registry</string>",
		"<string>apply</string>\n        <string>colima</string>",
		"<integer>6</integer>",
		"<integer>5</integer>",
		"/towns/live/.runtime/host/maint-registry.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered plist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, ">nodes<") {
		t.Errorf("the second row's plist carries the first row's target:\n%s", got)
	}
}

// TestMacMaintJobs_NodesRunsAt0530 pins the schedule the operator decided on:
// after the daemon patrol's 04:30 window.
func TestMacMaintJobs_NodesRunsAt0530(t *testing.T) {
	t.Parallel()
	job := macMaintJob(t, "nodes")
	if job.Target != "nodes" {
		t.Errorf("the nodes row applies target %q, want nodes", job.Target)
	}
	if job.Hour != 5 || job.Minute != 30 {
		t.Errorf("the nodes row starts at %02d:%02d, want 05:30", job.Hour, job.Minute)
	}
}

// TestMacMaintJobsIncludeRegistryCollect pins the registry collect's row: the
// Mac applies registry-collect at 05:45, a quarter hour after the nodes prune,
// and the nodes row keeps its own time.
func TestMacMaintJobsIncludeRegistryCollect(t *testing.T) {
	t.Parallel()
	job := macMaintJob(t, "registry-collect")
	if job.Target != "registry-collect" {
		t.Errorf("the registry-collect row applies target %q, want registry-collect", job.Target)
	}
	if job.Hour != 5 || job.Minute != 45 {
		t.Errorf("the registry-collect row starts at %02d:%02d, want 05:45", job.Hour, job.Minute)
	}
	if nodes := macMaintJob(t, "nodes"); nodes.Target != "nodes" || nodes.Hour != 5 || nodes.Minute != 30 {
		t.Errorf("the nodes row is %s at %02d:%02d, want nodes at 05:30", nodes.Target, nodes.Hour, nodes.Minute)
	}
}

// TestMacMaintJobPlist_EscapesTheValuesItWritesIntoXML covers a town path that
// holds XML syntax: the plist has to parse and carry the path as its text.
func TestMacMaintJobPlist_EscapesTheValuesItWritesIntoXML(t *testing.T) {
	t.Parallel()
	const townRoot = "/towns/a&b<c"
	got, err := MacMaintJobPlist(hostAgentGTPath, townRoot, macMaintJob(t, "nodes"))
	if err != nil {
		t.Fatalf("MacMaintJobPlist: %v", err)
	}
	if !strings.Contains(got, "<string>/towns/a&amp;b&lt;c</string>") {
		t.Errorf("rendered plist does not carry the escaped town root:\n%s", got)
	}
	if strings.Contains(got, "<c</string>") {
		t.Errorf("rendered plist carries the raw town root, which XML would misread:\n%s", got)
	}
	assertWellFormedXML(t, got)
}

// TestMacMaintJobPlist_RefusesAJobThatNamesNothing drives every refusal: no
// binary to run, no town to maintain, a label that is not a plain name, no
// target, and a time outside a day.
func TestMacMaintJobPlist_RefusesAJobThatNamesNothing(t *testing.T) {
	t.Parallel()
	good := macMaintJob(t, "nodes")
	for _, tc := range []struct {
		name     string
		gtPath   string
		townRoot string
		job      MacMaintJob
	}{
		{name: "no gt path", gtPath: "", townRoot: hostAgentTownRoot, job: good},
		{name: "no town root", gtPath: hostAgentGTPath, townRoot: "", job: good},
		{name: "no label suffix", gtPath: hostAgentGTPath, townRoot: hostAgentTownRoot,
			job: MacMaintJob{LabelSuffix: "", Target: "nodes", Hour: 5, Minute: 30}},
		{name: "label suffix is a path", gtPath: hostAgentGTPath, townRoot: hostAgentTownRoot,
			job: MacMaintJob{LabelSuffix: "../escape", Target: "nodes", Hour: 5, Minute: 30}},
		{name: "no maint target", gtPath: hostAgentGTPath, townRoot: hostAgentTownRoot,
			job: MacMaintJob{LabelSuffix: "nodes", Target: "", Hour: 5, Minute: 30}},
		{name: "hour before the day", gtPath: hostAgentGTPath, townRoot: hostAgentTownRoot,
			job: MacMaintJob{LabelSuffix: "nodes", Target: "nodes", Hour: -1, Minute: 30}},
		{name: "hour after the day", gtPath: hostAgentGTPath, townRoot: hostAgentTownRoot,
			job: MacMaintJob{LabelSuffix: "nodes", Target: "nodes", Hour: 24, Minute: 30}},
		{name: "minute before the hour", gtPath: hostAgentGTPath, townRoot: hostAgentTownRoot,
			job: MacMaintJob{LabelSuffix: "nodes", Target: "nodes", Hour: 5, Minute: -1}},
		{name: "minute after the hour", gtPath: hostAgentGTPath, townRoot: hostAgentTownRoot,
			job: MacMaintJob{LabelSuffix: "nodes", Target: "nodes", Hour: 5, Minute: 60}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := MacMaintJobPlist(tc.gtPath, tc.townRoot, tc.job); err == nil {
				t.Errorf("MacMaintJobPlist(%q, %q, %+v) rendered a plist, want a refusal:\n%s", tc.gtPath, tc.townRoot, tc.job, got)
			}
		})
	}
}

// TestRenderMacMaintJobPlist_Failures drives the three ways a render can fail
// against the embedded file, as the host agent's renderer test does.
func TestRenderMacMaintJobPlist_Failures(t *testing.T) {
	t.Parallel()
	data := MacMaintJobData{GTPath: hostAgentGTPath, TownRoot: hostAgentTownRoot, Label: "com.gastown.host-maint-nodes", Target: "nodes", Hour: 5, Minute: 30}
	for _, tc := range []struct {
		name string
		fsys fstest.MapFS
	}{
		{name: "template missing", fsys: fstest.MapFS{}},
		{name: "template does not parse", fsys: fstest.MapFS{
			macMaintJobTemplate: {Data: []byte("<plist>{{")},
		}},
		{name: "template wants a field the data lacks", fsys: fstest.MapFS{
			macMaintJobTemplate: {Data: []byte("{{.NoSuchField}}")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := renderMacMaintJobPlist(tc.fsys, data); err == nil {
				t.Errorf("renderMacMaintJobPlist = %q, want an error", got)
			}
		})
	}
}

// TestRenderMacMaintJobPlist_RendersTheGivenTemplate is the success path of the
// seam the failure cases go through.
func TestRenderMacMaintJobPlist_RendersTheGivenTemplate(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{macMaintJobTemplate: {Data: []byte("{{.GTPath}} {{.Target}} {{.Hour}}:{{.Minute}}")}}
	got, err := renderMacMaintJobPlist(fsys, MacMaintJobData{GTPath: "/opt/gt", Target: "nodes", Hour: 5, Minute: 30})
	if err != nil {
		t.Fatalf("renderMacMaintJobPlist: %v", err)
	}
	if want := "/opt/gt nodes 5:30"; got != want {
		t.Errorf("renderMacMaintJobPlist = %q, want %q", got, want)
	}
}
