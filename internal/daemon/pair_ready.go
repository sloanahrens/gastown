package daemon

// The pair-ready check (gt-4nlpw8): the images workflow publishes a daemon and
// a polecat image for each green commit, both tagged with the commit's
// 12-character stamp, and the green commit's branch is green/<sha12> on the
// rig's Forgejo. Once both images for a green commit newer than the one the
// running daemon was rendered from are in the registry, a roll can start. This
// check raises one item per such commit, so the roll starts when it can instead
// of when someone notices. The item clears when the running commit reaches the
// green commit, or when the commit is no longer newer.
//
// A read the check could not make keeps what its rig held and raises a second,
// low item saying the check is unmeasured (deploy.pair_unreadable), so a broken
// check cannot pass for a town with no pair. After a run of failed reads the
// stale pairs are dropped and only that item remains.
//
// For a green commit newer than the running one whose pair is not published, the
// same scan reads the images workflow's newest run for that commit: a run that
// ended failed or canceled means the pair will never arrive by itself, so the
// scan raises an item for it naming the commit and the first "Error:" line of the
// job it ran (images.failed). One item per commit: a re-scan keeps it, and a
// later green run for the commit clears it (gt-3vafwr). A failed run's answer is
// reused while it stays the commit's newest run, so its job log is read once.
//
// It reads through the viewer's credential and never rolls, pulls or changes a
// branch, so it is not a lifecycle step.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
)

const (
	// pairReadyInterval is how often the check reads. A green commit lands a
	// few times a day, so a quarter hour is prompt without being a poll.
	pairReadyInterval = 15 * time.Minute
	// pairReadyBudget bounds one read of every image rig, so the heartbeat is
	// held for at most this long. A rig that runs past it keeps what it held.
	pairReadyBudget = time.Minute
	// pairReadyBranchLimit is how many branches one listing page asks for, and
	// pairReadyBranchPages the most pages one rig's listing may run to. A listing
	// that has not ended by then cannot be known to hold every green branch.
	pairReadyBranchLimit = 50
	pairReadyBranchPages = 20
	// pairReadyBranchPrefix is the branch namespace the image build reads.
	pairReadyBranchPrefix = "green/"
	// pairReadyKeyPrefix keys one item: deploy-pair-ready:<rig>:<sha12>.
	pairReadyKeyPrefix = "deploy-pair-ready:"
	// pairReadyUnreadablePrefix keys the check's own failure item,
	// deploy-pair-unreadable:<what>. A rig's is "rig:<rig>"; the town read's is
	// "town:<what the check could not read>". The category keeps a rig named
	// like a town key from sharing its item.
	pairReadyUnreadablePrefix = "deploy-pair-unreadable:"
	// pairReadyTownState is the state entry of a read the check makes for every
	// rig at once: the running commit and the rig list. It is not a rig name.
	pairReadyTownState = "<town>"
	// pairReadyForgetAfter is the consecutive failed reads after which a rig's
	// pair items are dropped: a pair the check could not confirm or deny that
	// many times in a row is stale, and the check's unreadable item is what
	// remains to be looked at.
	pairReadyForgetAfter = 3
	// pairReadyImagesWorkflow is the workflow the image build runs, named as the
	// API's workflow_id matches it: the base name of its path.
	pairReadyImagesWorkflow = "images.yml"
	// pairReadyRunLimit bounds the images runs read for one green commit. The
	// route lists them newest first, so the first one carrying the commit is the
	// run that decides it, and the page is far wider than the runs one commit
	// carries.
	pairReadyRunLimit = 20
	// imagesFailedKeyPrefix keys one item: images-failed:<rig>:<sha12>, one per
	// commit.
	imagesFailedKeyPrefix = "images-failed:"
)

// pairReadyAPI is the viewer's read of a rig's Forgejo repository: its green
// branches, the comparison that says whether a green commit is newer, and the
// images workflow run for that commit with the log of the job it ran.
type pairReadyAPI interface {
	ListBranches(ctx context.Context, owner, repo string, page, limit int) ([]forgejo.Branch, error)
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*forgejo.CompareInfo, error)
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]forgejo.ActionRunJob, error)
	JobLogs(ctx context.Context, owner, repo string, jobID int64) (string, error)
}

// pairReadyRegistry reads the digest the registry holds for an image at a tag.
// found is false for a tag that is not there.
type pairReadyRegistry interface {
	TagDigest(ctx context.Context, image, tag string) (digest string, found bool, err error)
}

// pairReadyRig is one image rig as the check reads it: its repository, the
// viewer's API for that repository and the registry its images sit in. Err is
// set when the rig could not be resolved, so the check says so and keeps what
// the rig held.
type pairReadyRig struct {
	Rig   string
	Owner string
	Repo  string
	API   pairReadyAPI
	// Registry is the image registry. Its Owner is the account the images are
	// published under, which may differ from the repository's owner.
	Registry pairReadyRegistry
	Err      error
}

// pairReadyFailedRun is a failed images run a read found for a green commit: its
// run ID and the item it raised. The item is reused while the commit's newest
// run is still that run.
type pairReadyFailedRun struct {
	runID int64
	item  attention.Item
}

// pairReadyCheck holds, per rig, the items the rig's last read raised, the
// consecutive failed reads each rig has had, and when the next read is due. The
// heartbeat writes it and the attention tick reads it, so a mutex guards it.
type pairReadyCheck struct {
	mu   sync.Mutex
	held map[string][]attention.Item
	// fails counts a rig's consecutive failed reads, keyed like held, with
	// pairReadyTownState for the reads made for every rig at once.
	fails map[string]int
	// failed holds, per images-failed key, the failed run the last read found for
	// that commit. A read that finds the same run as the commit's newest one reuses
	// its item rather than reading the job and its log again.
	failed map[string]pairReadyFailedRun
	next   time.Time
}

// run makes the check when it is due at now, and does nothing otherwise. A
// rig whose read fails neither raises nor clears a pair: an unanswered read says
// nothing about whether one exists. It keeps the items it held, and raises the
// check's own unreadable item so the queue can tell this town from a town whose
// check answered and found nothing. After pairReadyForgetAfter consecutive
// failures the stale pairs are dropped. The failures are logged through logf,
// and never with the viewer's credential.
func (c *pairReadyCheck) run(ctx context.Context, townRoot string, now time.Time, rigs func() ([]pairReadyRig, error), logf func(string, ...any)) {
	if now.Before(c.next) {
		return
	}
	c.next = now.Add(pairReadyInterval)
	running, err := pairReadyRunningCommit(townRoot)
	if err != nil {
		logf("deploy pair check: %v", err)
		c.townFailed(pairReadyUnreadablePrefix+"town:running-commit", err)
		return
	}
	list, err := rigs()
	if err != nil {
		logf("deploy pair check: %v", err)
		c.townFailed(pairReadyUnreadablePrefix+"town:rigs", err)
		return
	}
	prev, fails, failed := c.snapshot()
	out := make(map[string][]attention.Item, len(list))
	next := make(map[string]int, len(list))
	nextFailed := make(map[string]pairReadyFailedRun)
	for _, r := range list {
		if r.Err != nil {
			logf("deploy pair check: %s: %v", r.Rig, r.Err)
			next[r.Rig] = fails[r.Rig] + 1
			out[r.Rig] = pairReadyKept(prev[r.Rig], next[r.Rig], pairReadyUnreadableItem(r.Rig, r.Err))
			continue
		}
		items, found, scanErr, err := pairReadyItems(ctx, r, running, prev[r.Rig], failed, logf)
		for key, f := range found {
			nextFailed[key] = f
		}
		switch {
		case err != nil:
			logf("deploy pair check: %s: %v", r.Rig, err)
			next[r.Rig] = fails[r.Rig] + 1
			out[r.Rig] = pairReadyKept(prev[r.Rig], next[r.Rig], pairReadyUnreadableItem(r.Rig, err))
		case scanErr != nil:
			logf("deploy pair check: %s: %v", r.Rig, scanErr)
			next[r.Rig] = fails[r.Rig] + 1
			out[r.Rig] = pairReadyKept(items, next[r.Rig], pairReadyUnreadableItem(r.Rig, scanErr))
		default:
			out[r.Rig] = items
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = out
	c.fails = next
	c.failed = nextFailed
}

// townFailed records a read the check could not make for every rig at once.
// Every rig's items are kept until the same read has failed
// pairReadyForgetAfter times in a row, and the unreadable item names what could
// not be read meanwhile.
func (c *pairReadyCheck) townFailed(key string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fails := c.fails[pairReadyTownState] + 1
	held := make(map[string][]attention.Item, len(c.held)+1)
	if fails < pairReadyForgetAfter {
		for rigName, items := range c.held {
			held[rigName] = items
		}
	}
	held[pairReadyTownState] = []attention.Item{pairReadyUnreadableItemAt(key, "", err)}
	c.held = held
	if c.fails == nil {
		c.fails = map[string]int{}
	}
	c.fails[pairReadyTownState] = fails
}

// pairReadyCommitItem reports whether kind is one of the per-commit items the
// check raises: a pair ready to roll, or an image build that failed for the
// commit.
func pairReadyCommitItem(kind attention.Kind) bool {
	return kind == attention.KindDeployPairReady || kind == attention.KindImagesFailed
}

// pairReadyKept is what a rig holds after a read that did not answer: what the
// read still confirms, with the check's unreadable item naming why. Before the
// failure bound that is a partial scan's items or what the rig held; past it the
// stale per-commit items are dropped, so an item the check cannot confirm for
// good does not outlive the check that raised it.
func pairReadyKept(prev []attention.Item, fails int, unreadable attention.Item) []attention.Item {
	var out []attention.Item
	if fails < pairReadyForgetAfter {
		for _, it := range prev {
			if pairReadyCommitItem(it.Kind) {
				out = append(out, it)
			}
		}
	}
	return append(out, unreadable)
}

// snapshot is a copy of the items and failure counts the check holds, read
// before a pass so a rig whose read fails can keep them. The failed runs are
// returned as they are: run replaces the map rather than writing to it, so a
// pass reads them without a copy.
func (c *pairReadyCheck) snapshot() (map[string][]attention.Item, map[string]int, map[string]pairReadyFailedRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := make(map[string][]attention.Item, len(c.held))
	for rigName, items := range c.held {
		held[rigName] = items
	}
	fails := make(map[string]int, len(c.fails))
	for name, n := range c.fails {
		fails[name] = n
	}
	return held, fails, c.failed
}

// observed is every item the check holds, ordered by key, for the attention
// queue.
func (c *pairReadyCheck) observed() []attention.Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []attention.Item
	for _, items := range c.held {
		out = append(out, items...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// pairReadyGreen is one green branch: the full commit it points at and the
// 12-character stamp its name carries.
type pairReadyGreen struct {
	sha   string
	sha12 string
}

// pairReadyItems reads one rig and returns the items it raises: one for each
// green commit newer than running whose daemon and polecat images are both in
// the registry at its stamp, and one for each commit whose pair is missing and
// whose images workflow run ended failed or canceled. A read that cannot list
// the branches returns an error, so the caller keeps what the rig held until the
// failure bound. A listing that ended early or a branch whose comparison or
// image read did not answer returns a non-nil scanErr with the items it did
// confirm plus the items from prev whose branch it did not judge: a scan that
// could not read a branch cannot say its pair is gone, and one branch's failure
// does not discard the pass. An image that is not there is not an error: one
// image alone raises nothing. A failed run list does not stop the pass either:
// that commit keeps the item it held and the read is logged. found is the failed
// runs this read found, for the next read to reuse.
func pairReadyItems(ctx context.Context, r pairReadyRig, running string, prev []attention.Item, runs map[string]pairReadyFailedRun, logf func(string, ...any)) (items []attention.Item, found map[string]pairReadyFailedRun, scanErr error, err error) {
	scan, err := pairReadyGreenCommits(ctx, r)
	if err != nil {
		return nil, nil, nil, err
	}
	var out []attention.Item
	found = make(map[string]pairReadyFailedRun)
	// seen holds the items this pass judged: both the pair and the failed build
	// of a commit it read to a verdict. A branch a failed read stopped at is not
	// seen, so its previous items are kept and a stale one is only cleared by a
	// read that looked at it.
	seen := make(map[string]bool, len(scan.greens)*2)
	mark := func(sha12 string) {
		seen[pairReadyKey(r.Rig, sha12)] = true
		seen[imagesFailedKey(r.Rig, sha12)] = true
	}
	incomplete := scan.err
	for _, g := range scan.greens {
		if g.sha == running {
			mark(g.sha12)
			continue
		}
		// running is an ancestor of the green commit when no commit of running
		// is missing from it: the comparison of green against running counts none.
		behind, err := r.API.CompareCommits(ctx, r.Owner, r.Repo, g.sha, running)
		if err != nil {
			incomplete = fmt.Errorf("comparing green/%s with the running commit %s: %w", g.sha12, greenRefShortSHA(running), err)
			break
		}
		if behind.TotalCommits != 0 {
			mark(g.sha12)
			continue
		}
		ahead, err := r.API.CompareCommits(ctx, r.Owner, r.Repo, running, g.sha)
		if err != nil {
			incomplete = fmt.Errorf("counting the commits of green/%s ahead of the running commit: %w", g.sha12, err)
			break
		}
		if ahead.TotalCommits == 0 {
			mark(g.sha12)
			continue
		}
		daemon, daemonFound, err := r.Registry.TagDigest(ctx, imagevalues.DaemonImage, g.sha12)
		if err != nil {
			incomplete = fmt.Errorf("reading the %s image at green/%s: %w", imagevalues.DaemonImage, g.sha12, err)
			break
		}
		polecat, polecatFound, err := r.Registry.TagDigest(ctx, imagevalues.PolecatImage, g.sha12)
		if err != nil {
			incomplete = fmt.Errorf("reading the %s image at green/%s: %w", imagevalues.PolecatImage, g.sha12, err)
			break
		}
		mark(g.sha12)
		if daemonFound && polecatFound {
			out = append(out, pairReadyItem(r.Rig, g, running, daemon, polecat, ahead.TotalCommits))
			continue
		}
		// The pair is missing, so the commit's images run is what says whether one
		// can still arrive. Only such a commit asks Forgejo about its run. The
		// check settles the commit's failed-build item here, so the incomplete
		// path below never has to.
		failed, failedRun := pairReadyImagesFailed(ctx, r, g, prev, runs, logf)
		if failed != nil {
			out = append(out, *failed)
		}
		if failedRun != 0 {
			found[imagesFailedKey(r.Rig, g.sha12)] = pairReadyFailedRun{runID: failedRun, item: *failed}
		}
	}
	if incomplete != nil {
		for _, it := range prev {
			if pairReadyCommitItem(it.Kind) && !seen[it.Key] {
				out = append(out, it)
			}
		}
		return out, found, incomplete, nil
	}
	return out, found, nil, nil
}

// pairReadyScan is one rig's branch listing. complete says the listing reached
// its end. An incomplete listing raises the branches it read and cannot clear a
// pair on one it did not reach; err says why it ended early.
type pairReadyScan struct {
	greens   []pairReadyGreen
	complete bool
	err      error
}

// pairReadyGreenCommits lists every green/<sha12> branch of the rig's
// repository. A branch whose name does not match the commit it points at is an
// error: the stamp the registry is read by would be wrong. A listing that fails
// on a page, or runs past pairReadyBranchPages, is incomplete rather than
// failed: the pages already read are kept, and the ones not reached are named
// in the scan's err.
func pairReadyGreenCommits(ctx context.Context, r pairReadyRig) (pairReadyScan, error) {
	var out []pairReadyGreen
	for page := 1; page <= pairReadyBranchPages; page++ {
		branches, err := r.API.ListBranches(ctx, r.Owner, r.Repo, page, pairReadyBranchLimit)
		if err != nil {
			return pairReadyScan{greens: out, err: fmt.Errorf("listing page %d of %s/%s: %w", page, r.Owner, r.Repo, err)}, nil
		}
		for _, b := range branches {
			stamp, ok := strings.CutPrefix(b.Name, pairReadyBranchPrefix)
			if !ok {
				continue
			}
			if !imagevalues.ValidCommit(b.Commit.ID) || greenRefShortSHA(b.Commit.ID) != stamp {
				return pairReadyScan{}, fmt.Errorf("the branch %s points at commit %q, which its name does not carry", b.Name, b.Commit.ID)
			}
			out = append(out, pairReadyGreen{sha: b.Commit.ID, sha12: stamp})
		}
		if len(branches) < pairReadyBranchLimit {
			return pairReadyScan{greens: out, complete: true}, nil
		}
	}
	return pairReadyScan{greens: out, err: fmt.Errorf("the branch listing of %s/%s runs past %d pages", r.Owner, r.Repo, pairReadyBranchPages)}, nil
}

// pairReadyKey is one pair item's key: deploy-pair-ready:<rig>:<sha12>.
func pairReadyKey(rig, sha12 string) string {
	return pairReadyKeyPrefix + rig + ":" + sha12
}

// pairReadyItem is the item for one green commit with both images published.
// Its summary names the stamp, both digests and how many commits the green
// commit is ahead of the running one.
func pairReadyItem(rig string, g pairReadyGreen, running, daemonDigest, polecatDigest string, ahead int) attention.Item {
	return attention.Item{
		Key:      pairReadyKey(rig, g.sha12),
		Kind:     attention.KindDeployPairReady,
		Severity: attention.SeverityLow,
		Rig:      rig,
		SHA:      g.sha,
		Summary: fmt.Sprintf("the images for green/%s are published: daemon %s and polecat %s, %d commits ahead of the running commit %s; roll the daemon onto them",
			g.sha12, daemonDigest, polecatDigest, ahead, greenRefShortSHA(running)),
	}
}

// imagesFailedKey is one failed image build's key:
// images-failed:<rig>:<sha12>. The commit is what it names, so the same commit
// re-scanned keeps one item and a later green run clears it by not re-raising
// it.
func imagesFailedKey(rig, sha12 string) string {
	return imagesFailedKeyPrefix + rig + ":" + sha12
}

// pairReadyImagesFailed judges the images workflow's newest run for a green
// commit whose pair is missing. A run that ended failed or canceled raises the
// item naming the commit and the first "Error:" line of the job it ran; a
// successful run raises nothing, which is what clears an item raised for the
// commit before. A run still in flight is no answer, and neither is a run list
// the check could not read or a commit with no run in the list: in each case the
// item the check last raised for the commit stands. A failed run whose log the
// check read is returned with its run ID, so the next read reuses the item; the
// ID is 0 otherwise.
func pairReadyImagesFailed(ctx context.Context, r pairReadyRig, g pairReadyGreen, prev []attention.Item, runs map[string]pairReadyFailedRun, logf func(string, ...any)) (item *attention.Item, runID int64) {
	key := imagesFailedKey(r.Rig, g.sha12)
	kept := func() *attention.Item {
		for i := range prev {
			if prev[i].Key == key && prev[i].Kind == attention.KindImagesFailed {
				it := prev[i]
				return &it
			}
		}
		return nil
	}
	list, err := r.API.ListRuns(ctx, r.Owner, r.Repo, forgejo.RunFilter{
		Event: []string{"push"}, WorkflowID: pairReadyImagesWorkflow, HeadSHA: g.sha, Limit: pairReadyRunLimit,
	})
	if err == nil && list == nil {
		err = errors.New("the list is not there")
	}
	if err != nil {
		logf("deploy pair check: %s: reading the %s runs for green/%s, the failed-build check keeps what it held for that commit: %v",
			r.Rig, pairReadyImagesWorkflow, g.sha12, err)
		return kept(), 0
	}
	// The route lists runs newest first, so the first one carrying this commit
	// is the run that decides it. The filter asks for the commit and the run's
	// own commit is checked too: a filter a server ignores must not name
	// another commit's failure.
	var run *forgejo.ActionRun
	for i := range list.Runs {
		if list.Runs[i].CommitSHA == g.sha {
			run = &list.Runs[i]
			break
		}
	}
	if run == nil {
		return kept(), 0
	}
	switch run.Status {
	case deployRunFailed, deployRunCancelled:
		if c, ok := runs[key]; ok && c.runID == run.ID {
			it := c.item
			return &it, run.ID
		}
		line, settled := pairReadyImagesErrorLine(ctx, r, *run)
		it := attention.Item{
			Key:      key,
			Kind:     attention.KindImagesFailed,
			Severity: attention.SeverityHigh,
			Rig:      r.Rig,
			SHA:      g.sha,
			Summary:  fmt.Sprintf("images workflow failed for %s: %s", g.sha12, line),
		}
		if !settled {
			return &it, 0
		}
		return &it, run.ID
	case deployRunSucceeded:
		return nil, 0
	}
	return kept(), 0
}

// pairReadyImagesErrorLine is what a failed images run says went wrong: the
// first line of the run's failed job's log matching "Error:", as quoted CI
// output. A log the check cannot read is said to be unreadable rather than left
// out, so the item names the failure it does know about (the run ended failed)
// and says which part it does not. settled is false when a read failed, so that
// answer is not kept for the next read.
func pairReadyImagesErrorLine(ctx context.Context, r pairReadyRig, run forgejo.ActionRun) (line string, settled bool) {
	jobs, err := r.API.ListRunJobs(ctx, r.Owner, r.Repo, run.ID)
	if err != nil {
		return "the job log could not be read: " + oneLine(err.Error()), false
	}
	jobID, ok := pairReadyFailedJob(jobs)
	if !ok {
		return "no failed job found, so no log was read", true
	}
	log, err := r.API.JobLogs(ctx, r.Owner, r.Repo, jobID)
	if err != nil {
		return "the job log could not be read: " + oneLine(err.Error()), false
	}
	if errLine := imagesErrorLine(log); errLine != "" {
		return imagesLogText(errLine), true
	}
	return "the job log names no Error: line", true
}

// imagesLogText is a job-log line as the summary states it. The log is text a
// build writes, so it is untrusted: whitespace becomes a space, other control and
// non-printing characters are dropped, the line keeps the 160-character cap, and
// it is quoted as CI output so it reads as data. A double quote in the line
// becomes a single one, so the quoting stays unambiguous.
func imagesLogText(line string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsPrint(r):
			return r
		}
		return -1
	}, line)
	clean = strings.ReplaceAll(clean, `"`, "'")
	return `log: "` + oneLine(clean) + `"`
}

// pairReadyFailedJob is the job whose log carries a failed run's reason: the
// first job the API reports as failed or canceled. A run that marks no such job
// has none to read, so the check does not guess at one.
func pairReadyFailedJob(jobs []forgejo.ActionRunJob) (int64, bool) {
	for _, job := range jobs {
		if job.Status == deployRunFailed || job.Status == deployRunCancelled {
			return job.ID, true
		}
	}
	return 0, false
}

// imagesErrorLine is the first line of a job log matching "Error:", trimmed.
// The build's guards print their reason that way. "" is a log with no such
// line.
func imagesErrorLine(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "Error:") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// pairReadyUnreadableItem is the check's own item for a read it could not make:
// rig names the rig, or "" for a read made for every rig at once. It says the
// check is unmeasured, which is a low severity record and not a wake: the queue
// row is how the overseer tells this town from a town whose check answered and
// found no pair.
func pairReadyUnreadableItem(rig string, err error) attention.Item {
	return pairReadyUnreadableItemAt(pairReadyUnreadablePrefix+"rig:"+rig, rig, err)
}

// pairReadyUnreadableItemAt builds the unreadable item at key, for rig or, when
// rig is "", for the town read key names.
func pairReadyUnreadableItemAt(key, rig string, err error) attention.Item {
	summary := fmt.Sprintf("the pair check could not finish a read it makes for every rig: %s", oneLine(err.Error()))
	if rig != "" {
		summary = fmt.Sprintf("the pair check could not finish reading %s's green images: %s", rig, oneLine(err.Error()))
	}
	return attention.Item{
		Key:      key,
		Kind:     attention.KindDeployPairUnreadable,
		Severity: attention.SeverityLow,
		Rig:      rig,
		Summary:  summary,
	}
}

// pairReadyRunningCommit is the commit the running daemon was rendered from:
// the commit line of the values file under the town's gastown directory. With
// no file there is no running commit to judge a green one against, so that is
// an error, not a commit the check can assume. imagevalues refuses a file whose
// commit line is not a full lowercase commit, so the commit read here is one a
// green branch name can carry; a file that will not parse is that same error.
func pairReadyRunningCommit(townRoot string) (string, error) {
	v, ok, err := imagevalues.LoadIn(filepath.Join(townRoot, "gastown"))
	if err != nil {
		return "", fmt.Errorf("reading the running commit from the values file: %w", err)
	}
	if !ok {
		return "", errors.New("no values file names the running commit")
	}
	return v.Commit, nil
}

// pairReadyTagReader is the production registry read: the viewer's basic
// credential against one registry. hc is nil for the default client, and a test
// replaces it so no socket opens.
type pairReadyTagReader struct {
	reg   imagevalues.Registry
	user  string
	token string
	hc    *http.Client
}

// TagDigest reads the digest of image at tag through the viewer's credential.
func (p pairReadyTagReader) TagDigest(ctx context.Context, image, tag string) (string, bool, error) {
	plaintext := forgejo.PlaintextHostOf(p.reg.Scheme, p.reg.Host)
	return imagevalues.TagDigest(ctx, p.hc, p.reg, plaintext, p.user, p.token, image, tag)
}

// checkPairReady is the pair check's heartbeat step. It runs within its own
// budget, and a failed read never changes the state a rig held.
func (d *Daemon) checkPairReady() {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, pairReadyBudget)
	defer cancel()
	d.pairReady.run(ctx, d.config.TownRoot, d.clk().Now(), d.productionPairReadyRigs, d.logger.Printf)
}

// productionPairReadyRigs resolves every image rig: its repository and Forgejo
// instance from its own remote, the registry its images sit in, and the
// viewer's clients. A rig that cannot be resolved is returned with its error,
// so the check reports it and keeps that rig's items.
func (d *Daemon) productionPairReadyRigs() ([]pairReadyRig, error) {
	names, err := d.greenRefImageRigs()
	if err != nil {
		return nil, err
	}
	tokenDir, tokenErr := pairReadyTokenDir(d.patrolConfig)
	out := make([]pairReadyRig, 0, len(names))
	for _, name := range names {
		out = append(out, d.pairReadyRig(name, tokenDir, tokenErr))
	}
	return out, nil
}

// pairReadyTokenDir is the directory the viewer's token file sits in: the
// landing worker's configured directory, or the host default.
func pairReadyTokenDir(cfg *DaemonPatrolConfig) (string, error) {
	return landingWorkerConfig(cfg).ForgejoTokenDir()
}

// pairReadyRig resolves one image rig from its own Forgejo settings. A refusal
// is returned in Err by pairReadyRigFrom.
func (d *Daemon) pairReadyRig(name, tokenDir string, tokenErr error) pairReadyRig {
	fc, err := rig.ResolveForgejoConfigWithError(d.repoFileOpener(), d.config.TownRoot, name)
	return pairReadyRigFrom(name, fc, err, tokenDir, tokenErr)
}

// pairReadyRigFrom resolves one image rig over its settings as the config
// reader returned them. Each refusal is returned in Err and names only what the
// rig's settings lack: the remote URL is never echoed, since it can carry a
// credential in its user info.
func pairReadyRigFrom(name string, fc *config.ForgejoConfig, resolveErr error, tokenDir string, tokenErr error) pairReadyRig {
	out := pairReadyRig{Rig: name}
	switch {
	case tokenErr != nil:
		out.Err = fmt.Errorf("the Forgejo token directory: %w", tokenErr)
		return out
	case resolveErr != nil:
		out.Err = errors.New("the rig's Forgejo settings could not be read")
		return out
	case fc == nil || strings.TrimSpace(fc.RemoteURL) == "":
		out.Err = fmt.Errorf("rig %s names no merge_queue.forgejo remote_url, so it has no images to read", name)
		return out
	}
	remote := strings.TrimSpace(fc.RemoteURL)
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || u.Host == "" {
		out.Err = fmt.Errorf("rig %s's remote_url is not a URL with a scheme and host", name)
		return out
	}
	owner, repo, err := land.RepoFromRemoteURL(remote)
	if err != nil {
		out.Err = fmt.Errorf("rig %s's remote_url names no owner and repository", name)
		return out
	}
	// The Forgejo API root of the instance the remote names: the root
	// land.APIBaseFromRemoteURL derives from the same URL.
	apiBase := u.Scheme + "://" + u.Host + "/api/v1"
	// The images are published under the registry_owner the rig names, or the
	// repository's owner when it names none (gt-69fgg3).
	registryOwner := owner
	if named := strings.TrimSpace(fc.RegistryOwner); named != "" {
		registryOwner = named
	}
	reg := imagevalues.Registry{Scheme: u.Scheme, Host: u.Host, Owner: registryOwner}
	if err := reg.Validate(); err != nil {
		out.Err = fmt.Errorf("rig %s names no usable registry: %w", name, err)
		return out
	}
	tokenPath := filepath.Join(tokenDir, "forgejo-"+config.ForgejoRoleViewer+".env")
	// The branch and compare reads carry the viewer's token, so they go
	// through the same credential-transport rule as the registry read: plain
	// http only to a loopback address or the town's own Forgejo, and no
	// redirect that changes the scheme or the host. A refused read never
	// reaches the network.
	plaintext := forgejo.PlaintextHostOf(u.Scheme, u.Host)
	api, err := forgejo.NewClient(config.ForgejoRoleViewer,
		forgejo.WithBaseURL(apiBase),
		forgejo.WithTokenFile(tokenPath),
		forgejo.WithRequestGuard(func(reqURL *url.URL) error {
			return imagevalues.CheckCredentialTransport(reqURL, plaintext)
		}),
		forgejo.WithHTTPClient(&http.Client{CheckRedirect: imagevalues.SameEndpointRedirect}),
	)
	if err != nil {
		out.Err = fmt.Errorf("no %s Forgejo token: %w", config.ForgejoRoleViewer, err)
		return out
	}
	user, token, err := forgejo.ReadCredentialFile(tokenPath)
	if err != nil {
		out.Err = fmt.Errorf("the %s token file: %w", config.ForgejoRoleViewer, err)
		return out
	}
	out.Owner, out.Repo = owner, repo
	out.API = api
	out.Registry = pairReadyTagReader{reg: reg, user: user, token: token}
	return out
}

// collectPairReady returns the pair items the pair check holds: one per green
// commit whose images are both published. It raises only its own kind, so the
// unreadable item has a collector of its own.
func (s *attentionSources) collectPairReady(context.Context) ([]attention.Item, error) {
	return s.pairReadyItemsOfKind(attention.KindDeployPairReady), nil
}

// collectPairUnreadable returns the check's own failure items: the reads it
// could not finish. It is a separate collector so a failed collector keeps the
// previous items of its own kind (gt-4nlpw8).
func (s *attentionSources) collectPairUnreadable(context.Context) ([]attention.Item, error) {
	return s.pairReadyItemsOfKind(attention.KindDeployPairUnreadable), nil
}

// collectImagesFailed returns the failed image builds the same read finds: one
// per green commit whose images run ended badly (gt-3vafwr). It is a collector
// of its own for the same reason the pair's two kinds are.
func (s *attentionSources) collectImagesFailed(context.Context) ([]attention.Item, error) {
	return s.pairReadyItemsOfKind(attention.KindImagesFailed), nil
}

// pairReadyItemsOfKind is the pair check's held items of one kind.
func (s *attentionSources) pairReadyItemsOfKind(kind attention.Kind) []attention.Item {
	if s.pairReady == nil {
		return nil
	}
	var out []attention.Item
	for _, it := range s.pairReady() {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}
