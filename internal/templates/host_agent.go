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
// townRoot, run by gtPath.
//
// The plist is the job: there is no shell wrapper and nothing else defines it,
// so an empty gt path or town root would render a job that runs nothing in no
// town, and both are refused rather than written.
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
		// hostprobe.Path is the one place the probe file's location is
		// written down, so the log sits beside it rather than under a second
		// copy of the same directory in the template.
		LogPath: filepath.Join(filepath.Dir(hostprobe.Path(townRoot)), "agent.log"),
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
