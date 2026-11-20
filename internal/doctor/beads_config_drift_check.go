package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/rig"
)

// beadsConfigTrackedPath is the rig repo's tracked bd settings file.
const beadsConfigTrackedPath = ".beads/config.yaml"

// BeadsConfigDriftCheck warns where a rig's tracked .beads/config.yaml and its
// live store disagree: nothing copies the tracked file to the store after
// `gt rig add`, so a landed change to it otherwise never takes effect
// (gt-3lvcjz.16, gt-87ue9d).
type BeadsConfigDriftCheck struct {
	BaseCheck
}

// NewBeadsConfigDriftCheck creates a check for drift between each rig's tracked
// .beads/config.yaml and its live store.
func NewBeadsConfigDriftCheck() *BeadsConfigDriftCheck {
	return &BeadsConfigDriftCheck{
		BaseCheck: BaseCheck{
			CheckName:        "beads-config-drift",
			CheckDescription: "Report a rig's tracked .beads/config.yaml settings that differ from the live store",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run reads each registered rig's tracked .beads/config.yaml at the rig repo's
// default branch and compares every top-level scalar key with what bd reports
// for that rig's store. The check changes nothing.
func (c *BeadsConfigDriftCheck) Run(ctx *CheckContext) *CheckResult {
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(ctx.TownRoot, "mayor", "rigs.json"))
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Could not load rigs registry",
			Details: []string{err.Error()},
		}
	}
	rigs := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		rigs = append(rigs, name)
	}
	sort.Strings(rigs)

	var details, problems []string
	drifted, checked := 0, 0
	for _, rigName := range rigs {
		file, err := rig.ReadRepoFile(ctx.rigRepoOpener(), ctx.TownRoot, rigName, beadsConfigTrackedPath)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !file.Found {
			continue
		}
		settings, err := beadsConfigSettings(string(file.Data))
		if err != nil {
			problems = append(problems, fmt.Sprintf("rig %s: cannot parse %s in %s: %v", rigName, beadsConfigTrackedPath, file.Source, err))
			continue
		}
		beadsDir := doltserver.FindRigBeadsDir(ctx.TownRoot, rigName)
		if _, err := os.Stat(beadsDir); err != nil {
			problems = append(problems, fmt.Sprintf("rig %s: cannot read the live store at %s: %v", rigName, beadsDir, err))
			continue
		}
		checked++
		bd := ctx.bd(filepath.Dir(beadsDir), doctorConfigEnv(beadsDir))
		rigDrifted := false
		for _, setting := range settings {
			live, err := bd.ConfigGet(setting.Key)
			if err != nil {
				problems = append(problems, fmt.Sprintf("rig %s: cannot read %s from the live store: %s", rigName, setting.Key, bdOutput(err)))
				continue
			}
			if strings.TrimSpace(live) != strings.TrimSpace(setting.Value) {
				rigDrifted = true
				details = append(details, fmt.Sprintf("%s: %s: tracked %q, live %q", rigName, setting.Key, setting.Value, live))
			}
		}
		if rigDrifted {
			drifted++
		}
	}

	if len(problems) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot compare %d tracked file(s) or live setting(s)", len(problems)),
			Details: append(problems, details...),
		}
	}
	if drifted > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Tracked %s differs from the live store in %d of %d rig(s)", beadsConfigTrackedPath, drifted, checked),
			Details: details,
		}
	}
	if checked == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No registered rig's default branch has a tracked " + beadsConfigTrackedPath,
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("Tracked %s matches the live store in %d rig(s)", beadsConfigTrackedPath, checked),
	}
}

// beadsConfigSetting is one top-level scalar setting of a bd config.yaml.
type beadsConfigSetting struct {
	Key   string
	Value string
}

// beadsConfigSettings returns the top-level scalar settings of a bd
// config.yaml, with each value as yaml gives it: unquoted, comment removed. A
// key whose value is a mapping is a nested table, not a single setting, so it
// is skipped. A file that does not parse is an error: an unreadable settings
// file must not read as one with no settings.
func beadsConfigSettings(content string) ([]beadsConfigSetting, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("its top level is not a mapping")
	}
	settings := make([]beadsConfigSetting, 0, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode {
			continue
		}
		settings = append(settings, beadsConfigSetting{Key: key.Value, Value: value.Value})
	}
	return settings, nil
}
