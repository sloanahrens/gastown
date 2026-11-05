package templates

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// HostAgentLabel is the launchd job label the host agent's plist carries. It is
// a job of its own, not part of the daemon's: the agent runs on the Mac while
// the daemon may run in a container.
const HostAgentLabel = "com.gastown.host-agent"

// hostAgentPlistName is the file that label is installed as, under
// ~/Library/LaunchAgents.
const hostAgentPlistName = HostAgentLabel + ".plist"

// hostAgentTemplate is the embedded plist source. The launchd directory's
// files are named for the label they carry, as com.gastown.daemon.plist is.
const hostAgentTemplate = "launchd/" + HostAgentLabel + ".plist"

// HostAgentData is what the host agent's plist renders from.
type HostAgentData struct {
	// GTPath is the gt binary launchd runs, as `gt host agent run`.
	GTPath string
	// TownRoot is the town whose .runtime/host/probes.json the agent writes.
	TownRoot string
	// LogPath is where launchd opens the job's log. It is the probe file's own
	// directory, so the agent and the log it writes share one place; the
	// template does not spell that directory out again.
	LogPath string
}

// HostAgentPlist renders the host agent's launchd plist for the town at
// townRoot, run by gtPath. An empty gt path or town root is refused rather
// than rendered: the plist defines the whole job, so it would name a program
// that runs nothing, in no town.
func HostAgentPlist(gtPath, townRoot string) (string, error) {
	if gtPath == "" {
		return "", errors.New("render host agent plist: no gt path")
	}
	if townRoot == "" {
		return "", errors.New("render host agent plist: no town root")
	}
	return renderHostAgentPlist(supervisorFS, HostAgentData{
		GTPath:   gtPath,
		TownRoot: townRoot,
		LogPath:  filepath.Join(filepath.Dir(hostprobe.Path(townRoot)), "agent.log"),
	})
}

// renderHostAgentPlist renders data from the template in fsys. fsys is a
// parameter so a test drives the read, parse and execute failures with a
// template of its own; production passes the embedded files.
func renderHostAgentPlist(fsys fs.FS, data HostAgentData) (string, error) {
	raw, err := fs.ReadFile(fsys, hostAgentTemplate)
	if err != nil {
		return "", fmt.Errorf("reading the host agent plist template: %w", err)
	}
	tmpl, err := template.New("host-agent").Funcs(template.FuncMap{"xml": xmlText}).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parsing the host agent plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering the host agent plist template: %w", err)
	}
	return buf.String(), nil
}

// xmlText escapes s for XML character data. The template writes paths straight
// into the plist, and a town whose path holds & or < would otherwise render a
// file launchd cannot parse — or, with a path crafted as </string><key>...,
// one that carries keys nobody wrote. Every value the template interpolates
// goes through this.
func xmlText(s string) string {
	var buf strings.Builder
	// EscapeText only reports its writer's failure, and strings.Builder's
	// Write never fails, so this escape has no error to carry back.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// MacMaintLabelPrefix is the label prefix of the daily jobs that run one
// maintenance target from the Mac.
const MacMaintLabelPrefix = "com.gastown.host-maint-"

// MacMaintJob is one daily job `gt host agent install` writes beside the host
// agent's: a maintenance target the container daemon cannot run (the nodes
// target needs the Mac's colima docker context), applied once a day from the
// Mac where that context exists.
type MacMaintJob struct {
	// LabelSuffix names the job and its plist: the label is
	// MacMaintLabelPrefix + LabelSuffix.
	LabelSuffix string
	// Target is the gt maint target the job applies, e.g. "nodes".
	Target string
	// Hour and Minute are the job's StartCalendarInterval, in local time.
	Hour   int
	Minute int
}

// macMaintJobs is the table install renders. One row is one Mac-only target
// and the time it runs; gt-gg9hv.24.5 adds the next row.
var macMaintJobs = []MacMaintJob{
	{LabelSuffix: "nodes", Target: "nodes", Hour: 5, Minute: 30},
}

// MacMaintJobs returns the table's rows. It is a copy, so a caller cannot
// change what a later install renders.
func MacMaintJobs() []MacMaintJob {
	return append([]MacMaintJob(nil), macMaintJobs...)
}

// Label is the launchd label the row's plist carries.
func (j MacMaintJob) Label() string { return MacMaintLabelPrefix + j.LabelSuffix }

// PlistName is the file the row's job is installed as.
func (j MacMaintJob) PlistName() string { return j.Label() + ".plist" }

// validate refuses a row that would render a job launchd cannot run: a label
// suffix that is not a plain file name, no maint target to apply, or a time
// outside a day.
func (j MacMaintJob) validate() error {
	if j.LabelSuffix == "" || j.LabelSuffix == "." || j.LabelSuffix == ".." || filepath.Base(j.LabelSuffix) != j.LabelSuffix {
		return fmt.Errorf("host maint job: label suffix %q is not a plain name", j.LabelSuffix)
	}
	if j.Target == "" {
		return errors.New("host maint job: no maint target")
	}
	if j.Hour < 0 || j.Hour > 23 {
		return fmt.Errorf("host maint job: hour %d is outside 0-23", j.Hour)
	}
	if j.Minute < 0 || j.Minute > 59 {
		return fmt.Errorf("host maint job: minute %d is outside 0-59", j.Minute)
	}
	return nil
}

// MacMaintJobData is what a daily maint job's plist renders from.
type MacMaintJobData struct {
	// GTPath is the gt binary the job runs, as `gt maint apply <Target>`.
	GTPath string
	// TownRoot is the town the job maintains.
	TownRoot string
	// Label is the row's launchd label.
	Label string
	// Target is the gt maint target the job applies.
	Target string
	// Hour and Minute are its StartCalendarInterval, in local time.
	Hour   int
	Minute int
	// LogPath is where launchd opens the job's log, under the town's host
	// directory, in a file of the job's own.
	LogPath string
}

// macMaintJobTemplate is the embedded plist source every row renders through.
const macMaintJobTemplate = "launchd/com.gastown.host-maint.plist"

// MacMaintJobPlist renders the daily plist for one row, run by gtPath for the
// town at townRoot. An empty gt path or town root, or a row that would name no
// job, no target or no time, is refused rather than rendered: the plist
// defines the whole job, so it would run nothing, or at no time.
func MacMaintJobPlist(gtPath, townRoot string, job MacMaintJob) (string, error) {
	if gtPath == "" {
		return "", errors.New("render host maint job plist: no gt path")
	}
	if townRoot == "" {
		return "", errors.New("render host maint job plist: no town root")
	}
	if err := job.validate(); err != nil {
		return "", fmt.Errorf("render host maint job plist: %w", err)
	}
	return renderMacMaintJobPlist(supervisorFS, MacMaintJobData{
		GTPath:   gtPath,
		TownRoot: townRoot,
		Label:    job.Label(),
		Target:   job.Target,
		Hour:     job.Hour,
		Minute:   job.Minute,
		LogPath:  filepath.Join(filepath.Dir(hostprobe.Path(townRoot)), "maint-"+job.LabelSuffix+".log"),
	})
}

// renderMacMaintJobPlist renders data from the template in fsys. fsys is a
// parameter so a test drives the read, parse and execute failures with a
// template of its own, as renderHostAgentPlist's does.
func renderMacMaintJobPlist(fsys fs.FS, data MacMaintJobData) (string, error) {
	raw, err := fs.ReadFile(fsys, macMaintJobTemplate)
	if err != nil {
		return "", fmt.Errorf("reading the host maint job plist template: %w", err)
	}
	tmpl, err := template.New("host-maint-job").Funcs(template.FuncMap{"xml": xmlText}).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parsing the host maint job plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering the host maint job plist template: %w", err)
	}
	return buf.String(), nil
}

// HostAgentPlistPath returns where the host agent's plist is installed:
// ~/Library/LaunchAgents/com.gastown.host-agent.plist.
func HostAgentPlistPath() (string, error) {
	return realHost.hostAgentPlistPath()
}

// hostAgentPlistPath is HostAgentPlistPath for the host h describes. It is the
// host seam a test reads a home-directory failure through.
func (h supervisorHost) hostAgentPlistPath() (string, error) {
	homeDir, err := h.homeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	return filepath.Join(homeDir, "Library", "LaunchAgents", hostAgentPlistName), nil
}
