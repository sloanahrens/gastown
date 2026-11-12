package forgejoprov

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/flock"
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
//
// The read and the write run under the list's lock, so two provisions appending at
// once cannot each read the list before the other writes and drop the other's line.
func (p *Provisioner) EnsureWorkflowProtected(repo, listPath string) (err error) {
	if _, _, err := splitRepo(repo); err != nil {
		return err
	}
	unlock, err := p.lockWorkflowList(listPath)
	if err != nil {
		return err
	}
	defer func() {
		if uerr := unlock(); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unlock the workflow-file hook list %s: %w", listPath, uerr))
		}
	}()
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

// lockWorkflowList takes the exclusive lock that serializes the list's read-modify-write
// across processes. The lock is the sibling listPath+".lock", never the list itself: the
// list is replaced by a rename on every write, so a lock held on the old file would not
// stop a second provision reading the list it is about to replace.
func (p *Provisioner) lockWorkflowList(listPath string) (func() error, error) {
	if p.ListLock != nil {
		return p.ListLock(listPath)
	}
	fl := flock.New(listPath + ".lock")
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("lock the workflow-file hook list %s: %w", listPath, err)
	}
	return fl.Unlock, nil
}
