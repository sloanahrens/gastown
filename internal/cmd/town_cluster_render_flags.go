package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// renderFlagsRelPath is where a live render leaves its flags, relative to the
// town's gastown directory: beside imagevalues' values file, so both describe
// the same running pair. The town's own gastown directory is not the tracked
// repo, so no host path (a config directory, a URL) lands in a file git sees.
const renderFlagsRelPath = "deploy/values/render-flags.json"

// renderFlagsPathIn is the recorded-flags file's path under townRoot, the one
// place its location is resolved: the render writes it and the replay reads it
// through this function, so the two cannot disagree, as imagevalues.PathIn is
// for the values file (gt-uzllg0).
func renderFlagsPathIn(townRoot string) string {
	return filepath.Join(townRoot, "gastown", renderFlagsRelPath)
}

// renderFlagsRecord is the typed file: the flags of `gt town cluster render`
// as they were last given to a live render, one JSON field per flag. It is a
// separate type from clusterRenderFlags so the file's schema is explicit and
// the flag struct stays free of tags (gt-uzllg0).
//
// Every field of clusterRenderFlags that names a flag has a field here of the
// same name and type; TestRenderFlagsRecordCoversTheFlagStruct fails until a
// newly added flag is recorded or named as deliberately not recorded.
type renderFlagsRecord struct {
	Dir                  string `json:"dir"`
	Out                  string `json:"out"`
	Namespace            string `json:"namespace"`
	Rehearsal            bool   `json:"rehearsal"`
	CPU                  string `json:"cpu"`
	Memory               string `json:"memory"`
	HostGateway          string `json:"host_gateway"`
	ConfirmStopped       bool   `json:"confirm_container_stopped"`
	WithHelper           bool   `json:"with_helper"`
	ProxyImage           string `json:"proxy_image"`
	PolecatImage         string `json:"polecat_image"`
	ConfigDir            string `json:"config_dir"`
	OpenShellConfigDir   string `json:"openshell_config_dir"`
	ForgeHost            string `json:"forge_host"`
	DoltBackupHostPath   string `json:"dolt_backup_host_path"`
	Dashboard            bool   `json:"dashboard"`
	DashboardClaudeDir   string `json:"dashboard_claude_dir"`
	BdGatewayURL         string `json:"bd_gateway_url"`
	BdGatewayTokenSecret string `json:"bd_gateway_token_secret"`
	DaemonClaudeDir      string `json:"daemon_claude_dir"`
	Tracker              bool   `json:"tracker"`
	TrackerDSNSecret     string `json:"tracker_dsn_secret"`
	TrackerBackup        bool   `json:"tracker_backup"`
	TrackerBackupImage   string `json:"tracker_backup_image"`
	DaemonBdGatewayURL   string `json:"daemon_bd_gateway_url"`
}

// record is f as the file holds it.
func (f clusterRenderFlags) record() renderFlagsRecord {
	return renderFlagsRecord{
		Dir:                  f.dir,
		Out:                  f.out,
		Namespace:            f.namespace,
		Rehearsal:            f.rehearsal,
		CPU:                  f.cpu,
		Memory:               f.memory,
		HostGateway:          f.hostGateway,
		ConfirmStopped:       f.confirmStopped,
		WithHelper:           f.withHelper,
		ProxyImage:           f.proxyImage,
		PolecatImage:         f.polecatImage,
		ConfigDir:            f.configDir,
		OpenShellConfigDir:   f.openShellConfigDir,
		ForgeHost:            f.forgeHost,
		DoltBackupHostPath:   f.doltBackupHostPath,
		Dashboard:            f.dashboard,
		DashboardClaudeDir:   f.dashboardClaudeDir,
		BdGatewayURL:         f.bdGatewayURL,
		BdGatewayTokenSecret: f.bdGatewayTokenSecret,
		DaemonClaudeDir:      f.daemonClaudeDir,
		Tracker:              f.tracker,
		TrackerDSNSecret:     f.trackerDSNSecret,
		TrackerBackup:        f.trackerBackup,
		TrackerBackupImage:   f.trackerBackupImage,
		DaemonBdGatewayURL:   f.daemonBdGatewayURL,
	}
}

// flags is r as the render's flags. daemonBdGatewayURLSet is not in the file:
// it records that the flag was named at all, which a recorded URL carries
// already, so a non-empty URL answers true as it does on the command line.
func (r renderFlagsRecord) flags() clusterRenderFlags {
	return clusterRenderFlags{
		dir:                   r.Dir,
		out:                   r.Out,
		namespace:             r.Namespace,
		rehearsal:             r.Rehearsal,
		cpu:                   r.CPU,
		memory:                r.Memory,
		hostGateway:           r.HostGateway,
		confirmStopped:        r.ConfirmStopped,
		withHelper:            r.WithHelper,
		proxyImage:            r.ProxyImage,
		polecatImage:          r.PolecatImage,
		configDir:             r.ConfigDir,
		openShellConfigDir:    r.OpenShellConfigDir,
		forgeHost:             r.ForgeHost,
		doltBackupHostPath:    r.DoltBackupHostPath,
		dashboard:             r.Dashboard,
		dashboardClaudeDir:    r.DashboardClaudeDir,
		bdGatewayURL:          r.BdGatewayURL,
		bdGatewayTokenSecret:  r.BdGatewayTokenSecret,
		daemonClaudeDir:       r.DaemonClaudeDir,
		tracker:               r.Tracker,
		trackerDSNSecret:      r.TrackerDSNSecret,
		trackerBackup:         r.TrackerBackup,
		trackerBackupImage:    r.TrackerBackupImage,
		daemonBdGatewayURL:    r.DaemonBdGatewayURL,
		daemonBdGatewayURLSet: r.DaemonBdGatewayURL != "",
	}
}

// saveRenderFlags records f at path, atomically, creating the directory the
// file names. The file is the whole flag set: a failed write leaves the flags
// of the previous render in place rather than a partial one.
func saveRenderFlags(path string, f clusterRenderFlags) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create the directory for the recorded render flags %s: %w", path, err)
	}
	if err := atomicfile.WriteJSON(path, f.record()); err != nil {
		return fmt.Errorf("write the recorded render flags %s: %w", path, err)
	}
	return nil
}

// loadRenderFlags reads the flags recorded at path. A missing file answers
// false with no error: no live render has recorded its flags yet, which is not
// a failure. A file that exists and cannot be read or decoded is an error
// naming the path, never an absent flag set.
func loadRenderFlags(path string) (clusterRenderFlags, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return clusterRenderFlags{}, false, nil
	}
	if err != nil {
		return clusterRenderFlags{}, false, fmt.Errorf("read the recorded render flags %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	// A field this build does not know is refused: a record the replay drops a
	// flag from would render a different pod than the one the file describes.
	dec.DisallowUnknownFields()
	var r *renderFlagsRecord
	if err := dec.Decode(&r); err != nil {
		return clusterRenderFlags{}, false, fmt.Errorf("the recorded render flags %s are refused: %w", path, err)
	}
	// A JSON null decodes into a nil pointer; a file holding null must not read
	// as a record whose every flag is empty.
	if r == nil {
		return clusterRenderFlags{}, false, fmt.Errorf("the recorded render flags %s are refused: the file holds no record", path)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return clusterRenderFlags{}, false, fmt.Errorf("the recorded render flags %s are refused: the file holds more than one JSON value", path)
	}
	return r.flags(), true, nil
}
