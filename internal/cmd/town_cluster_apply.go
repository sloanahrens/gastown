package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
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
// applyPlanFiles applies the files, printing out's rollback lines first.
// Whatever the outcome, the run appends one line to the ledger beside the
// approval record (time, plan id, the files this run applied, and the result),
// so a refused run is recorded too. A line that cannot be written is an error: a
// change on the cluster is never left unrecorded.
func runTownClusterApply(ctx context.Context, kube update.RunFunc, out io.Writer, planDir, approvalPath string, kubeconfigs planFlags, verify approvalVerifier) (err error) {
	var planID string
	var applied []string
	defer func() {
		result := applyResultRefused
		switch {
		case err == nil:
			result = applyResultApplied
		case len(applied) > 0:
			result = applyResultPartial
		}
		objects := applied
		if objects == nil {
			objects = []string{}
		}
		entry := applyLedgerEntry{TS: time.Now().UTC(), Plan: planID, Objects: objects, Result: result}
		if ledgerErr := appendApplyLedger(applyLedgerPath(approvalPath), entry); ledgerErr != nil {
			// The run's own error stays beside the ledger error; a good apply
			// whose line was not written is an error, never a silent success.
			err = errors.Join(err, fmt.Errorf("gt town cluster apply: the run's outcome is not recorded, and its ledger line could not be written: %w", ledgerErr))
		}
	}()
	record, held, err := loadApplyPlan(planDir, kubeconfigs)
	if err != nil {
		return err
	}
	planID = record.ID
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
	applied, err = applyPlanFiles(ctx, kube, out, planDir, files, kubeconfigs)
	return err
}

// applyLedgerName is the ledger `gt town cluster apply` appends one line to per
// run. It sits beside the approval record, in the approvals directory the
// deployer owns, and its 0644 create mode lets the operator read it.
const applyLedgerName = "apply-ledger.jsonl"

// The result a ledger line records: applied is a run that applied every file,
// refused one that applied nothing, and partial one that applied some and then
// stopped.
const (
	applyResultApplied = "applied"
	applyResultRefused = "refused"
	applyResultPartial = "partial"
)

// applyLedgerEntry is one line of the apply's ledger: when the run happened, the
// plan it applied, the files it applied in order, and the result. A refused run
// names no files, and its plan id is empty until plan.json has read.
type applyLedgerEntry struct {
	TS      time.Time `json:"ts"`
	Plan    string    `json:"plan"`
	Objects []string  `json:"objects"`
	Result  string    `json:"result"`
}

// applyLedgerPath is the ledger a run appends to: beside the approval record,
// the file whose directory the deployer owns.
func applyLedgerPath(approvalPath string) string {
	return filepath.Join(filepath.Dir(approvalPath), applyLedgerName)
}

// applyLedgerWriter is what an append writes through: the ledger file in
// production, and a fake in a test, so a write or close failure that a full disk
// would cause is driven without one.
type applyLedgerWriter interface {
	io.Writer
	io.Closer
}

// applyLedger appends lines to one ledger file. open is a field so a test can
// hand an append a file whose write or close fails.
type applyLedger struct {
	path string
	open func(string) (applyLedgerWriter, error)
}

// openApplyLedger opens the ledger at path for an append, creating it 0644 so
// the operator can read it. A symlink at path is refused, and so is a path that
// cannot be checked: the apply never appends through a link or a path it could
// not examine.
func openApplyLedger(path string) (applyLedgerWriter, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("the ledger %s is a symlink, and the apply appends only to a real file", path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the ledger %s cannot be checked: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("the ledger %s cannot be opened: %w", path, err)
	}
	return f, nil
}

// append writes one entry as a JSON line. The line goes in one Write under
// O_APPEND, so two runs' lines cannot interleave.
func (l applyLedger) append(entry applyLedgerEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("the apply's ledger line cannot be built: %w", err)
	}
	line = append(line, '\n')
	f, err := l.open(l.path)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		// The write error is the failure; a close error joins it rather than
		// replacing it, and a clean close leaves the write error alone.
		err = errors.Join(err, f.Close())
		return fmt.Errorf("the ledger %s cannot be written: %w", l.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("the ledger %s cannot be written: %w", l.path, err)
	}
	return nil
}

// appendApplyLedger appends one line for a run to the ledger at path.
func appendApplyLedger(path string, entry applyLedgerEntry) error {
	return applyLedger{path: path, open: openApplyLedger}.append(entry)
}

// applyPlanFile is one plan file with the bytes checkApplyPlanFiles read and
// verified. The apply sends these bytes rather than reading the file again, so
// the document applied is the document that was hashed and checked. doc is the
// file's first document as those bytes decode, so the apply knows a Deployment's
// name and namespace without decoding the file a second time.
type applyPlanFile struct {
	file planRecordFile
	data []byte
	doc  applyDocument
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
		doc, err := checkApplyDocuments(file, data)
		if err != nil {
			return nil, err
		}
		files = append(files, applyPlanFile{file: file, data: data, doc: doc})
	}
	return files, nil
}

// checkApplyDocuments refuses a file holding a Secret document, in either phase,
// and a static policy document (ValidatingAdmissionPolicy, its binding, or a
// Role) in phase W, which takes none. Every document of the file is decoded, so a
// Secret after the first document is refused too. A document that does not decode
// is refused, since an unread document never passes as allowed. It returns the
// file's first document that names a kind, so the apply reads a Deployment's name
// and namespace from the bytes it checked.
func checkApplyDocuments(file planRecordFile, data []byte) (applyDocument, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first applyDocument
	var found bool
	for {
		var doc applyDocument
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return first, nil
		}
		if err != nil {
			return applyDocument{}, fmt.Errorf("gt town cluster apply: the plan file %s does not decode as documents, so nothing was applied", file.Name)
		}
		if !found && doc.Kind != "" {
			first, found = doc, true
		}
		if err := checkApplyDocument(file, doc); err != nil {
			return applyDocument{}, err
		}
	}
}

// applyDocument is the part of a plan file's document the apply checks: its kind,
// the name and namespace a Deployment's undo line and wait read, and a List's
// documents, which kubectl applies one by one.
type applyDocument struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
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

// applyRolloutWait bounds the rollout status wait after each Deployment the
// apply writes: a Deployment that has not rolled out within it stops the apply.
// The apply takes no flag of its own for it.
const applyRolloutWait = 10 * time.Minute

// applyStep is one file of the apply in the order it is sent. deployment and
// namespace are the Deployment the file's document names, empty when the file
// holds none, and were checked in applySteps before the file was applied.
type applyStep struct {
	file       applyPlanFile
	deployment string
	namespace  string
}

// applyPlanFiles applies each file from the bytes checkApplyPlanFiles verified,
// with one plain kubectl apply per file sent on stdin: phase P's first under the
// policy kubeconfig, then phase W's under the apply kubeconfig, in the order
// applySteps gives. Every phase's kubeconfig is read before the first apply, so a
// missing flag refuses with nothing applied rather than after phase P has been
// applied, and a file whose phase is not one of the plan's two is refused rather
// than dropped. Before the first apply it prints the undo line of every
// Deployment it writes; a Deployment with no revision recorded in plan.json
// refuses with nothing applied. After each Deployment is applied it waits for its
// rollout and its Available condition before the next object, so a gateway that
// does not come back stops the apply before the daemon behind it is replaced. A
// failed call or a failed wait stops the run with no rollback, and the error names
// the files applied and not applied, never kubectl's output. It returns the files
// it applied, in order, so the ledger records them; on a failure that list is the
// files applied before it.
func applyPlanFiles(ctx context.Context, kube update.RunFunc, out io.Writer, planDir string, files []applyPlanFile, kubeconfigs planFlags) ([]string, error) {
	if out == nil {
		return nil, errors.New("gt town cluster apply: the apply has nowhere to print its rollback lines, so an operator could not undo by hand, and nothing was applied")
	}
	ordered, err := applySteps(files)
	if err != nil {
		return nil, err
	}
	byPhase := map[string]string{}
	for _, step := range ordered {
		phase := step.file.file.Phase
		if _, ok := byPhase[phase]; ok {
			continue
		}
		kubeconfig, err := applyPhaseKubeconfig(phase, kubeconfigs)
		if err != nil {
			return nil, err
		}
		byPhase[phase] = kubeconfig
	}
	if err := printApplyRollbackLines(out, ordered); err != nil {
		return nil, err
	}
	applied := make([]string, 0, len(ordered))
	for i, step := range ordered {
		f := step.file
		_, exit, err := kube(withRunStdin(ctx, f.data), planDir, nil, "kubectl", "--kubeconfig", byPhase[f.file.Phase], "apply", "-f", "-")
		if err != nil || exit != 0 {
			return applied, fmt.Errorf("gt town cluster apply: the apply of %s failed, a partial apply: applied %s and not applied %s; nothing was rolled back", f.file.Name, applyFileNames(ordered[:i]), applyFileNames(ordered[i:]))
		}
		applied = append(applied, f.file.Name)
		if step.deployment == "" {
			// A file that holds no Deployment has nothing to roll out, so the next
			// object follows at once.
			continue
		}
		resource := "deployment/" + step.deployment
		readiness := applyKubeconfigRunner(kube, byPhase[f.file.Phase])
		if err := waitDeploymentRollout(ctx, readiness, out, step.namespace, resource, applyRolloutWait); err != nil {
			return applied, fmt.Errorf("gt town cluster apply: %s was applied but did not come back ready, so nothing further was applied: applied %s and not applied %s; nothing was rolled back: %w", resource, applyFileNames(ordered[:i+1]), applyFileNames(ordered[i+1:]), err)
		}
	}
	return applied, nil
}

// applySteps returns the files in the order the apply sends them: phase P's in
// plan order, then phase W's with the bd gateway Deployment moved ahead of every
// other Deployment. The gateway leads the Deployments because each one is waited
// for before the next is applied, so the gateway is ready before the daemon it
// fronts is replaced; a gateway change and a daemon change each then work with the
// other's previous version for one roll. Each Deployment file's name and namespace
// are checked here, before the first apply, since the wait after its apply puts
// them in kubectl's arguments. A file whose phase is not one of the plan's two
// is refused rather than dropped.
func applySteps(files []applyPlanFile) ([]applyStep, error) {
	var p, w []applyStep
	for _, f := range files {
		step := applyStep{file: f}
		if f.doc.Kind == deploymentKind {
			step.deployment = f.doc.Metadata.Name
			step.namespace = f.doc.Metadata.Namespace
			if err := errors.Join(checkClusterName(step.deployment), checkClusterNamespace(step.namespace)); err != nil {
				return nil, fmt.Errorf("gt town cluster apply: the Deployment in the plan file %s is not one kubectl could wait for, so nothing was applied: %w", f.file.Name, err)
			}
		}
		switch f.file.Phase {
		case planPhaseStatic:
			p = append(p, step)
		case planPhaseWorkloads:
			w = append(w, step)
		default:
			return nil, fmt.Errorf("gt town cluster apply: %q is not a plan phase, so it has no place in the apply, and nothing was applied", f.file.Phase)
		}
	}
	return append(p, hoistGatewayDeployment(w)...), nil
}

// deploymentKind is the kind a plan file holds when the apply waits for it.
const deploymentKind = "Deployment"

// hoistGatewayDeployment moves the bd gateway Deployment ahead of the first
// Deployment of the phase, leaving every other file where the plan put it. A
// phase with no gateway Deployment, or one whose gateway already leads, is
// returned as it is.
func hoistGatewayDeployment(steps []applyStep) []applyStep {
	gateway, first := -1, -1
	for i, step := range steps {
		if step.deployment == "" {
			continue
		}
		if first < 0 {
			first = i
		}
		if gateway < 0 && step.deployment == k8sdaemon.BdGatewayName {
			gateway = i
		}
	}
	if gateway < 0 || gateway == first {
		return steps
	}
	ordered := make([]applyStep, 0, len(steps))
	for i, step := range steps {
		if i == gateway {
			continue
		}
		if i == first {
			ordered = append(ordered, steps[gateway])
		}
		ordered = append(ordered, step)
	}
	return ordered
}

// printApplyRollbackLines prints, before the first apply, the command that undoes
// each Deployment the apply writes: every Deployment file the plan does not mark
// Absent, with the revision plan.json records for it. The plan records no
// per-file difference, so a Deployment whose bytes changed nothing gets a line
// too; undoing it to its recorded revision is harmless. A Deployment with no
// positive recorded revision refuses with nothing applied, since the command the
// operator would copy would name no revision that exists. The apply prints the
// commands and never runs one, so they need only kubectl and a kubeconfig, which
// works while the daemon is down.
func printApplyRollbackLines(out io.Writer, steps []applyStep) error {
	var lines []string
	for _, step := range steps {
		if step.deployment == "" || step.file.file.Absent {
			continue
		}
		if !planRevisionIsNumber(step.file.file.Revision) {
			return fmt.Errorf("gt town cluster apply: plan.json records no revision for the plan file %s, so the undo command for deployment/%s would name none, and nothing was applied", step.file.file.Name, step.deployment)
		}
		lines = append(lines, fmt.Sprintf("kubectl rollout undo deployment/%s -n %s --to-revision=%s", step.deployment, step.namespace, step.file.file.Revision))
	}
	if len(lines) == 0 {
		return nil
	}
	fmt.Fprintln(out, "the apply never rolls back by itself; to undo each Deployment it writes, run:")
	for _, line := range lines {
		fmt.Fprintln(out, line)
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

// applyFileNames lists the steps' files for a partial-apply error, or "nothing"
// when the list is empty.
func applyFileNames(steps []applyStep) string {
	if len(steps) == 0 {
		return "nothing"
	}
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.file.file.Name)
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
