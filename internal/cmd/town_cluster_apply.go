package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// approvalVerifier is the apply's check that an approval record authorizes the
// plan it applies. The apply takes the type so the command supplies the real
// check, whose live-version comparison is a later bead of this chain.
type approvalVerifier func(rec clusterapproval.Record, plan planRecord) error

// runTownClusterApply starts the apply: it reads the plan with loadApplyPlan
// and then the approval at approvalPath with readApplyApproval, under the flags
// the command was given, and returns the first reader's error, which names
// nothing as applied. Each file the record names must hash to the SHA-256 the
// record holds for it, checked by checkApplyPlanFiles before the approval is
// read, so a changed file refuses with no cluster call. That check also holds
// the bytes it verified, and applyPlanFiles applies those bytes, so the file
// applied is the file that was checked. An approval whose PlanSHA256 is not the
// plan record's ID authorizes another plan and is refused before verify.
// Otherwise verify is called with the record and the plan, and its error is
// returned naming nothing as applied. Each held Deployment is then re-read with
// recheckApplyDeployments; one the cluster no longer holds at the plan's
// resourceVersion refuses with nothing applied, before any apply call. Then
// applyPlanFiles applies the files. Nothing in the tree calls it yet: the
// command that runs it is gt-7bxoh0.
func runTownClusterApply(ctx context.Context, kube update.RunFunc, planDir, approvalPath string, kubeconfigs planFlags, verify approvalVerifier) error {
	record, held, err := loadApplyPlan(planDir, kubeconfigs)
	if err != nil {
		return err
	}
	files, err := checkApplyPlanFiles(planDir, record)
	if err != nil {
		return err
	}
	approval, err := readApplyApproval(approvalPath)
	if err != nil {
		return err
	}
	// The approval's hash is checked here, before verify, so an approval of
	// another plan is refused without the verifier's clock and live reads.
	if approval.PlanSHA256 != record.ID {
		return fmt.Errorf("gt town cluster apply: the approval at %s is for another plan, %s, not %s, so nothing was applied", approvalPath, approval.PlanSHA256, record.ID)
	}
	if err := verify(approval, record); err != nil {
		return fmt.Errorf("gt town cluster apply: the approval at %s does not authorize this plan, so nothing was applied: %w", approvalPath, err)
	}
	if err := recheckApplyDeployments(ctx, kube, held); err != nil {
		return err
	}
	return applyPlanFiles(ctx, kube, planDir, files, kubeconfigs)
}

// applyPlanFile is one plan file with the bytes checkApplyPlanFiles read and
// verified. The apply sends these bytes rather than reading the file again, so
// the document applied is the document that was hashed and checked.
type applyPlanFile struct {
	file planRecordFile
	data []byte
}

// checkApplyPlanFiles refuses a plan whose file bytes differ from the SHA-256
// plan.json records for them. Every file the record names is checked, held or
// not, so a Deployment the cluster does not hold is no exception. A name that is
// not a file in planDir, and a file that cannot be read, are refused too: an
// unread file never passes as matching. Each matching file's documents are then
// checked by checkApplyDocuments, from the same bytes, and those bytes are
// returned for the apply. The error names the file and never a hash, and nothing
// is applied.
func checkApplyPlanFiles(planDir string, record planRecord) ([]applyPlanFile, error) {
	files := make([]applyPlanFile, 0, len(record.Files))
	for _, file := range record.Files {
		if filepath.Base(file.Name) != file.Name {
			return nil, fmt.Errorf("gt town cluster apply: the plan names %q, which is not a file in the plan directory, so nothing was applied", file.Name)
		}
		data, err := os.ReadFile(filepath.Join(planDir, file.Name))
		if err != nil {
			return nil, fmt.Errorf("gt town cluster apply: the plan file %s cannot be read, so nothing was applied: %w", file.Name, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return nil, fmt.Errorf("gt town cluster apply: the plan file %s does not match the sha256 plan.json records for it, so nothing was applied", file.Name)
		}
		if err := checkApplyDocuments(file, data); err != nil {
			return nil, err
		}
		files = append(files, applyPlanFile{file: file, data: data})
	}
	return files, nil
}

// checkApplyDocuments refuses a file holding a Secret document, in either phase,
// and a static policy document (ValidatingAdmissionPolicy, its binding, or a
// Role) in phase W, which takes none. Every document of the file is decoded, so a
// Secret after the first document is refused too. A document that does not decode
// is refused, since an unread document never passes as allowed.
func checkApplyDocuments(file planRecordFile, data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc applyDocument
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("gt town cluster apply: the plan file %s does not decode as documents, so nothing was applied", file.Name)
		}
		if err := checkApplyDocument(file, doc); err != nil {
			return err
		}
	}
}

// applyDocument is the part of a plan file's document the apply checks. Items
// holds a List's documents, which kubectl applies one by one.
type applyDocument struct {
	Kind  string          `yaml:"kind"`
	Items []applyDocument `yaml:"items"`
}

// checkApplyDocument refuses a document that holds a Secret or, in phase W, a
// static policy document. A List holds documents of its own, so each item is
// checked: a Secret wrapped in `kind: List` is refused like a bare one. A typed
// list such as SecretList names its element kind, which its items may omit, so
// that kind is checked too.
func checkApplyDocument(file planRecordFile, doc applyDocument) error {
	kinds := []string{doc.Kind}
	if elem, ok := strings.CutSuffix(doc.Kind, "List"); ok && elem != "" {
		kinds = append(kinds, elem)
	}
	for _, kind := range kinds {
		if err := checkApplyKind(file, kind); err != nil {
			return err
		}
	}
	for _, item := range doc.Items {
		if err := checkApplyDocument(file, item); err != nil {
			return err
		}
	}
	return nil
}

// checkApplyKind refuses one kind: a Secret in either phase, and a static policy
// kind in phase W.
func checkApplyKind(file planRecordFile, kind string) error {
	switch {
	case kind == secretKind:
		return fmt.Errorf("gt town cluster apply: the plan file %s holds a Secret, which apply never applies, so nothing was applied", file.Name)
	case file.Phase == planPhaseWorkloads && (kind == validatingPolicyKind || kind == validatingPolicyBindKind || kind == roleKind):
		return fmt.Errorf("gt town cluster apply: the plan file %s holds a %s in phase W, and only phase P applies static documents, so nothing was applied", file.Name, kind)
	}
	return nil
}

// applyPlanFiles applies each file from the bytes checkApplyPlanFiles verified,
// with one plain kubectl apply per file sent on stdin: phase P's first under the
// policy kubeconfig, then phase W's under the apply kubeconfig, each in plan.json
// order. Every phase's kubeconfig is read before the first apply, so a missing
// flag refuses with nothing applied rather than after phase P has been applied,
// and a file whose phase is not one of the plan's two is refused rather than
// dropped. A failed call stops the run with no rollback, and the error names the
// files applied and not applied, never kubectl's output.
func applyPlanFiles(ctx context.Context, kube update.RunFunc, planDir string, files []applyPlanFile, kubeconfigs planFlags) error {
	byPhase := map[string]string{}
	for _, f := range files {
		if _, ok := byPhase[f.file.Phase]; ok {
			continue
		}
		kubeconfig, err := applyPhaseKubeconfig(f.file.Phase, kubeconfigs)
		if err != nil {
			return err
		}
		byPhase[f.file.Phase] = kubeconfig
	}
	var ordered []applyPlanFile
	for _, phase := range []string{planPhaseStatic, planPhaseWorkloads} {
		for _, f := range files {
			if f.file.Phase == phase {
				ordered = append(ordered, f)
			}
		}
	}
	for i, f := range ordered {
		_, exit, err := kube(withRunStdin(ctx, f.data), planDir, nil, "kubectl", "--kubeconfig", byPhase[f.file.Phase], "apply", "-f", "-")
		if err != nil || exit != 0 {
			return fmt.Errorf("gt town cluster apply: the apply of %s failed, a partial apply: applied %s and not applied %s; nothing was rolled back", f.file.Name, applyFileNames(ordered[:i]), applyFileNames(ordered[i:]))
		}
	}
	return nil
}

// applyPhaseKubeconfig is phaseKubeconfig for the apply: it names the apply
// command, not the plan, since the apply is what refused.
func applyPhaseKubeconfig(phase string, f planFlags) (string, error) {
	flag, kubeconfig := "--policy-kubeconfig", f.policyKubeconfig
	switch phase {
	case planPhaseStatic:
	case planPhaseWorkloads:
		flag, kubeconfig = "--apply-kubeconfig", f.applyKubeconfig
	default:
		return "", fmt.Errorf("gt town cluster apply: %q is not a plan phase, so it has no kubeconfig and nothing was applied", phase)
	}
	if kubeconfig == "" {
		return "", fmt.Errorf("gt town cluster apply: phase %s has files but no %s, and the apply takes no other cluster identity, so nothing was applied", phase, flag)
	}
	return kubeconfig, nil
}

// applyFileNames lists the files' names for a partial-apply error, or "nothing"
// when the list is empty.
func applyFileNames(files []applyPlanFile) string {
	if len(files) == 0 {
		return "nothing"
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.file.Name)
	}
	return strings.Join(names, ", ")
}

// applyKubeconfigRunner binds kube to kubeconfig the way the plan's dry runs
// pass it, --kubeconfig before the verb, so a read cannot fall back to the
// operator's default cluster.
func applyKubeconfigRunner(kube update.RunFunc, kubeconfig string) update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		return kube(ctx, dir, env, name, append([]string{"--kubeconfig", kubeconfig}, args...)...)
	}
}

// recheckApplyDeployments re-reads each Deployment the plan holds, through the
// kubeconfig of its phase, and refuses one the cluster no longer holds at the
// plan's resourceVersion: absent, without a resourceVersion, or at another one.
// The error names the object and never a version value, so the operator re-reads
// it rather than diffing values in a log line. A read that failed is refused too,
// so an unread cluster never reads as in step; every refusal says nothing was
// applied, and no apply call is made.
func recheckApplyDeployments(ctx context.Context, kube update.RunFunc, held []applyDeployment) error {
	for _, d := range held {
		runner := applyKubeconfigRunner(kube, d.kubeconfig)
		live, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: runner}, "deployment/"+d.name, d.namespace, false)
		if err != nil {
			return fmt.Errorf("gt town cluster apply: cannot re-read %s, so nothing was applied: %w", d.key, err)
		}
		if absent {
			return fmt.Errorf("gt town cluster apply: the cluster no longer holds %s, which the plan names, so nothing was applied", d.key)
		}
		metadata, _ := live["metadata"].(map[string]any)
		version, _ := metadata["resourceVersion"].(string)
		if version != d.version {
			return fmt.Errorf("gt town cluster apply: the live %s is not the version the plan names, so nothing was applied", d.key)
		}
	}
	return nil
}
