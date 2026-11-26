package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/clusterapproval"
)

// applyDeployment is a Deployment the plan holds at a resourceVersion: its
// Versions key, the name and namespace its live read names, and the kubeconfig
// of its phase.
type applyDeployment struct {
	key, namespace, name, version, kubeconfig string
}

// loadApplyPlan reads plan.json and the held Deployments' documents. No cluster
// is called here. Each file's phase must have its kubeconfig.
func loadApplyPlan(planDir string, kubeconfigs planFlags) (planRecord, []applyDeployment, error) {
	data, err := os.ReadFile(filepath.Join(planDir, planFileName))
	if err != nil {
		return planRecord{}, nil, fmt.Errorf("gt town cluster apply: the plan record cannot be read, so nothing was applied: %w", err)
	}
	var record planRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return planRecord{}, nil, errors.New("gt town cluster apply: the plan record is not a plan record, so nothing was applied")
	}
	var held []applyDeployment
	for _, file := range record.Files {
		kubeconfig, err := phaseKubeconfig(file.Phase, kubeconfigs)
		if err != nil {
			return planRecord{}, nil, err
		}
		if err := checkClusterKubeconfig(kubeconfig); err != nil {
			return planRecord{}, nil, err
		}
		if file.Absent || file.ResourceVersion == "" {
			continue
		}
		d, err := readApplyDeployment(planDir, file, kubeconfig)
		if err != nil {
			return planRecord{}, nil, err
		}
		held = append(held, d)
	}
	return record, held, nil
}

// readApplyDeployment reads the document a held file names. The file must be one
// name in planDir, and the document a Deployment whose name and namespace kubectl
// can take. Errors name the file, never its text.
func readApplyDeployment(planDir string, file planRecordFile, kubeconfig string) (applyDeployment, error) {
	if filepath.Base(file.Name) != file.Name {
		return applyDeployment{}, fmt.Errorf("gt town cluster apply: the plan names %q, which is not a file in the plan directory, so nothing was applied", file.Name)
	}
	data, err := os.ReadFile(filepath.Join(planDir, file.Name))
	if err != nil {
		return applyDeployment{}, fmt.Errorf("gt town cluster apply: the plan file %s cannot be read, so nothing was applied: %w", file.Name, err)
	}
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Kind != "Deployment" {
		return applyDeployment{}, fmt.Errorf("gt town cluster apply: the plan file %s is not a Deployment, so nothing was applied", file.Name)
	}
	if err := errors.Join(checkClusterName(doc.Metadata.Name), checkClusterNamespace(doc.Metadata.Namespace)); err != nil {
		return applyDeployment{}, err
	}
	return applyDeployment{
		key:        "deployment/" + doc.Metadata.Namespace + "/" + doc.Metadata.Name,
		namespace:  doc.Metadata.Namespace,
		name:       doc.Metadata.Name,
		version:    file.ResourceVersion,
		kubeconfig: kubeconfig,
	}, nil
}

// readApplyApproval reads the approval record at path. A record that does not
// parse is refused without ParseRecord's text, which can carry a value from it.
func readApplyApproval(path string) (clusterapproval.Record, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return clusterapproval.Record{}, fmt.Errorf("gt town cluster apply: there is no approval record at %s, so nothing was applied", path)
	case err != nil:
		return clusterapproval.Record{}, fmt.Errorf("gt town cluster apply: the approval record %s cannot be read, so nothing was applied: %w", path, err)
	}
	rec, err := clusterapproval.ParseRecord(data)
	if err != nil {
		return clusterapproval.Record{}, fmt.Errorf("gt town cluster apply: the approval record %s is not a valid approval record, so nothing was applied", path)
	}
	return rec, nil
}
