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
// reused while it stays the commit's newest run, so its job log is read once. A
// run list the scan cannot read raises images.unknown for that commit, since a
// failure to look is not a clean commit; the next readable list clears it.
//
// The update ledger records a pair the pre-swap check refused, with the check
// and the failure it named. A green commit newer than the running one that the
// ledger records as refused raises deploy.pair_refused instead of
// deploy.pair_ready, naming the reason, so no roll starts from it; it clears
// when the ledger clears the refusal or the running commit reaches it. A ledger
// the check cannot read is a pair answer it could not make: no pair-ready item
// is raised while it is unreadable, since a published pair may be one the
// pre-swap check refused, and the check raises its unmeasured item naming the
// ledger. A refused item the check already holds survives an unreadable ledger,
// which cannot say the refusal is gone (gt-t8vf15).
//
// A pair whose bd pin carries another schema level than the live database would
// roll a daemon its startup handshake refuses, so the check raises that skew
// instead of the ready item, and raises an unreadable item beside the ready one
// when it cannot read the level (gt-5f0kue; "The roll" in docs/architecture.md
// names the items).
//
// Whether a green commit is newer than the running one is read from the rig's
// bare clone, with no fetch: running is an ancestor of green. The Forgejo
// comparison answers only when the clone lacks one of the two commits, or when
// the rig has no clone at all, and the item then says so (gt-enj8p3).
//
// It reads through the viewer's credential and never rolls, pulls or changes a
// branch, so it is not a lifecycle step.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdskew"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/update"
)

const (
	// pairReadyInterval is how often the check reads. A green commit lands a
	// few times a day, so a quarter hour is prompt without being a poll.
	pairReadyInterval = 15 * time.Minute
	// pairReadyBudget bounds one read of every image rig, so the heartbeat is
	// held for at most this long. A rig that runs past it keeps what it held.
	pairReadyBudget = time.Minute
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
	// imagesUnknownKeyPrefix keys one item: images-unknown:<rig>:<sha12>, one per
	// commit whose run list could not be read.
	imagesUnknownKeyPrefix = "images-unknown:"
	// pairReadyRefusedPrefix keys one item:
	// deploy-pair-refused:<rig>:<sha12>, one per commit the update ledger
	// records as refused.
	pairReadyRefusedPrefix = "deploy-pair-refused:"
	// pairReadyStuckPrefix keys one item: deploy-pair-stuck:<rig>:<sha12>.
	pairReadyStuckPrefix = "deploy-pair-stuck:"
	// pairReadyStuckAfter is how long a green commit newer than the running one
	// may wait for its pair before the check raises deploy-pair-stuck (gt-afwjb4).
	pairReadyStuckAfter = 90 * time.Minute
	// pairReadySkewPrefix keys one skewed pair: deploy-pair-skew:<rig>:<sha12>.
	pairReadySkewPrefix = "deploy-pair-skew:"
	// pairReadySkewKeep bounds each map of the skew guard's cache of immutable
	// commits: the pin each green names and the level each bd commit carries.
	// The maps start over at the bound, so a town's history of greens and pins
	// cannot grow them without end.
	pairReadySkewKeep = 64
)

// pairReadyAPI is the viewer's read of a rig's Forgejo repository: its green
// branches, the repository itself (the confirmation a 404 from the green refs
// is checked with), the comparison that says whether a green commit is newer,
// and the images workflow run for that commit with the log of the job it ran.
type pairReadyAPI interface {
	ListRefs(ctx context.Context, owner, repo, prefix string) ([]forgejo.Ref, error)
	Repository(ctx context.Context, owner, repo string) (*forgejo.Repository, error)
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

// pairReadyGit is the rig's bare clone as the check reads it: whether it holds a
// commit and whether one commit is an ancestor of another. daemonGit satisfies
// it, and a test passes a gitfake repository.
type pairReadyGit interface {
	HasObject(sha string) (bool, error)
	IsAncestor(ancestor, descendant string) (bool, error)
}

// pairReadyAged is the clone's read of a commit's time, which ages a green ref
// whose pair is missing.
type pairReadyAged interface {
	CommitTime(rev string) (time.Time, error)
}

// pairReadyPinsReader is the clone's read of a file at a commit: the green
// commit's image pins name the bd commit the pair would roll.
type pairReadyPinsReader interface {
	ShowFileAtRev(ref, path string) (string, error)
}

// pairReadyPinsRead reads the image-pins file a green commit carries from the
// rig's bare clone. nil is a rig with no clone to read, which the skew guard
// reports as an unread pin rather than a ready pair.
type pairReadyPinsRead func(sha string) (string, error)

// pairReadySchemaStore is the part of the startup handshake's store the skew
// guard reads: the migration level of the database the town's bd serves.
type pairReadySchemaStore interface {
	SchemaLevel() (int, error)
}

// pairReadySkewReads is the skew guard's two town-wide reads: the schema level
// a bd commit carries and the live database's level. A nil field is unwired,
// and the guard reads it as an error rather than as an agreeing level.
type pairReadySkewReads struct {
	// LevelAt reads the schema level the bd commit carries.
	LevelAt func(ctx context.Context, commit string) (int, error)
	// Database reads the live database's schema level. It is called once per
	// pass, so a migration that moves the level is noticed on the next pass.
	Database func(ctx context.Context) (int, error)
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
	// Git is the rig's bare clone, which judges whether a green commit is newer
	// than the running one without a Forgejo call. A rig the daemon has no clone
	// for has nil, and every green is compared through the API instead.
	Git pairReadyGit
	// Pins reads the image-pins file at a green commit from the same clone, for
	// the bd pin the skew guard checks. nil is a rig without that read, and the
	// guard then reports the pin unread rather than the pair ready.
	Pins pairReadyPinsRead
	Err  error
}

// pairReadyFailedRun is a failed images run a read found for a green commit: its
// run ID and the item it raised. The item is reused while the commit's newest
// run is still that run.
type pairReadyFailedRun struct {
	runID int64
	item  attention.Item
}

// pairReadyRefusal is the update ledger's answer on the refused pair: known is
// false when the ledger could not be read, so the check raises no pair-ready
// item this pass (it cannot say whether the pair was refused) and keeps the
// refused items it holds. err is the read failure, for the item that says the
// check is unmeasured. commit is the green commit the pre-swap check refused and
// reason the failure it recorded, both empty when the ledger names no refusal.
type pairReadyRefusal struct {
	known  bool
	err    error
	commit string
	reason string
}

// pairReadyLedgerRead reads the update ledger at a town root. nil in
// pairReadyCheck reads the town's file, so a test replaces it to drive a
// refusal without one.
type pairReadyLedgerRead func(townRoot string) (update.ImageRecord, error)

// pairReadyCheck holds, per rig, the items the rig's last read raised, the
// consecutive failed reads each rig has had, and when the next read is due. The
// heartbeat writes it and the attention tick reads it, so a mutex guards it.
type pairReadyCheck struct {
	mu   sync.Mutex
	held map[string][]attention.Item
	// fails counts a rig's consecutive failed reads, keyed like held, with
	// pairReadyTownState for the reads made for every rig at once.
	fails map[string]int
	// failed holds, per images-failed key, the failed run that answers the item the
	// rig holds for that commit: the run the last read found, or the one an
	// unreadable read kept. A read that finds the same run as the commit's newest
	// one reuses its item rather than reading the job and its log again.
	failed map[string]pairReadyFailedRun
	next   time.Time
	// ledger reads the update ledger whose refused pair the check names, and
	// ledgerErr is the read error last logged, so an unreadable ledger is
	// reported once per outage rather than on every pass.
	ledger    pairReadyLedgerRead
	ledgerErr string
	// runningCommit reads the commit the running daemon was rendered from. nil
	// reads the town's values file, which a partial roll leaves stale.
	runningCommit func() (string, error)
	// skew is the skew guard's town-wide reads, or nil when the check builds
	// the production ones from the town root. A test sets agreeing reads.
	skew *pairReadySkewReads
	// skewCache remembers the guard's reads of immutable commits across passes.
	skewCache *pairReadySkewCache
}

// pairReadySkewCache remembers what the skew guard reads from commits, whose
// content cannot change: the bd commit a green commit's image pins name, keyed
// rig:sha, and the schema level a bd commit carries. A failed read is not
// stored, so the next pass asks again. Each map starts over at
// pairReadySkewKeep, so a town's history of greens and pins cannot grow it
// without bound.
type pairReadySkewCache struct {
	mu     sync.Mutex
	pins   map[string]string
	levels map[string]int
}

// pinOf returns the bd commit cached for a green commit.
func (c *pairReadySkewCache) pinOf(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pin, ok := c.pins[key]
	return pin, ok
}

// keepPin records the bd commit a green commit names.
func (c *pairReadySkewCache) keepPin(key, pin string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pins) >= pairReadySkewKeep {
		c.pins = nil
	}
	if c.pins == nil {
		c.pins = make(map[string]string, pairReadySkewKeep)
	}
	c.pins[key] = pin
}

// levelOf returns the schema level cached for a bd commit.
func (c *pairReadySkewCache) levelOf(commit string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	level, ok := c.levels[commit]
	return level, ok
}

// keepLevel records the schema level a bd commit carries.
func (c *pairReadySkewCache) keepLevel(commit string, level int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.levels) >= pairReadySkewKeep {
		c.levels = nil
	}
	if c.levels == nil {
		c.levels = make(map[string]int, pairReadySkewKeep)
	}
	c.levels[commit] = level
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
	readRunning := c.runningCommit
	if readRunning == nil {
		readRunning = func() (string, error) { return pairReadyRunningCommit(townRoot) }
	}
	running, err := readRunning()
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
	refused := c.readRefused(townRoot, logf)
	skew := c.skewPass(townRoot)
	prev, fails, failed := c.snapshot()
	out := make(map[string][]attention.Item, len(list))
	next := make(map[string]int, len(list))
	nextFailed := make(map[string]pairReadyFailedRun)
	for _, r := range list {
		if r.Err != nil {
			logf("deploy pair check: %s: %v", r.Rig, r.Err)
			next[r.Rig] = fails[r.Rig] + 1
			out[r.Rig] = pairReadyKept(prev[r.Rig], next[r.Rig], pairReadyUnreadableItem(r.Rig, r.Err), refused, running)
			carryFailedRuns(nextFailed, failed, out[r.Rig])
			continue
		}
		items, found, scanErr, err := pairReadyItems(ctx, r, running, now, prev[r.Rig], failed, refused, skew, logf)
		for key, f := range found {
			nextFailed[key] = f
		}
		switch {
		case err != nil:
			logf("deploy pair check: %s: %v", r.Rig, err)
			next[r.Rig] = fails[r.Rig] + 1
			out[r.Rig] = pairReadyKept(prev[r.Rig], next[r.Rig], pairReadyUnreadableItem(r.Rig, err), refused, running)
		case scanErr != nil:
			logf("deploy pair check: %s: %v", r.Rig, scanErr)
			next[r.Rig] = fails[r.Rig] + 1
			out[r.Rig] = pairReadyKept(items, next[r.Rig], pairReadyUnreadableItem(r.Rig, scanErr), refused, running)
		default:
			out[r.Rig] = items
		}
		carryFailedRuns(nextFailed, failed, out[r.Rig])
	}
	if !refused.known {
		// The pair answer is unmeasured while the ledger cannot be read, so the
		// queue is told that once for the town, beside the rig items the pass
		// still raised.
		out[pairReadyTownState] = []attention.Item{pairReadyRefusalUnreadableItem(refused.err)}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = out
	c.fails = next
	c.failed = nextFailed
}

// skewPass is one pass's skew guard: its reads and the level the database was
// at when the pass asked. The database level is read once per pass, since a
// migration can move it while the pass runs, and a failed read is settled for
// the rest of the pass so a broken store is not asked once per green commit.
// c.skew is nil in production, where the reads come from the town root.
func (c *pairReadyCheck) skewPass(townRoot string) *pairReadySkewPass {
	reads := c.skew
	if reads == nil {
		reads = pairReadySkewReadsAt(townRoot, pairReadySkewGitRun, func(ctx context.Context) (pairReadySchemaStore, error) {
			return deps.NewHandshakeStore(ctx, townRoot), nil
		})
	}
	c.mu.Lock()
	if c.skewCache == nil {
		c.skewCache = &pairReadySkewCache{}
	}
	cache := c.skewCache
	c.mu.Unlock()
	return &pairReadySkewPass{
		reads: reads,
		cache: cache,
		owner: func() (string, error) { return config.ResolveForgeOwner(townRoot) },
	}
}

// pairReadySkewPass is one pass's skew reads: the memoized pin and level reads,
// and the database level asked at most once.
type pairReadySkewPass struct {
	reads *pairReadySkewReads
	cache *pairReadySkewCache
	// owner expands the {owner} placeholder an image-pins file can carry. It is
	// called only when the file holds the placeholder.
	owner  func() (string, error)
	db     int
	dbRead bool
	dbErr  error
}

// database returns the live database's schema level, read once per pass.
func (p *pairReadySkewPass) database(ctx context.Context) (int, error) {
	if p.dbRead {
		return p.db, p.dbErr
	}
	p.dbRead = true
	if p.reads.Database == nil {
		p.dbErr = errors.New("the database's schema level read is not wired")
		return 0, p.dbErr
	}
	p.db, p.dbErr = p.reads.Database(ctx)
	return p.db, p.dbErr
}

// levelAt returns the schema level the bd commit carries, from the cache when an
// earlier pass read it, since a commit's content cannot change. A failed read is
// not cached.
func (p *pairReadySkewPass) levelAt(ctx context.Context, commit string) (int, error) {
	if level, ok := p.cache.levelOf(commit); ok {
		return level, nil
	}
	if p.reads.LevelAt == nil {
		return 0, errors.New("the bd schema level read is not wired")
	}
	level, err := p.reads.LevelAt(ctx, commit)
	if err != nil {
		return 0, err
	}
	p.cache.keepLevel(commit, level)
	return level, nil
}

// bdPin returns the bd commit green's image pins name, from the cache when an
// earlier pass read it.
func (p *pairReadySkewPass) bdPin(r pairReadyRig, g pairReadyGreen) (string, error) {
	key := r.Rig + ":" + g.sha
	if pin, ok := p.cache.pinOf(key); ok {
		return pin, nil
	}
	if r.Pins == nil {
		return "", errors.New("the rig has no local clone to read the bd pin from")
	}
	text, err := r.Pins(g.sha)
	if err != nil {
		return "", fmt.Errorf("reading %s at green/%s: %w", daemonimage.PinsPath, g.sha12, err)
	}
	pins, err := parseImagePins([]byte(text), p.owner)
	if err != nil {
		return "", fmt.Errorf("reading the bd pin from %s at green/%s: %w", daemonimage.PinsPath, g.sha12, err)
	}
	pin := pairReadyBdPin(pins)
	p.cache.keepPin(key, pin)
	return pin, nil
}

// pairReadyBdPin returns the bd commit pins names. parseImagePins refuses a file
// that does not name each of daemonimage.PinNames once, so bd is always there;
// should it be missing anyway, the empty answer is not a full sha and the lister
// refuses it, so it is an error and never a level.
func pairReadyBdPin(pins []daemonimage.Pin) string {
	for _, pin := range pins {
		if pin.Name == "bd" {
			return pin.Commit
		}
	}
	return ""
}

// pairReadySkewFor compares the schema levels for one green commit whose pair is
// published. It returns the bd pin and the skew when the levels disagree, and a
// nil skew when they agree. An error names the read that could not be made, so
// the caller raises neither a skew item nor a ready one.
func pairReadySkewFor(ctx context.Context, r pairReadyRig, g pairReadyGreen, pass *pairReadySkewPass) (string, *bdskew.Skew, error) {
	pin, err := pass.bdPin(r, g)
	if err != nil {
		return "", nil, err
	}
	level, err := pass.levelAt(ctx, pin)
	if err != nil {
		return pin, nil, fmt.Errorf("the schema level bd %s carries: %w", greenRefShortSHA(pin), err)
	}
	database, err := pass.database(ctx)
	if err != nil {
		return pin, nil, fmt.Errorf("the database's schema level: %w", err)
	}
	return pin, bdskew.Compare(level, database), nil
}

// readRefused reads the update ledger's refused pair. known is false when the
// ledger could not be read: a read that did not answer says nothing about what
// the ledger records, so the check raises no pair-ready item this pass (the
// published pair may be the refused one) and keeps the refused items it holds,
// since the ledger could not say the refusal is gone. The outage is logged once,
// when the error changes, so a damaged ledger is reported without a line per
// pass.
func (c *pairReadyCheck) readRefused(townRoot string, logf func(string, ...any)) pairReadyRefusal {
	read := c.ledger
	if read == nil {
		read = func(townRoot string) (update.ImageRecord, error) {
			return update.FileImageLedger{Dir: update.Dir(townRoot)}.Read()
		}
	}
	rec, err := read(townRoot)
	c.mu.Lock()
	log := err != nil && err.Error() != c.ledgerErr
	text := ""
	if err != nil {
		text = err.Error()
	}
	c.ledgerErr = text
	c.mu.Unlock()
	if err != nil {
		if log {
			logf("deploy pair check: the update ledger could not be read: %v; no pair-ready item is raised from it", err)
		}
		return pairReadyRefusal{err: err}
	}
	if rec.Refused.Commit == "" {
		return pairReadyRefusal{known: true}
	}
	return pairReadyRefusal{known: true, commit: rec.Refused.Commit, reason: strings.TrimSpace(rec.RefusedReason)}
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
// check raises: a pair ready to roll, stuck or skewed, a pair the update ledger
// refuses, or an image build that failed or whose status is unknown for the
// commit.
func pairReadyCommitItem(kind attention.Kind) bool {
	return kind == attention.KindDeployPairReady || kind == attention.KindDeployPairStuck ||
		kind == attention.KindDeployPairRefused || kind == attention.KindDeployPairSkew ||
		kind == attention.KindImagesFailed || kind == attention.KindImagesUnknown
}

// pairReadyKeptItem reports whether an item the check holds is one it rebuilds
// with its commit each pass: a per-commit verdict, or the skew guard's
// per-commit unreadable item, which is re-judged with the commit it names. The
// rig's own unreadable item is rebuilt beside these and is not one of them.
func pairReadyKeptItem(it attention.Item) bool {
	return pairReadyCommitItem(it.Kind) || strings.HasPrefix(it.Key, pairReadySkewUnreadableKeyPrefix)
}

// carryFailedRuns keeps the failed-run answer of each images-failed item a rig
// keeps after its read, so a read that did not reach the commit does not make
// the next read fetch the job and its log again. An answer the read settled this
// pass is already in next and wins.
func carryFailedRuns(next, failed map[string]pairReadyFailedRun, items []attention.Item) {
	for _, it := range items {
		if _, done := next[it.Key]; done || it.Kind != attention.KindImagesFailed {
			continue
		}
		if f, ok := failed[it.Key]; ok {
			next[it.Key] = f
		}
	}
}

// pairReadyKept is what a rig holds after a read that did not answer: what the
// read still confirms, with the check's unreadable item naming why. Before the
// failure bound that is a partial scan's items or what the rig held; past it the
// stale per-commit items are dropped, so an item the check cannot confirm for
// good does not outlive the check that raised it. A refused item is dropped only
// when pairRefusedStale says the read can no longer confirm it: the ledger, not
// the branch read, is what raises and clears it, so a readable ledger that still
// names the commit leaves the item standing.
func pairReadyKept(prev []attention.Item, fails int, unreadable attention.Item, refused pairReadyRefusal, running string) []attention.Item {
	var out []attention.Item
	if fails < pairReadyForgetAfter {
		for _, it := range prev {
			if !pairReadyKeptItem(it) {
				continue
			}
			if it.Kind == attention.KindDeployPairRefused && pairRefusedStale(it, refused, running) {
				continue
			}
			out = append(out, it)
		}
	}
	return append(out, unreadable)
}

// pairRefusedStale reports whether a held refused item is one the check can no
// longer confirm: the running commit reached the item's commit, or the ledger
// answered and does not name that commit. A ledger that could not be read leaves
// the item, since it cannot say the refusal is gone.
func pairRefusedStale(it attention.Item, refused pairReadyRefusal, running string) bool {
	if it.SHA == running {
		return true
	}
	return refused.known && it.SHA != refused.commit
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

// imagesAnswerFor reports what the check knows about rig's green commit sha,
// whose first 12 characters key its items: failed when it holds a failed images
// run for the commit, published when it holds the commit's pair of images in the
// registry (the images run succeeded), and neither when it holds no answer. A
// skewed pair holds the images just as a ready one does, so it answers published
// too: the images are there, only the roll is refused (gt-5f0kue). A check that
// has not run, or whose read did not reach the commit, holds neither answer,
// which is no success (gt-ogt6jh, gt-opkfch).
func (c *pairReadyCheck) imagesAnswerFor(rig, sha string) (failed, published bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sha12 := greenRefShortSHA(sha)
	if _, ok := c.failed[imagesFailedKey(rig, sha12)]; ok {
		return true, false
	}
	key := pairReadyKey(rig, sha12)
	skewKey := pairReadySkewKey(rig, sha12)
	for _, it := range c.held[rig] {
		if it.Key == key && it.Kind == attention.KindDeployPairReady {
			return false, true
		}
		if it.Key == skewKey && it.Kind == attention.KindDeployPairSkew {
			return false, true
		}
	}
	return false, false
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
// the registry at its stamp, one for each green commit newer than running that
// the update ledger records as refused, one for each commit whose pair is
// missing past pairReadyStuckAfter, and one for each commit whose pair is
// missing and whose images workflow run ended failed or canceled. A read that
// cannot list the branches returns an error, so the caller keeps what the rig
// held until the failure bound. A listing that ended early or a branch whose
// comparison or image read did not answer returns a non-nil scanErr with the
// items it did confirm plus the items from prev whose branch it did not judge: a
// scan that could not read a branch cannot say its pair is gone, and one
// branch's failure does not discard the pass. An image that is not there is not
// an error: one image alone raises nothing. A failed run list does not stop the
// pass either: that commit raises its images-unknown item and the pass goes on. A
// ledger the check could not read suppresses the pair-ready item, since a
// published pair may be the one the pre-swap check refused; a refused item the
// rig holds stays. A published pair is checked against the bd pin's schema
// level: a skew raises the skew item instead of the ready one, and a level the
// pass cannot read raises the guard's unreadable item beside the ready one, so
// an unread level never passes for agreement. found is the failed runs this read
// settled, for the next read to reuse. A missing pair behind a ready green raises
// nothing, since the roll takes the ready green.
func pairReadyItems(ctx context.Context, r pairReadyRig, running string, now time.Time, prev []attention.Item, runs map[string]pairReadyFailedRun, refused pairReadyRefusal, skew *pairReadySkewPass, logf func(string, ...any)) (items []attention.Item, found map[string]pairReadyFailedRun, scanErr error, err error) {
	scan, err := pairReadyGreenCommits(ctx, r)
	if err != nil {
		return nil, nil, nil, err
	}
	var out []attention.Item
	found = make(map[string]pairReadyFailedRun)
	// seen holds the items this pass judged: the pair and the build items of a
	// commit it read to a verdict. A branch a failed read stopped at is not
	// seen, so its previous items are kept and a stale one is only cleared by a
	// read that looked at it.
	seen := make(map[string]bool, len(scan.greens)*2)
	mark := func(sha12 string) {
		seen[pairReadyKey(r.Rig, sha12)] = true
		seen[pairReadyStuckKey(r.Rig, sha12)] = true
		seen[imagesFailedKey(r.Rig, sha12)] = true
		seen[imagesUnknownKey(r.Rig, sha12)] = true
		seen[pairReadySkewKey(r.Rig, sha12)] = true
		seen[pairReadySkewUnreadableKey(r.Rig, sha12)] = true
	}
	// refusedSettled names the refused items this pass decided: the commit it
	// judged, whether the verdict was that the pair is refused or that it is not
	// newer. Only judging the refused commit settles its item, so a scan that
	// broke before reaching it leaves the item standing while the ledger names
	// it.
	refusedSettled := make(map[string]bool, 1)
	// ready is the commits that raised their pair-ready item, and missing is each
	// green whose pair is missing with the items the pass raised for it. A green
	// that is skewed, whose level cannot be read, or that the ledger does not
	// clear to raise a pair-ready item is not ready, so it supersedes nothing. A
	// missing pair behind a ready green is superseded after the loop.
	var ready []string
	var missing []pairReadyMissing
	incomplete := scan.err
	for _, g := range scan.greens {
		if g.sha == running {
			mark(g.sha12)
			continue
		}
		v, err := pairReadyJudge(ctx, r, g, running)
		if err != nil {
			incomplete = err
			break
		}
		// The ledger's refusal is what is known about the commit: while it names
		// this commit and the commit is newer, the pair is refused, so the item
		// that says so replaces the pair-ready item the registry would raise, and
		// the commit's build items are settled too. A commit that is not newer is
		// the running one's or behind it, which is what clears the item.
		if refused.known && refused.commit == g.sha {
			mark(g.sha12)
			refusedSettled[pairReadyRefusedKey(r.Rig, g.sha12)] = true
			if v.newer {
				out = append(out, pairReadyRefusedItem(r.Rig, g, refused.reason))
			}
			continue
		}
		if !v.newer {
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
			// The ledger is what says a published pair was not refused. A ledger
			// the check could not read leaves it unmeasured, so no item that
			// says the pair is ready is raised from it.
			if !refused.known {
				continue
			}
			pin, skewed, err := pairReadySkewFor(ctx, r, g, skew)
			if err != nil {
				// The levels are unread, so no ready item is raised: the update
				// patrol rolls only a ready item, and an unread level must not
				// start a roll (gt-5f0kue). The unreadable item holds the commit
				// until a later pass reads the levels.
				logf("deploy pair check: %s: green/%s: %v", r.Rig, g.sha12, err)
				out = append(out, pairReadySkewUnreadableItem(r.Rig, g, err))
				continue
			}
			if skewed != nil {
				out = append(out, pairReadySkewItem(r.Rig, g, pin, *skewed))
				continue
			}
			// Only the pair-ready item makes the green ready: a roll takes it, so a
			// green that raised no such item must not supersede an older build.
			ready = append(ready, g.sha)
			out = append(out, pairReadyItem(r.Rig, g, running, daemon, polecat, v))
			continue
		}
		// The pair is missing, so the commit is aged before its run is read: a
		// commit past the stuck bound raises its stuck item whether or not a run
		// exists. Only such a commit asks Forgejo about its run, and the check
		// settles the commit's build items here so the incomplete path never has to.
		var raised []attention.Item
		if age, ok := pairReadyGreenAge(r, g, now, logf); ok && age >= pairReadyStuckAfter {
			raised = append(raised, pairReadyStuckItem(r.Rig, g, age))
		}
		items, settled := pairReadyImagesFailed(ctx, r, g, prev, runs, logf)
		raised = append(raised, items...)
		out = append(out, raised...)
		missing = append(missing, pairReadyMissing{sha: g.sha, sha12: g.sha12, items: raised})
		if settled != nil {
			found[imagesFailedKey(r.Rig, g.sha12)] = *settled
		}
	}
	// A missing pair behind a ready green can never be rolled: the roll takes the
	// newer green. Its items and failed-run entry are dropped here, its keys stay
	// seen, so the previous items clear. An ancestry that cannot be judged keeps
	// the items and makes the scan incomplete.
	dropped := make(map[string]bool)
	for _, m := range missing {
		behind, err := pairReadyBehindAny(r, m.sha, ready)
		if err != nil {
			incomplete = err
			continue
		}
		if !behind {
			continue
		}
		for _, it := range m.items {
			dropped[it.Key] = true
		}
		delete(found, imagesFailedKey(r.Rig, m.sha12))
	}
	if len(dropped) > 0 {
		kept := out[:0]
		for _, it := range out {
			if !dropped[it.Key] {
				kept = append(kept, it)
			}
		}
		out = kept
	}
	if incomplete != nil {
		for _, it := range prev {
			if !pairReadyKeptItem(it) {
				continue
			}
			if it.Kind == attention.KindDeployPairRefused {
				// A refusal is raised and cleared by the ledger, not by the
				// branch read, so a commit the scan broke before judging keeps
				// its item whichever green the break was on; pairReadyKept
				// settles what it lifts against the ledger.
				if refusedSettled[it.Key] {
					continue
				}
				out = append(out, it)
				continue
			}
			if seen[it.Key] {
				continue
			}
			out = append(out, it)
		}
		return out, found, incomplete, nil
	}
	if !refused.known {
		// A complete scan still could not settle the refusal, which the ledger
		// alone answers, so a refused item the rig holds stays until a readable
		// ledger clears it or the running commit reaches it.
		for _, it := range prev {
			if it.Kind != attention.KindDeployPairRefused {
				continue
			}
			if pairRefusedStale(it, refused, running) {
				continue
			}
			out = append(out, it)
		}
	}
	return out, found, nil, nil
}

// pairReadyMissing is a green commit whose pair is missing, with the items the
// pass raised for it.
type pairReadyMissing struct {
	sha   string
	sha12 string
	items []attention.Item
}

// pairReadyBehindAny says whether commit is an ancestor of one of the ready
// greens, in the rig's local clone. A rig with no clone cannot judge that, so it
// is an error, not a no.
func pairReadyBehindAny(r pairReadyRig, commit string, ready []string) (bool, error) {
	if len(ready) == 0 {
		return false, nil
	}
	if r.Git == nil {
		return false, fmt.Errorf("green/%s: the rig has no local clone to judge it behind a ready green", greenRefShortSHA(commit))
	}
	for _, sha := range ready {
		behind, err := r.Git.IsAncestor(commit, sha)
		if err != nil {
			return false, fmt.Errorf("judging green/%s behind the ready green %s in the local clone: %w", greenRefShortSHA(commit), greenRefShortSHA(sha), err)
		}
		if behind {
			return true, nil
		}
	}
	return false, nil
}

// pairReadyScan is one rig's green listing. complete says the listing was read
// to its answer; err says why it was not, and a failed listing reads nothing.
type pairReadyScan struct {
	greens   []pairReadyGreen
	complete bool
	err      error
}

// pairReadyGreenCommits lists every green/<sha12> ref of the rig's repository
// in one call. A ref whose name does not match the commit it points at is an
// error: the stamp the registry is read by would be wrong. A 404 is ambiguous,
// since a wrong or moved repository name answers it too, so one read of the
// repository decides: a readable repository has no green refs, which is a
// complete and empty scan; an unreadable one leaves the scan incomplete naming
// the repository. Any other failed listing leaves the scan incomplete with
// nothing read.
func pairReadyGreenCommits(ctx context.Context, r pairReadyRig) (pairReadyScan, error) {
	refs, err := r.API.ListRefs(ctx, r.Owner, r.Repo, pairReadyBranchPrefix)
	if err != nil {
		var apiErr *forgejo.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			if _, repoErr := r.API.Repository(ctx, r.Owner, r.Repo); repoErr != nil {
				return pairReadyScan{err: fmt.Errorf("the green refs of %s/%s answered 404 and the repository is not readable: %w", r.Owner, r.Repo, repoErr)}, nil
			}
			return pairReadyScan{complete: true}, nil
		}
		return pairReadyScan{err: fmt.Errorf("listing the green refs of %s/%s: %w", r.Owner, r.Repo, err)}, nil
	}
	var out []pairReadyGreen
	for _, ref := range refs {
		stamp, ok := strings.CutPrefix(ref.Name, pairReadyBranchPrefix)
		if !ok {
			continue
		}
		if !imagevalues.ValidCommit(ref.SHA) || greenRefShortSHA(ref.SHA) != stamp {
			return pairReadyScan{}, fmt.Errorf("the branch %s points at commit %q, which its name does not carry", ref.Name, ref.SHA)
		}
		out = append(out, pairReadyGreen{sha: ref.SHA, sha12: stamp})
	}
	return pairReadyScan{greens: out, complete: true}, nil
}

// pairReadyKey is one pair item's key: deploy-pair-ready:<rig>:<sha12>.
func pairReadyKey(rig, sha12 string) string {
	return pairReadyKeyPrefix + rig + ":" + sha12
}

// pairReadyVerdict is how one green commit compares with the running one. newer
// says the green commit is ahead of it. ahead is the commit count the API gave,
// or -1 when the local clone judged it and counted nothing. note says why the
// API answered, when a commit was missing from the clone, and is "" otherwise.
type pairReadyVerdict struct {
	newer bool
	ahead int
	note  string
}

// pairReadyNoCloneNote is on a verdict the Forgejo comparison answered because
// the rig has no bare clone to read.
const pairReadyNoCloneNote = "; the comparison was read from Forgejo, since the rig has no local clone"

// pairReadyJudge decides whether green is newer than running. Both commits in
// the rig's bare clone answer it locally: running is an ancestor of green, and
// they differ (the caller has ruled out equal). An older or diverged green is
// not newer, with no Forgejo call. A commit the clone lacks falls back to the
// Forgejo comparison, and the verdict's note names that commit. A rig with no
// clone at all also falls back, and its note says so. A clone that cannot be
// read is an error, not a fallback, so a broken clone is not read as no pair.
func pairReadyJudge(ctx context.Context, r pairReadyRig, g pairReadyGreen, running string) (pairReadyVerdict, error) {
	if r.Git == nil {
		v, err := pairReadyCompareAPI(ctx, r, g, running)
		if err != nil {
			return pairReadyVerdict{}, err
		}
		v.note = pairReadyNoCloneNote
		return v, nil
	}
	for _, sha := range []string{running, g.sha} {
		has, err := r.Git.HasObject(sha)
		if err != nil {
			return pairReadyVerdict{}, fmt.Errorf("reading the local clone of %s for green/%s: %w", r.Rig, g.sha12, err)
		}
		if has {
			continue
		}
		v, err := pairReadyCompareAPI(ctx, r, g, running)
		if err != nil {
			return pairReadyVerdict{}, err
		}
		v.note = fmt.Sprintf("; the comparison was read from Forgejo, since commit %s is not in the local clone", greenRefShortSHA(sha))
		return v, nil
	}
	newer, err := r.Git.IsAncestor(running, g.sha)
	if err != nil {
		return pairReadyVerdict{}, fmt.Errorf("judging green/%s against the running commit %s in the local clone: %w", g.sha12, greenRefShortSHA(running), err)
	}
	return pairReadyVerdict{newer: newer, ahead: -1}, nil
}

// pairReadyCompareAPI judges green against running with two Forgejo comparisons:
// running is an ancestor of green when no commit of running is missing from it,
// and green is ahead when it has commits running does not.
func pairReadyCompareAPI(ctx context.Context, r pairReadyRig, g pairReadyGreen, running string) (pairReadyVerdict, error) {
	behind, err := r.API.CompareCommits(ctx, r.Owner, r.Repo, g.sha, running)
	if err != nil {
		return pairReadyVerdict{}, fmt.Errorf("comparing green/%s with the running commit %s: %w", g.sha12, greenRefShortSHA(running), err)
	}
	if behind.TotalCommits != 0 {
		return pairReadyVerdict{}, nil
	}
	ahead, err := r.API.CompareCommits(ctx, r.Owner, r.Repo, running, g.sha)
	if err != nil {
		return pairReadyVerdict{}, fmt.Errorf("counting the commits of green/%s ahead of the running commit: %w", g.sha12, err)
	}
	return pairReadyVerdict{newer: ahead.TotalCommits != 0, ahead: ahead.TotalCommits}, nil
}

// pairReadyItem is the item for one green commit with both images published.
// Its summary names the stamp, both digests and how the green commit is newer
// than the running one: the commits it is ahead by when the API counted them, or
// that the local clone judged it. A comparison the API answered for a missing
// commit, or for a rig with no clone, says so in the note.
func pairReadyItem(rig string, g pairReadyGreen, running, daemonDigest, polecatDigest string, v pairReadyVerdict) attention.Item {
	newer := "newer than the running commit " + greenRefShortSHA(running) + ", judged from the local clone"
	if v.ahead >= 0 {
		newer = fmt.Sprintf("%d commits ahead of the running commit %s", v.ahead, greenRefShortSHA(running))
	}
	return attention.Item{
		Key:      pairReadyKey(rig, g.sha12),
		Kind:     attention.KindDeployPairReady,
		Severity: attention.SeverityLow,
		Rig:      rig,
		SHA:      g.sha,
		Summary: fmt.Sprintf("the images for green/%s are published: daemon %s and polecat %s, %s; roll the daemon onto them%s",
			g.sha12, daemonDigest, polecatDigest, newer, v.note),
	}
}

// pairReadySkewKey is one skewed pair's key: deploy-pair-skew:<rig>:<sha12>.
func pairReadySkewKey(rig, sha12 string) string {
	return pairReadySkewPrefix + rig + ":" + sha12
}

// pairReadySkewItem is the item for a green commit whose published pair names a
// bd pin at another schema level than the live database's: the daemon the roll
// would start is refused at its startup handshake, so no pair-ready item is
// raised beside it.
func pairReadySkewItem(rig string, g pairReadyGreen, pin string, skew bdskew.Skew) attention.Item {
	return attention.Item{
		Key:      pairReadySkewKey(rig, g.sha12),
		Kind:     attention.KindDeployPairSkew,
		Severity: attention.SeverityHigh,
		Rig:      rig,
		SHA:      g.sha,
		Summary: fmt.Sprintf("green/%s pins bd %s at schema level %d but the database is at %d, so the roll would start a daemon its startup handshake refuses",
			g.sha12, greenRefShortSHA(pin), skew.Pin, skew.Database),
	}
}

// pairReadySkewUnreadableKeyPrefix keys the skew guard's per-commit unreadable
// item: deploy-pair-unreadable:skew:<rig>:<sha12>.
const pairReadySkewUnreadableKeyPrefix = pairReadyUnreadablePrefix + "skew:"

// pairReadySkewUnreadableKey is one commit's unread skew key.
func pairReadySkewUnreadableKey(rig, sha12 string) string {
	return pairReadySkewUnreadableKeyPrefix + rig + ":" + sha12
}

// pairReadySkewUnreadableItem is the item for a published pair whose bd pin's
// schema level the pass could not read. It stands in place of the pair-ready
// item, never beside a skew, so "could not verify" is raised as itself and an
// unread level never passes for agreement. The item fails the roll closed: the
// update patrol rolls only a ready item. The read failure is quoted as data.
func pairReadySkewUnreadableItem(rig string, g pairReadyGreen, err error) attention.Item {
	return attention.Item{
		Key:      pairReadySkewUnreadableKey(rig, g.sha12),
		Kind:     attention.KindDeployPairUnreadable,
		Severity: attention.SeverityMedium,
		Rig:      rig,
		SHA:      g.sha,
		Summary: fmt.Sprintf("green/%s is not announced ready: the schema level of its bd pin could not be read, so the pair is not verified free of skew and no roll starts until it can be read: %s",
			g.sha12, oneLine(err.Error())),
	}
}

// pairReadyRefusedKey is one refused pair's key:
// deploy-pair-refused:<rig>:<sha12>. The commit is what it names, so a commit
// the ledger keeps refused keeps one item, and the pass after the ledger clears
// it stops raising it.
func pairReadyRefusedKey(rig, sha12 string) string {
	return pairReadyRefusedPrefix + rig + ":" + sha12
}

// pairReadyRefusedItem is the item for a green commit the update ledger records
// as refused: it names the rig, the commit and the reason the pre-swap check
// recorded, so the operator sees which check refused the pair and why.
func pairReadyRefusedItem(rig string, g pairReadyGreen, reason string) attention.Item {
	why := "the pre-swap check recorded no reason"
	if reason != "" {
		why = "the pre-swap check refused it: " + oneLine(reason)
	}
	return attention.Item{
		Key:      pairReadyRefusedKey(rig, g.sha12),
		Kind:     attention.KindDeployPairRefused,
		Severity: attention.SeverityHigh,
		Rig:      rig,
		SHA:      g.sha,
		Summary:  fmt.Sprintf("green/%s was refused: %s; no roll starts from it", g.sha12, why),
	}
}

// pairReadyStuckKey is one stuck pair's key: deploy-pair-stuck:<rig>:<sha12>.
func pairReadyStuckKey(rig, sha12 string) string {
	return pairReadyStuckPrefix + rig + ":" + sha12
}

// pairReadyGreenAge is how long before now the green commit was made, read from
// the commit the ref points at, whose time stands in for the push time since
// the ref is the landing worker's own merge commit. ok is false for a ref the
// clone cannot date — no clone, a failed read, or no time — which raises no
// stuck item and is logged.
func pairReadyGreenAge(r pairReadyRig, g pairReadyGreen, now time.Time, logf func(string, ...any)) (time.Duration, bool) {
	git, ok := r.Git.(pairReadyAged)
	if !ok {
		logf("deploy pair check: %s: green/%s is not aged, since the rig has no local clone that dates a commit", r.Rig, g.sha12)
		return 0, false
	}
	at, err := git.CommitTime(g.sha)
	switch {
	case err != nil:
		logf("deploy pair check: %s: green/%s is not aged, since its commit time could not be read: %v", r.Rig, g.sha12, err)
		return 0, false
	case at.IsZero():
		logf("deploy pair check: %s: green/%s is not aged, since its commit carries no time", r.Rig, g.sha12)
		return 0, false
	}
	return now.Sub(at), true
}

// pairReadyStuckItem is the item for a green commit whose pair is still
// unpublished past pairReadyStuckAfter.
func pairReadyStuckItem(rig string, g pairReadyGreen, age time.Duration) attention.Item {
	return attention.Item{
		Key:      pairReadyStuckKey(rig, g.sha12),
		Kind:     attention.KindDeployPairStuck,
		Severity: attention.SeverityHigh,
		Rig:      rig,
		SHA:      g.sha,
		Summary: fmt.Sprintf("green/%s is %d minutes old and its pair is not published; no roll can start from it",
			g.sha12, int(age/time.Minute)),
	}
}

// imagesFailedKey is one failed image build's key:
// images-failed:<rig>:<sha12>. The commit is what it names, so the same commit
// re-scanned keeps one item and a later green run clears it by not re-raising
// it.
func imagesFailedKey(rig, sha12 string) string {
	return imagesFailedKeyPrefix + rig + ":" + sha12
}

// imagesUnknownKey is one unknown build status's key:
// images-unknown:<rig>:<sha12>. It is one per commit, like the failed item, and
// the next list that can be read replaces or clears it.
func imagesUnknownKey(rig, sha12 string) string {
	return imagesUnknownKeyPrefix + rig + ":" + sha12
}

// pairReadyHeld is the item of kind at key that prev holds, or nil.
func pairReadyHeld(prev []attention.Item, key string, kind attention.Kind) *attention.Item {
	for i := range prev {
		if prev[i].Key == key && prev[i].Kind == kind {
			it := prev[i]
			return &it
		}
	}
	return nil
}

// pairReadyImagesUnknownItem is the item for a commit whose run list could not
// be read. The error is quoted as data, since it is text Forgejo or a proxy
// wrote.
func pairReadyImagesUnknownItem(rig string, g pairReadyGreen, err error) attention.Item {
	return attention.Item{
		Key:      imagesUnknownKey(rig, g.sha12),
		Kind:     attention.KindImagesUnknown,
		Severity: attention.SeverityLow,
		Rig:      rig,
		SHA:      g.sha,
		Summary:  fmt.Sprintf("images status unknown for %s: the run list could not be read: %s", g.sha12, imagesLogText(err.Error())),
	}
}

// pairReadyImagesFailed judges the images workflow's newest run for a green
// commit whose pair is missing. A run that ended failed or canceled raises the
// item naming the commit and the first "Error:" line of the job it ran; a
// successful run raises nothing, which is what clears an item raised for the
// commit before. A run still in flight is no answer, and neither is a commit
// with no run in the list: the failed item the check last raised for the commit
// stands. A run list the check cannot read is no answer either, and it is not a
// clean commit: the commit gets an images-unknown item naming the error, and the
// failed item it held stands beside that, since the failure is still known. The
// unknown item is logged when it starts, not on each read it stays. settled is
// the failed run the read judged, with its run ID, for the next read to reuse;
// it is nil when the read settled no failed run.
func pairReadyImagesFailed(ctx context.Context, r pairReadyRig, g pairReadyGreen, prev []attention.Item, runs map[string]pairReadyFailedRun, logf func(string, ...any)) (items []attention.Item, settled *pairReadyFailedRun) {
	key := imagesFailedKey(r.Rig, g.sha12)
	unknownKey := imagesUnknownKey(r.Rig, g.sha12)
	// kept is the failed item the check held for the commit, if any: what a read
	// with no answer leaves standing.
	kept := func() []attention.Item {
		if it := pairReadyHeld(prev, key, attention.KindImagesFailed); it != nil {
			return []attention.Item{*it}
		}
		return nil
	}
	list, err := r.API.ListRuns(ctx, r.Owner, r.Repo, forgejo.RunFilter{
		WorkflowID: pairReadyImagesWorkflow, HeadSHA: g.sha, Limit: pairReadyRunLimit,
	})
	if err == nil && list == nil {
		err = errors.New("the list is not there")
	}
	if err != nil {
		// The streak logs once: a commit that already holds the unknown item has
		// been logged for this failing read, and the next readable list ends it.
		if pairReadyHeld(prev, unknownKey, attention.KindImagesUnknown) == nil {
			logf("deploy pair check: %s: reading the %s runs for green/%s: %v",
				r.Rig, pairReadyImagesWorkflow, g.sha12, err)
		}
		return append(kept(), pairReadyImagesUnknownItem(r.Rig, g, err)), nil
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
		return kept(), nil
	}
	switch run.Status {
	case deployRunFailed, deployRunCancelled:
		if c, ok := runs[key]; ok && c.runID == run.ID {
			it := c.item
			return []attention.Item{it}, &c
		}
		line, lineSettled := pairReadyImagesErrorLine(ctx, r, *run)
		it := attention.Item{
			Key:      key,
			Kind:     attention.KindImagesFailed,
			Severity: attention.SeverityHigh,
			Rig:      r.Rig,
			SHA:      g.sha,
			Summary:  fmt.Sprintf("images workflow failed for %s: %s", g.sha12, line),
		}
		if !lineSettled {
			return []attention.Item{it}, nil
		}
		return []attention.Item{it}, &pairReadyFailedRun{runID: run.ID, item: it}
	case deployRunSucceeded:
		return nil, nil
	}
	return kept(), nil
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
		return "the job log could not be read: " + imagesLogText(err.Error()), false
	}
	jobID, ok := pairReadyFailedJob(jobs)
	if !ok {
		return "no failed job found, so no log was read", true
	}
	log, err := r.API.JobLogs(ctx, r.Owner, r.Repo, jobID)
	if err != nil {
		return "the job log could not be read: " + imagesLogText(err.Error()), false
	}
	if errLine := imagesErrorLine(log); errLine != "" {
		return imagesLogText(errLine), true
	}
	return "the job log names no Error: line", true
}

// imagesLogText is a line of CI output as the summary states it: a job-log line,
// or the text of an error read from the job list or the log. That text is
// untrusted, so whitespace becomes a space, other control and non-printing
// characters are dropped, the line keeps the 160-character cap, and it is quoted
// as CI output so it reads as data. A double quote in the line becomes a single
// one, so the quoting stays unambiguous.
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

// pairReadyRefusalUnreadableItem is the item for an update ledger the check
// could not read: the town-wide pair answer is unmeasured, so no pair-ready item
// is raised from it.
func pairReadyRefusalUnreadableItem(err error) attention.Item {
	return attention.Item{
		Key:      pairReadyUnreadablePrefix + "town:update-ledger",
		Kind:     attention.KindDeployPairUnreadable,
		Severity: attention.SeverityLow,
		Summary:  fmt.Sprintf("the pair check could not read the update ledger: %s; no pair-ready item is raised from it", oneLine(err.Error())),
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
	d.pairReady.runningCommit = d.runningCommit
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
	tokenDir, tokenErr := pairReadyTokenDir(d.patrol())
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
	out := pairReadyRigFrom(name, fc, err, tokenDir, tokenErr)
	out.Git = d.pairReadyBareClone(name)
	out.Pins = pairReadyPinsAt(out.Git)
	return out
}

// pairReadyPinsAt reads the image-pins file at a commit through the rig's bare
// clone. A nil clone, or one that cannot read a file at a commit, is no reader:
// the skew guard then reports the pin unread rather than the pair ready.
func pairReadyPinsAt(clone pairReadyGit) pairReadyPinsRead {
	if clone == nil {
		return nil
	}
	reader, ok := clone.(pairReadyPinsReader)
	if !ok {
		return nil
	}
	return func(sha string) (string, error) {
		return reader.ShowFileAtRev(sha, daemonimage.PinsPath)
	}
}

// pairReadySkewReadsAt builds the skew guard's town-wide reads for the town at
// townRoot: the schema level a bd commit carries, read through run over the
// beads rig's bare clone, and the live database's level, read through open. A
// nil run or open leaves that read unwired, and the guard then reports the pair
// unread rather than ready.
func pairReadySkewReadsAt(townRoot string, run func(ctx context.Context, dir string, args ...string) ([]byte, error), open func(context.Context) (pairReadySchemaStore, error)) *pairReadySkewReads {
	reads := &pairReadySkewReads{}
	if run != nil {
		lister := bdskew.GitLister{Run: run, Repo: bdskew.BeadsRigRepo(townRoot)}
		reads.LevelAt = func(ctx context.Context, commit string) (int, error) {
			return bdskew.LevelAtCommit(ctx, lister, commit)
		}
	}
	if open != nil {
		reads.Database = func(ctx context.Context) (int, error) {
			store, err := open(ctx)
			if err != nil {
				return 0, err
			}
			if store == nil {
				return 0, errors.New("no store to read the database's schema level from")
			}
			return store.SchemaLevel()
		}
	}
	return reads
}

// pairReadySkewGitRun runs git for the skew guard's lister under ctx.
func pairReadySkewGitRun(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: fixed binary; the lister validates the commit and dir
	cmd.Dir = dir
	return pairReadyGitOutput(cmd)
}

// pairReadyGitOutput runs cmd and returns its standard output. The error names
// the call, never git's output, which a failed git can fill with the repository
// path it was given. A command that cannot start is the failure a test drives
// in-process, so the branch is judged without a subprocess.
func pairReadyGitOutput(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(cmd.Args, " "), err)
	}
	return out, nil
}

// pairReadyBareClone opens the rig's bare clone, which the daemon keeps and
// this check only reads. The landing worker fetches that clone on every pass,
// so within a pass it is current; this check never fetches. A rig with no
// clone there has none: the check falls back to the Forgejo comparison rather
// than calling every green unreadable. A stat failure other than not-exist
// opens the git anyway, so a clone that is there but cannot be read still fails
// closed.
func (d *Daemon) pairReadyBareClone(name string) pairReadyGit {
	repo := constants.RigBareRepoPath(filepath.Join(d.config.TownRoot, name))
	if _, err := os.Stat(repo); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return d.gitAt(repo)
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

// collectImagesUnknown returns the commits whose run list the same read could
// not read, so their build status is unknown. It is a collector of its own, like
// the failed builds (gt-3vafwr).
func (s *attentionSources) collectImagesUnknown(context.Context) ([]attention.Item, error) {
	return s.pairReadyItemsOfKind(attention.KindImagesUnknown), nil
}

// collectPairStuck returns the stuck pairs the same read finds: one per green
// commit whose pair is still unpublished past pairReadyStuckAfter (gt-afwjb4).
func (s *attentionSources) collectPairStuck(context.Context) ([]attention.Item, error) {
	return s.pairReadyItemsOfKind(attention.KindDeployPairStuck), nil
}

// collectPairRefused returns the refused pairs the same read finds: one per
// green commit whose pair the update ledger records as refused by the pre-swap
// check (gt-t8vf15). It is a collector of its own, like the failed builds.
func (s *attentionSources) collectPairRefused(context.Context) ([]attention.Item, error) {
	return s.pairReadyItemsOfKind(attention.KindDeployPairRefused), nil
}

// collectPairSkew returns the skewed pairs the same read finds: one per green
// commit whose published pair names a bd pin at a schema level the live
// database does not have (gt-5f0kue). It is a collector of its own, for the
// same reason the pair's other kinds are.
func (s *attentionSources) collectPairSkew(context.Context) ([]attention.Item, error) {
	return s.pairReadyItemsOfKind(attention.KindDeployPairSkew), nil
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
