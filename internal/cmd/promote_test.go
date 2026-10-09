package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/promote"
)

const promoteTestRig = "fractals"

// promoteFixture is one rig cut over to Forgejo with a GitHub promote target:
// both repositories in gitfake, the rig's config, key file and red-main state
// on disk under a temporary town root. The command resolves the rig's block,
// reads Forgejo main in the rig's own repository and pushes to the target
// through the fixture's git surface, so no part of a test reaches the network.
type promoteFixture struct {
	town      string
	f         *gitfake.Fake
	repo      gitfake.Repo
	forgejo   string // the rig's Forgejo repository
	github    string // the rig's GitHub promote target
	key       string // the deploy key's path, which the command never reads
	base, sha string // Forgejo main's seed, and the green commit on top of it
	escalated []string
	out, errs bytes.Buffer
}

func newPromoteFixture(t *testing.T) *promoteFixture {
	t.Helper()
	town := t.TempDir()
	f := gitfake.New()
	rigRepo := filepath.Join(town, promoteTestRig, ".repo.git")
	forgejo := filepath.Join(town, "forgejo.git")
	github := filepath.Join(town, "github.git")
	f.InitBare(t, rigRepo)
	f.InitBare(t, forgejo)
	f.InitBare(t, github)
	base := f.Commit(t, forgejo, "main", "main: seed", map[string]string{"a.txt": "one\n"})
	sha := f.Commit(t, forgejo, "main", "green", map[string]string{"b.txt": "ok\n"})
	// GitHub main starts where the last promotion left it: behind Forgejo, so
	// promoting the green commit is a fast-forward.
	f.SetRef(t, github, "refs/heads/main", base)

	fx := &promoteFixture{
		town:    town,
		f:       f,
		repo:    f.Open(rigRepo),
		forgejo: forgejo,
		github:  github,
		key:     filepath.Join(town, "promote-"+promoteTestRig+".key"),
		base:    base,
		sha:     sha,
	}
	if err := os.WriteFile(fx.key, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-key\n"), 0o600); err != nil {
		t.Fatalf("writing the deploy key: %v", err)
	}
	fx.writeForgejo(t, fmt.Sprintf(`{"remote_url":%q,"promote_target":%q,"promote_key_file":%q}`, forgejo, github, fx.key))
	return fx
}

// writeForgejo writes the rig's merge_queue.forgejo block, in the shape
// rig.ResolveForgejoConfig reads.
func (fx *promoteFixture) writeForgejo(t *testing.T, block string) {
	t.Helper()
	body := fmt.Sprintf(`{"type":"rig","version":1,"name":%q,"merge_queue":{"forgejo":%s}}`, promoteTestRig, block)
	dir := filepath.Join(fx.town, promoteTestRig)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the rig directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing the rig config: %v", err)
	}
}

func (fx *promoteFixture) deps() promoteDeps {
	return promoteDeps{
		TownRoot: fx.town,
		Repo:     func(string) promoteGit { return fx.repo },
		State:    daemon.RedMainStateStore,
		Escalate: func(rigName, message string) { fx.escalated = append(fx.escalated, message) },
		Out:      &fx.out,
		Err:      &fx.errs,
	}
}

func (fx *promoteFixture) record(t *testing.T) landworker.MainState {
	t.Helper()
	st, err := daemon.RedMainStateStore(fx.town, promoteTestRig).Load()
	if err != nil {
		t.Fatalf("reading the red-main state: %v", err)
	}
	return st
}

func TestPromotePushesTheSHAAndRecordsIt(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)

	if err := promoteRigSHA(fx.deps(), promoteTestRig, fx.sha); err != nil {
		t.Fatalf("promoteRigSHA: %v", err)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.sha {
		t.Errorf("GitHub main = %q, want the promoted commit %s", got, fx.sha)
	}
	st := fx.record(t)
	if st.LastPromoted != fx.sha {
		t.Errorf("last_promoted = %q, want the full commit %s", st.LastPromoted, fx.sha)
	}
	if st.LastError != "" || st.GitHubDiverged != nil {
		t.Errorf("record = %+v, want no error and no divergence", st)
	}
	if !strings.Contains(fx.out.String(), shortSHA(fx.sha)) {
		t.Errorf("out = %q, want the commit named", fx.out.String())
	}
	if len(fx.escalated) != 0 {
		t.Errorf("escalations = %v, want none", fx.escalated)
	}
}

// A commit Forgejo holds on another branch is not on main, so it is not
// promotable: nothing reaches GitHub and nothing is recorded.
func TestPromoteRefusesACommitNotOnTheRigMain(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	offMain := fx.f.Commit(t, fx.forgejo, "side", "side: not on main", map[string]string{"c.txt": "x\n"})

	err := promoteRigSHA(fx.deps(), promoteTestRig, offMain)
	if code := exitCodeForError(err); code != promoteExitNotPromotable {
		t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitNotPromotable)
	}
	if !strings.Contains(err.Error(), "Forgejo main") {
		t.Errorf("err = %v, want it to name the rig's Forgejo main", err)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.base {
		t.Errorf("GitHub main = %q, want %q untouched", got, fx.base)
	}
	if st := fx.record(t); st.LastPromoted != "" {
		t.Errorf("record = %+v, want nothing recorded for a refused commit", st)
	}
}

// A rig the tier sweep covers promotes only the sweep's last green commit:
// publishing anything else would put a commit on GitHub that the town's own
// tier checks have not covered (gt-qk0pi).
func TestPromoteRefusesACommitPastTheSweepsLastGreen(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	green := fx.f.Commit(t, fx.forgejo, "main", "newer green", map[string]string{"d.txt": "green\n"})
	deps := fx.deps()
	deps.Sweep = func(string, string) (daemon.TierSweepCoverage, error) {
		return daemon.TierSweepCoverage{Covered: true, LastGreenSHA: green}, nil
	}

	err := promoteRigSHA(deps, promoteTestRig, fx.sha)
	if code := exitCodeForError(err); code != promoteExitNotPromotable {
		t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitNotPromotable)
	}
	if !strings.Contains(err.Error(), shortSHA(green)) {
		t.Errorf("err = %v, want it to name the sweep's last green commit %s", err, shortSHA(green))
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.base {
		t.Errorf("GitHub main = %q, want %q untouched", got, fx.base)
	}
	if st := fx.record(t); st.LastPromoted != "" {
		t.Errorf("record = %+v, want nothing recorded for a refused commit", st)
	}
}

// A sweep-covered rig with no fully green cycle has nothing promotable: the
// sweep owns the promotion from the moment it covers the rig (gt-qk0pi).
func TestPromoteRefusesASweepCoveredRigWithNoGreenCycle(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	deps := fx.deps()
	deps.Sweep = func(string, string) (daemon.TierSweepCoverage, error) {
		return daemon.TierSweepCoverage{Covered: true}, nil
	}

	err := promoteRigSHA(deps, promoteTestRig, fx.sha)
	if code := exitCodeForError(err); code != promoteExitNotPromotable {
		t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitNotPromotable)
	}
	if !strings.Contains(err.Error(), "tier sweep") {
		t.Errorf("err = %v, want it to name the tier sweep", err)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.base {
		t.Errorf("GitHub main = %q, want %q untouched", got, fx.base)
	}
}

// The sweep's own last green commit is the one a covered rig promotes.
func TestPromoteAllowsTheSweepsLastGreen(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	deps := fx.deps()
	deps.Sweep = func(string, string) (daemon.TierSweepCoverage, error) {
		return daemon.TierSweepCoverage{Covered: true, LastGreenSHA: fx.sha}, nil
	}

	if err := promoteRigSHA(deps, promoteTestRig, fx.sha); err != nil {
		t.Fatalf("promoteRigSHA: %v", err)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.sha {
		t.Errorf("GitHub main = %q, want the promoted commit %s", got, fx.sha)
	}
}

// A rig outside the sweep is the caller's to promote; the guard is the
// sweep's, not a general one.
func TestPromoteLeavesARigOutsideTheSweepToTheCaller(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	deps := fx.deps()
	deps.Sweep = func(string, string) (daemon.TierSweepCoverage, error) {
		return daemon.TierSweepCoverage{}, nil
	}

	if err := promoteRigSHA(deps, promoteTestRig, fx.sha); err != nil {
		t.Fatalf("promoteRigSHA: %v", err)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.sha {
		t.Errorf("GitHub main = %q, want the promoted commit %s", got, fx.sha)
	}
}

// A sweep that cannot be read is a failure, not a promotion: publishing past
// an unread guard is the condition the guard exists to prevent (gt-qk0pi).
func TestPromoteFailsWhenTheSweepCannotBeRead(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	deps := fx.deps()
	deps.Sweep = func(string, string) (daemon.TierSweepCoverage, error) {
		return daemon.TierSweepCoverage{}, errors.New("corrupt record")
	}

	err := promoteRigSHA(deps, promoteTestRig, fx.sha)
	if code := exitCodeForError(err); code != promoteExitFailed {
		t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitFailed)
	}
	if !strings.Contains(err.Error(), "corrupt record") {
		t.Errorf("err = %v, want the read's own failure", err)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.base {
		t.Errorf("GitHub main = %q, want %q untouched", got, fx.base)
	}
}

func TestPromoteRefusesARigThatDoesNotPromote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, block, want string
	}{
		{"no promote_target", fmt.Sprintf(`{"remote_url":%q}`, "FORGEJO"), "promote_target"},
		{"no promote_key_file", fmt.Sprintf(`{"remote_url":%q,"promote_target":%q}`, "FORGEJO", "GITHUB"), "promote_key_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPromoteFixture(t)
			block := strings.NewReplacer("FORGEJO", fx.forgejo, "GITHUB", fx.github).Replace(tc.block)
			fx.writeForgejo(t, block)

			err := promoteRigSHA(fx.deps(), promoteTestRig, fx.sha)
			if code := exitCodeForError(err); code != promoteExitNotPromotable {
				t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitNotPromotable)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %s", err, tc.want)
			}
			if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.base {
				t.Errorf("GitHub main = %q, want %q untouched", got, fx.base)
			}
		})
	}
}

// A rig that lands elsewhere has no Forgejo main to promote onto GitHub, so
// the command refuses rather than promoting a commit nothing validated.
func TestPromoteRefusesARigNotCutOverToForgejo(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	body := fmt.Sprintf(`{"type":"rig","version":1,"name":%q}`, promoteTestRig)
	if err := os.WriteFile(filepath.Join(fx.town, promoteTestRig, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing the rig config: %v", err)
	}

	err := promoteRigSHA(fx.deps(), promoteTestRig, fx.sha)
	if code := exitCodeForError(err); code != promoteExitNotPromotable {
		t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitNotPromotable)
	}
	if !strings.Contains(err.Error(), "Forgejo-primary landing") {
		t.Errorf("err = %v, want the Forgejo-primary refusal", err)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.base {
		t.Errorf("GitHub main = %q, want %q untouched", got, fx.base)
	}
}

// A target someone pushed to directly is not a fast-forward: the command
// leaves it alone, records the divergence and raises it once.
func TestPromoteReportsADivergedTarget(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	ghOnly := fx.f.Commit(t, fx.github, "main", "github: pushed directly", map[string]string{"github.txt": "direct\n"})

	err := promoteRigSHA(fx.deps(), promoteTestRig, fx.sha)
	if code := exitCodeForError(err); code != promoteExitFailed {
		t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitFailed)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != ghOnly {
		t.Errorf("GitHub main = %q, want %q untouched: a divergence is never pushed", got, ghOnly)
	}
	if st := fx.record(t); st.GitHubDiverged == nil || st.GitHubDiverged.Commit != fx.sha {
		t.Errorf("record = %+v, want the divergence at %s", st, fx.sha)
	}
	if len(fx.escalated) != 1 {
		t.Errorf("escalations = %v, want one for the divergence", fx.escalated)
	}
}

// A push the target refuses — the rig reads it and cannot write to it — is a
// promotion failure, told apart from a divergence by the failure it records.
func TestPromoteReportsARejectedPush(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	// "upstream" reads the GitHub target and pushes to a repository that is not
	// there, which is what git's push URL override models: the fetch that
	// judges ancestry succeeds and the push is refused.
	if err := fx.repo.AddUpstreamRemote(fx.github); err != nil {
		t.Fatalf("adding the upstream remote: %v", err)
	}
	if err := fx.repo.ConfigurePushURL("upstream", filepath.Join(fx.town, "missing.git")); err != nil {
		t.Fatalf("setting the push URL: %v", err)
	}
	fx.writeForgejo(t, fmt.Sprintf(`{"remote_url":%q,"promote_target":"upstream","promote_key_file":%q}`, fx.forgejo, fx.key))

	err := promoteRigSHA(fx.deps(), promoteTestRig, fx.sha)
	if code := exitCodeForError(err); code != promoteExitFailed {
		t.Fatalf("exit code = %d (%v), want %d", code, err, promoteExitFailed)
	}
	st := fx.record(t)
	if !strings.Contains(st.LastError, "pushing") {
		t.Errorf("last_promote_error = %q, want the push's own failure", st.LastError)
	}
	if st.LastPromoted != "" {
		t.Errorf("last_promoted = %q, want it unchanged after a failed push", st.LastPromoted)
	}
	if got := fx.f.Ref(fx.github, promote.MainRef); got != fx.base {
		t.Errorf("GitHub main = %q, want %q untouched", got, fx.base)
	}
	if len(fx.escalated) != 0 {
		t.Errorf("escalations = %v, want none: a refused push is not a divergence", fx.escalated)
	}
}

// The deploy key's path never reaches the command's output, which a caller may
// log: only ssh ever names the key.
func TestPromoteKeepsTheKeyPathOutOfItsOutput(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	if err := promoteRigSHA(fx.deps(), promoteTestRig, fx.sha); err != nil {
		t.Fatalf("promoteRigSHA: %v", err)
	}
	for _, line := range []string{fx.out.String(), fx.errs.String()} {
		if strings.Contains(line, fx.key) {
			t.Errorf("output %q names the deploy key path", line)
		}
	}
}

// abbreviationGit is a repository that resolves an abbreviated id to the
// commit it names, which gitfake does not model: it answers the ancestry
// check's four calls and nothing else, so a promote.Repo call panics.
type abbreviationGit struct {
	promote.Repo
	full string
}

func (g abbreviationGit) FetchRefspecWithTimeout(string, string, time.Duration) error { return nil }

func (g abbreviationGit) RefExists(ref string) (bool, error) {
	return strings.HasPrefix(g.full, strings.TrimSuffix(ref, "^{commit}")), nil
}

func (g abbreviationGit) Rev(string) (string, error) { return g.full, nil }

func (g abbreviationGit) IsAncestor(ancestor, descendant string) (bool, error) {
	return ancestor == g.full && descendant == rigMainRef, nil
}

// A caller passing the abbreviated id a commit line shows names the same
// commit, and the full id is what the promotion records: the sweep's own retry
// test compares last_promoted against a full sha, so an abbreviated record
// would read as a promotion still owed.
func TestCommitOnRigMainResolvesAnAbbreviatedSHA(t *testing.T) {
	t.Parallel()
	full := strings.Repeat("a", 40)
	got, err := commitOnRigMain(abbreviationGit{full: full}, "forgejo", "aaaaaaa")
	if err != nil {
		t.Fatalf("commitOnRigMain: %v", err)
	}
	if got != full {
		t.Errorf("commit = %q, want the full id %s", got, full)
	}
}

// midPushStore is a red-main store whose second Load returns what the daemon
// wrote while the push ran: a verdict landing in the record after the command
// read it and before it saves (gt-1ohu4).
type midPushStore struct {
	loads int
	saved landworker.MainState
}

func (s *midPushStore) Load() (landworker.MainState, error) {
	s.loads++
	if s.loads == 1 {
		return landworker.MainState{}, nil
	}
	return landworker.MainState{LastGreen: "green-mid-push", LastRun: "green-mid-push"}, nil
}

func (s *midPushStore) Save(st landworker.MainState) error {
	s.saved = st
	return nil
}

// A verdict the daemon writes while the push runs survives the promotion: the
// command re-reads the record after the push and replaces only the promotion
// State, so last_green written mid-push is kept rather than overwritten with
// the copy loaded before it (gt-1ohu4).
func TestPromoteKeepsAVerdictWrittenDuringThePush(t *testing.T) {
	t.Parallel()
	fx := newPromoteFixture(t)
	store := &midPushStore{}
	deps := fx.deps()
	deps.State = func(string, string) landworker.MainStateStore { return store }

	if err := promoteRigSHA(deps, promoteTestRig, fx.sha); err != nil {
		t.Fatalf("promoteRigSHA: %v", err)
	}
	if store.loads != 2 {
		t.Fatalf("Load calls = %d, want the record re-read after the push", store.loads)
	}
	if got := store.saved.LastGreen; got != "green-mid-push" {
		t.Errorf("saved last_green = %q, want the verdict written during the push", got)
	}
	if got := store.saved.LastPromoted; got != fx.sha {
		t.Errorf("saved last_promoted = %q, want the promoted commit %s", got, fx.sha)
	}
}
