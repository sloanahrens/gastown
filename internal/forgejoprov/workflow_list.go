package forgejoprov

import (
	"bytes"
	"fmt"
	"strings"
)

// DefaultWorkflowList is the pre-receive list the workflow-file hook reads: the
// repositories it protects. A repository provisioned here is appended to it, so the
// hook covers a new rig with no operator step.
const DefaultWorkflowList = "/data/gitea/home/hooks/pre-receive.d/gt-protected-workflow-repos"

// EnsureWorkflowProtected leaves repo named in the workflow-file hook list at listPath.
// A repository the list already names on a whole line, in any case, is not written
// again. Any other is appended once, after every existing line. A list that cannot be
// read, or holds nothing, fails the call: a hook reading an empty list protects nothing,
// so passing would hide the gap.
func (p *Provisioner) EnsureWorkflowProtected(repo, listPath string) error {
	if _, _, err := splitRepo(repo); err != nil {
		return err
	}
	files := p.files()
	data, err := files.ReadFile(listPath)
	if err != nil {
		return fmt.Errorf("read the workflow-file hook list %s: %w", listPath, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("the workflow-file hook list %s is empty: refusing to provision %s, since the hook would protect nothing", listPath, repo)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), repo) {
			p.logf("workflow-file hook list %s already names %s", listPath, repo)
			return nil
		}
	}
	if p.Client.DryRun {
		p.logf("would append %s to the workflow-file hook list %s", repo, listPath)
		return nil
	}
	info, err := files.Stat(listPath)
	if err != nil {
		return fmt.Errorf("stat the workflow-file hook list %s: %w", listPath, err)
	}
	out := append([]byte(nil), data...)
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	out = append(out, repo+"\n"...)
	if err := files.WriteFileAtomic(listPath, out, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write the workflow-file hook list %s: %w", listPath, err)
	}
	p.logf("appended %s to the workflow-file hook list %s", repo, listPath)
	return nil
}
