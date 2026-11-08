package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// greenRefFixture is a town whose rig publishes green refs to a Forgejo
// remote the fake git world models: the rig's bare repository has a forgejo
// remote that points at a second fake repository, and the test can read the
// refs the cycle left there.
type greenRefFixture struct {
	d      *Daemon
	rec    *tierSweepRunRecorder
	logs   *bytes.Buffer
	clk    *clockwork.FakeClock
	f      *gitfake.Fake
	repo   string
	remote string
	git    *greenRefRecordingRepo
}

// newGreenRefFixture builds the town with image_rigs naming its one rig, so a
// green cycle publishes there.
func newGreenRefFixture(t *testing.T, now time.Time) *greenRefFixture {
	t.Helper()
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.ImageRigs = []string{"gastown"}
	f := useGitfake(t, d)
	town := d.config.TownRoot

	repo := filepath.Join(town, "gastown", ".repo.git")
	f.InitBare(t, repo)
	remote := filepath.Join(t.TempDir(), "forgejo.git")
	f.InitBare(t, remote)
	f.AddRemote(t, repo, "forgejo", remote)
	// The rig names the remote its landing worker pushes through, which is
	// the one the green ref must use.
	writeDaemonRigConfigFile(t, filepath.Join(town, "gastown"),
		`{"type":"rig","version":1,"name":"gastown","default_branch":"main","merge_queue":{"forgejo":{"remote_url":"`+remote+`","gate_workflow":"gate","bots":{"landing":"gt-landing"}}}}`)

	recorder := &greenRefRecordingRepo{}
	open := d.openGitFn
	d.openGitFn = func(dir string) daemonGit {
		g := open(dir)
		if filepath.Clean(dir) == filepath.Clean(repo) {
			recorder.daemonGit = g
			return recorder
		}
		return g
	}
	clk, ok := d.clk().(*clockwork.FakeClock)
	if !ok {
		t.Fatalf("the fixture's clock = %T, want the fake clock the sweep tests advance", d.clk())
	}
	return &greenRefFixture{d: d, rec: rec, logs: logs, clk: clk, f: f, repo: repo, remote: remote, git: recorder}
}

// commit makes another commit in the rig's repository.
func (fx *greenRefFixture) commit(t *testing.T, branch string) string {
	t.Helper()
	return fx.f.Commit(t, fx.repo, branch, "commit on "+branch, map[string]string{"f.txt": branch + "\n"})
}

// runCycle drives one town-wide cycle for the rig at sha, with every stage
// answering what rec was set to.
func (fx *greenRefFixture) runCycle(t *testing.T, sha string) bool {
	t.Helper()
	fx.d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
	fx.d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	cycle := fx.d.startDogCycle("tier_sweep")
	defer cycle.close()
	return fx.d.runTierSweepRig(context.Background(), cycle, "gastown", fx.d.clk().Now())
}

// greenRefRecordingRepo records what the green ref path writes to the remote,
// and fails the reads and writes a test tells it to.
type greenRefRecordingRepo struct {
	daemonGit
	pushes    []greenRefPush
	deletes   []string
	lists     []string
	dated     []string
	pushErr   error
	deleteErr func(branch string) error
	listErr   func(prefix string) error
	commitErr func(hash string) error
	injectRef func(prefix string) []git.RemoteRef
}

func (g *greenRefRecordingRepo) ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	g.lists = append(g.lists, prefix)
	if g.listErr != nil {
		if err := g.listErr(prefix); err != nil {
			return nil, err
		}
	}
	refs, err := g.daemonGit.ListRemoteRefsWithHashes(remote, prefix)
	if err != nil {
		return nil, err
	}
	if g.injectRef != nil {
		refs = append(refs, g.injectRef(prefix)...)
	}
	return refs, nil
}

func (g *greenRefRecordingRepo) CommitTime(rev string) (time.Time, error) {
	g.dated = append(g.dated, rev)
	if g.commitErr != nil {
		if err := g.commitErr(rev); err != nil {
			return time.Time{}, err
		}
	}
	return g.daemonGit.CommitTime(rev)
}

type greenRefPush struct {
	remote  string
	refspec string
	force   bool
}

func (g *greenRefRecordingRepo) Push(remote, refspec string, force bool) error {
	g.pushes = append(g.pushes, greenRefPush{remote: remote, refspec: refspec, force: force})
	if g.pushErr != nil && strings.Contains(refspec, greenRefPrefix) {
		return g.pushErr
	}
	return g.daemonGit.Push(remote, refspec, force)
}

func (g *greenRefRecordingRepo) DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error {
	g.deletes = append(g.deletes, branch)
	if g.deleteErr != nil {
		if err := g.deleteErr(branch); err != nil {
			return err
		}
	}
	return g.daemonGit.DeleteRemoteBranchIfAt(remote, branch, expectedHash)
}

// greenRefItems is the daemon's green ref attention items.
func greenRefItems(t *testing.T, d *Daemon) []attention.Item {
	t.Helper()
	items, err := (&attentionSources{d: d}).collectGreenRef(context.Background())
	if err != nil {
		t.Fatalf("collectGreenRef: %v", err)
	}
	return items
}

// greenRefLogLines is the green ref lines the sweep logged.
func greenRefLogLines(logs *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "green ref") {
			out = append(out, line)
		}
	}
	return out
}

// Only a cycle that covered every tier green marks its commit for the image
// build. A red tier, a stage that deferred and a tier the cycle never reached
// each leave the ref alone (gt-gg9hv.61).
func TestGreenRefPushedOnlyForAFullyGreenCycle(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)

	t.Run("a fully green cycle pushes once", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		fx.rec.green()
		if !fx.runCycle(t, sha) {
			t.Fatal("the cycle deferred; every tier ran green")
		}
		want := sha + ":refs/heads/" + greenRefName(sha)
		if len(fx.git.pushes) != 1 {
			t.Fatalf("pushes = %+v, want exactly one", fx.git.pushes)
		}
		if got := fx.git.pushes[0]; got.refspec != want || got.force {
			t.Errorf("push = %+v, want a plain push of %q", got, want)
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(sha)); got != sha {
			t.Errorf("remote %s = %q, want %q", greenRefName(sha), got, sha)
		}
	})

	t.Run("a red tier pushes nothing", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		fx.rec.result = func(tierSweepStage) tierSweepStageResult {
			return tierSweepStageResult{
				output:   "tier-sweep: shell RED passed=1 failed=1 skipped=0 failed: TestRed (logs /tmp/tier-sweep.x)\n",
				exitCode: 1, ran: true,
			}
		}
		fx.runCycle(t, sha)
		if len(fx.git.pushes) != 0 {
			t.Errorf("pushes = %+v, want none for a red cycle", fx.git.pushes)
		}
	})

	t.Run("a deferred stage pushes nothing", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		fx.rec.result = func(st tierSweepStage) tierSweepStageResult {
			if st.slot {
				return tierSweepStageResult{err: errors.New("slot busy")}
			}
			return tierSweepStageResult{
				output:   "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n",
				exitCode: 0, ran: true,
			}
		}
		fx.runCycle(t, sha)
		if len(fx.git.pushes) != 0 {
			t.Errorf("pushes = %+v, want none for a deferred cycle", fx.git.pushes)
		}
	})

	t.Run("an uncovered tier pushes nothing", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		// The shell tier is green, and the integration stage is cut off after
		// it began but before race printed: race is starved, so the cycle is
		// partial and covers no green at this tip.
		fx.rec.result = func(st tierSweepStage) tierSweepStageResult {
			if st.slot {
				return tierSweepStageResult{
					output:   "tier-sweep: integration ./internal/cmd ...\n",
					exitCode: -1, timedOut: true, ran: true,
				}
			}
			return tierSweepStageResult{
				output:   "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n",
				exitCode: 0, ran: true,
			}
		}
		fx.runCycle(t, sha)
		if len(fx.git.pushes) != 0 {
			t.Errorf("pushes = %+v, want none while a scheduled tier was not covered", fx.git.pushes)
		}
		if st := tierSweepTestState(t, fx.d, "gastown"); !st.Partial || st.LastGreenSHA == sha {
			t.Errorf("record = %+v, want a partial cycle with no green tip at %s", st, shortSHA(sha))
		}
	})

	t.Run("an empty tip pushes nothing", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		fx.rec.green()
		fx.runCycle(t, "")
		if len(fx.git.pushes) != 0 {
			t.Errorf("pushes = %+v, want none without a commit to name", fx.git.pushes)
		}
		if lines := greenRefLogLines(fx.logs); len(lines) != 0 {
			t.Errorf("green ref log = %q, want none: there is no commit to publish", lines)
		}
	})

	t.Run("a rig outside image_rigs pushes nothing", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		fx.d.patrolConfig.Patrols.TierSweep.ImageRigs = nil
		sha := fx.commit(t, "main")
		fx.rec.green()
		if !fx.runCycle(t, sha) {
			t.Fatal("the cycle deferred")
		}
		if len(fx.git.pushes) != 0 {
			t.Errorf("pushes = %+v, want none: the default publishes nothing", fx.git.pushes)
		}
	})

	t.Run("an unknown rig in image_rigs is refused by name", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		fx.d.patrolConfig.Patrols.TierSweep.ImageRigs = []string{"gastwon"}
		sha := fx.commit(t, "main")
		fx.rec.green()
		if !fx.runCycle(t, sha) {
			t.Fatal("the cycle deferred")
		}
		if len(fx.git.pushes) != 0 {
			t.Errorf("pushes = %+v, want none for a list naming no known rig", fx.git.pushes)
		}
		if logs := fx.logs.String(); !strings.Contains(logs, `"gastwon" is not a known rig`) {
			t.Errorf("logs = %q, want the refused rig named", logs)
		}
	})
}

// A rig named in image_rigs whose settings name no Forgejo remote has nothing
// to publish to: the push is refused rather than sent to origin, and the
// cycle's verdict is untouched (gt-gg9hv.61).
func TestGreenRefPublishRefusesARigWithNoForgejoRemote(t *testing.T) {
	t.Parallel()
	fx := newGreenRefFixture(t, atHour(time.Now(), 15))
	writeDaemonRigConfigFile(t, filepath.Join(fx.d.config.TownRoot, "gastown"),
		`{"type":"rig","version":1,"name":"gastown","default_branch":"main"}`)
	sha := fx.commit(t, "main")
	fx.rec.green()
	if !fx.runCycle(t, sha) {
		t.Fatal("the refused publish changed the cycle's verdict")
	}
	if len(fx.git.pushes) != 0 {
		t.Errorf("pushes = %+v, want none without a Forgejo remote", fx.git.pushes)
	}
	if logs := fx.logs.String(); !strings.Contains(logs, "no merge_queue.forgejo remote_url") {
		t.Errorf("logs = %q, want the refusal naming the missing block", logs)
	}
}

// A remote the daemon cannot resolve is a refusal too: an unreadable rig
// config and a remote_url that names none of the repository's remotes each
// publish nothing, and the cycle keeps its verdict. The log and the item name
// the failure's class, never the resolution error's text, which names the
// configured URL and can carry its credential (gt-gg9hv.61, gt-atcddq).
func TestGreenRefPublishRefusesAnUnresolvableRemote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		prepare   func(t *testing.T, fx *greenRefFixture)
		want      string
		forbidden string
	}{
		{
			name: "a settings tier that does not decode",
			prepare: func(t *testing.T, fx *greenRefFixture) {
				dir := filepath.Join(fx.d.config.TownRoot, "gastown", "settings")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"merge_queue":`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want:      "the rig's Forgejo settings could not be read",
			forbidden: "settings/config.json",
		},
		{
			name: "a remote_url matching no remote",
			prepare: func(t *testing.T, fx *greenRefFixture) {
				writeDaemonRigConfigFile(t, filepath.Join(fx.d.config.TownRoot, "gastown"),
					`{"type":"rig","version":1,"name":"gastown","default_branch":"main","merge_queue":{"forgejo":{"remote_url":"https://bot:secret-token@forgejo.example/other/repo.git","gate_workflow":"gate","bots":{"landing":"gt-landing"}}}}`)
			},
			want:      "no git remote matches merge_queue.forgejo.remote_url",
			forbidden: "secret-token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newGreenRefFixture(t, atHour(time.Now(), 15))
			tc.prepare(t, fx)
			sha := fx.commit(t, "main")
			fx.rec.green()
			// The third refused cycle raises the item whose summary carries
			// the detail, so the check covers both the line and the item.
			for i := 0; i < 3; i++ {
				if !fx.runCycle(t, sha) {
					t.Fatal("the refused publish changed the cycle's verdict")
				}
			}
			if len(fx.git.pushes) != 0 {
				t.Errorf("pushes = %+v, want none when the remote cannot be resolved", fx.git.pushes)
			}
			if logs := fx.logs.String(); !strings.Contains(logs, tc.want) {
				t.Errorf("logs = %q, want the refusal naming %q", logs, tc.want)
			}
			if logs := fx.logs.String(); strings.Contains(logs, tc.forbidden) {
				t.Errorf("logs = %q, want the resolution error's text kept out", logs)
			}
			items := greenRefItems(t, fx.d)
			if len(items) != 1 || items[0].Key != "green-ref-push:gastown" {
				t.Fatalf("items = %+v, want one push item after three refused cycles", items)
			}
			if !strings.Contains(items[0].Summary, tc.want) {
				t.Errorf("summary = %q, want it naming %q", items[0].Summary, tc.want)
			}
			if strings.Contains(items[0].Summary, tc.forbidden) {
				t.Errorf("summary = %q, want the resolution error's text kept out", items[0].Summary)
			}
		})
	}
}

// A ref already at the green commit is success and no second push; a ref the
// remote holds at another commit is refused, raises one conflict item and is
// never force-updated (gt-gg9hv.61).
func TestGreenRefIsIdempotentAndNeverForced(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)

	t.Run("a ref already at the commit is not pushed again", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		fx.f.SetRef(t, fx.remote, "refs/heads/"+greenRefName(sha), sha)
		fx.rec.green()
		if !fx.runCycle(t, sha) {
			t.Fatal("the cycle deferred")
		}
		if len(fx.git.pushes) != 0 {
			t.Errorf("pushes = %+v, want none: the remote already holds the ref", fx.git.pushes)
		}
		if items := greenRefItems(t, fx.d); len(items) != 0 {
			t.Errorf("items = %+v, want none: the ref is at the green commit", items)
		}
	})

	t.Run("a ref at another commit is refused, one item, never forced", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		other := fx.f.Commit(t, fx.remote, "someone-else", "another commit", map[string]string{"o.txt": "other\n"})
		fx.f.SetRef(t, fx.remote, "refs/heads/"+greenRefName(sha), other)
		fx.rec.green()
		if !fx.runCycle(t, sha) {
			t.Fatal("the cycle deferred")
		}
		if len(fx.git.pushes) != 0 {
			t.Fatalf("pushes = %+v, want none: the ref is held at another commit", fx.git.pushes)
		}
		for _, p := range fx.git.pushes {
			if p.force {
				t.Errorf("a forced push was issued: %+v", p)
			}
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(sha)); got != other {
			t.Errorf("remote %s = %q, want the other writer's %q left alone", greenRefName(sha), got, other)
		}
		items := greenRefItems(t, fx.d)
		wantKey := "green-ref-conflict:gastown:" + greenRefShortSHA(sha)
		if len(items) != 1 || items[0].Key != wantKey {
			t.Fatalf("items = %+v, want one keyed %q", items, wantKey)
		}
		if items[0].Kind != attention.KindGreenRef || items[0].Severity != attention.SeverityLow {
			t.Errorf("item = %+v, want a low green-ref item", items[0])
		}
	})

	// The conflict is about the tip the sweep could not mark. Once the rig
	// publishes a different commit the image build is marked, so the old
	// tip's item must not stand: nothing else would ever clear it, because
	// the ref is never force-updated (gt-atcddq).
	t.Run("a conflict clears when the rig publishes a different commit", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		first := fx.commit(t, "main")
		other := fx.f.Commit(t, fx.remote, "someone-else", "another commit", map[string]string{"o.txt": "other\n"})
		fx.f.SetRef(t, fx.remote, "refs/heads/"+greenRefName(first), other)
		fx.rec.green()
		if !fx.runCycle(t, first) {
			t.Fatal("the cycle deferred")
		}
		conflictKey := "green-ref-conflict:gastown:" + greenRefShortSHA(first)
		if items := greenRefItems(t, fx.d); len(items) != 1 || items[0].Key != conflictKey {
			t.Fatalf("items = %+v, want the conflict item keyed %q", items, conflictKey)
		}

		next := fx.commit(t, "main")
		if !fx.runCycle(t, next) {
			t.Fatal("the cycle deferred")
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(next)); got != next {
			t.Errorf("remote %s = %q, want the newly green tip %q", greenRefName(next), got, next)
		}
		if items := greenRefItems(t, fx.d); len(items) != 0 {
			t.Errorf("items = %+v, want none after the rig published %s", items, shortSHA(next))
		}
	})
}

// The ref is named by the first twelve characters of its commit, and the
// remote is asked about that exact name. ls-remote matches its pattern as a
// path suffix, so a remote that holds refs/heads/x/green/<sha12> at another
// commit must not read as the green ref held: the green ref is pushed, not
// refused (gt-xmomer).
func TestGreenRefNamesTheTwelveCharacterPrefixAndMatchesTheExactRef(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)

	t.Run("the ref is named by the first twelve characters of the commit", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		if len(sha) != 40 {
			t.Fatalf("commit %q is %d characters, want a full 40-character sha", sha, len(sha))
		}
		fx.rec.green()
		if !fx.runCycle(t, sha) {
			t.Fatal("the cycle deferred")
		}
		want := sha + ":refs/heads/green/" + sha[:12]
		if len(fx.git.pushes) != 1 || fx.git.pushes[0].refspec != want {
			t.Fatalf("pushes = %+v, want the one push %q", fx.git.pushes, want)
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/green/"+sha[:12]); got != sha {
			t.Errorf("remote green/%s = %q, want %q", sha[:12], got, sha)
		}
	})

	t.Run("a listing that holds the name as a path suffix is not the ref held", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		sha := fx.commit(t, "main")
		if len(sha) != 40 {
			t.Fatalf("commit %q is %d characters, want a full 40-character sha", sha, len(sha))
		}
		name := "green/" + sha[:12]
		other := fx.f.Commit(t, fx.remote, "someone-else", "another commit", map[string]string{"o.txt": "other\n"})
		// The pattern refs/heads/green/<sha12> comes back with this ref under
		// its suffix match; it is not the exact ref the publish names.
		fx.git.injectRef = func(prefix string) []git.RemoteRef {
			if prefix != "refs/heads/"+name {
				return nil
			}
			return []git.RemoteRef{{Hash: other, Name: "refs/heads/x/" + name}}
		}
		fx.rec.green()
		if !fx.runCycle(t, sha) {
			t.Fatal("the cycle deferred")
		}
		want := sha + ":refs/heads/" + name
		if len(fx.git.pushes) != 1 || fx.git.pushes[0].refspec != want {
			t.Fatalf("pushes = %+v, want the publish %q: a suffix match is not the ref held", fx.git.pushes, want)
		}
		if items := greenRefItems(t, fx.d); len(items) != 0 {
			t.Errorf("items = %+v, want none: the exact ref was never held at another commit", items)
		}
	})
}

// A push that fails leaves the cycle's verdict alone: the sweep is green, the
// failure is logged once an hour, retried on the next cycle for the same
// commit, and the third consecutive failure raises one low item that the next
// success clears (gt-gg9hv.61).
func TestGreenRefPushFailureIsNotRed(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	fx := newGreenRefFixture(t, now)
	sha := fx.commit(t, "main")
	fx.rec.green()
	fx.git.pushErr = errors.New("git push: exit status 1\nremote: pre-receive hook declined")

	if !fx.runCycle(t, sha) {
		t.Fatal("a failed green ref push changed the cycle's verdict")
	}
	if st := tierSweepTestState(t, fx.d, "gastown"); st.LastGreenSHA != sha {
		t.Fatalf("LastGreenSHA = %q, want the green tip %q", st.LastGreenSHA, sha)
	}
	if len(fx.git.pushes) != 1 {
		t.Fatalf("pushes = %d, want the first attempt", len(fx.git.pushes))
	}
	if lines := greenRefLogLines(fx.logs); len(lines) != 1 || !strings.Contains(lines[0], "pre-receive hook declined") {
		t.Fatalf("green ref log = %q, want one line naming the remote's message", lines)
	}

	// The next cycle runs at the same unchanged main: the failure is retried,
	// and the hour has not passed, so the line is not printed again.
	if !fx.runCycle(t, sha) {
		t.Fatal("the retry changed the cycle's verdict")
	}
	if len(fx.git.pushes) != 2 {
		t.Fatalf("pushes = %d, want the failure retried on the next cycle", len(fx.git.pushes))
	}
	if lines := greenRefLogLines(fx.logs); len(lines) != 1 {
		t.Errorf("green ref log = %q, want the second failure held to the hour", lines)
	}

	// An hour on, the third failure prints its line and raises the item.
	fx.clk.Advance(time.Hour)
	if !fx.runCycle(t, sha) {
		t.Fatal("the third attempt changed the cycle's verdict")
	}
	if len(fx.git.pushes) != 3 {
		t.Fatalf("pushes = %d, want three attempts", len(fx.git.pushes))
	}
	if lines := greenRefLogLines(fx.logs); len(lines) != 2 {
		t.Errorf("green ref log = %q, want the hourly line for the third failure", lines)
	}
	items := greenRefItems(t, fx.d)
	wantKey := "green-ref-push:gastown"
	if len(items) != 1 || items[0].Key != wantKey || items[0].Severity != attention.SeverityLow {
		t.Fatalf("items = %+v, want one low item keyed %q", items, wantKey)
	}

	// A push that succeeds clears the item.
	fx.git.pushErr = nil
	if !fx.runCycle(t, sha) {
		t.Fatal("the successful cycle changed the verdict")
	}
	if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(sha)); got != sha {
		t.Errorf("remote %s = %q, want the pushed %q", greenRefName(sha), got, sha)
	}
	if items := greenRefItems(t, fx.d); len(items) != 0 {
		t.Errorf("items = %+v, want none after a successful push", items)
	}
}

// A ref read that fails is a failed attempt too: nothing is pushed, the
// reason is logged, and the cycle keeps its green verdict (gt-gg9hv.61).
func TestGreenRefReadFailureIsNotARedSweep(t *testing.T) {
	t.Parallel()
	fx := newGreenRefFixture(t, atHour(time.Now(), 15))
	sha := fx.commit(t, "main")
	fx.git.listErr = func(string) error { return errors.New("ls-remote: remote hung up") }
	fx.rec.green()
	if !fx.runCycle(t, sha) {
		t.Fatal("a failed ref read changed the cycle's verdict")
	}
	if len(fx.git.pushes) != 0 {
		t.Errorf("pushes = %+v, want none when the ref cannot be read", fx.git.pushes)
	}
	if st := tierSweepTestState(t, fx.d, "gastown"); st.LastGreenSHA != sha {
		t.Errorf("LastGreenSHA = %q, want the green tip %q", st.LastGreenSHA, sha)
	}
	if lines := greenRefLogLines(fx.logs); len(lines) != 1 {
		t.Errorf("green ref log = %q, want one line for the failed attempt", lines)
	}
}

// greenRefExitError is a git exit status the reason classifier matches by
// ExitCode, as *exec.ExitError answers.
type greenRefExitError int

func (e greenRefExitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e greenRefExitError) ExitCode() int { return int(e) }

// When the remote says nothing, the logged reason names the failure's class
// rather than echoing git's error text, which can carry the remote URL and its
// credential (gt-gg9hv.61).
func TestGreenRefPushFailureReasonIsAClassTheRemoteSpokeNot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an exit status", fmt.Errorf("git push: %w", greenRefExitError(128)), "exit status 128"},
		{"a timeout", fmt.Errorf("pushing: %w", context.DeadlineExceeded), "timed out"},
		{"no detail", errors.New("something went wrong"), "the git command failed with no remote message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newGreenRefFixture(t, atHour(time.Now(), 15))
			sha := fx.commit(t, "main")
			fx.git.pushErr = tc.err
			fx.rec.green()
			fx.runCycle(t, sha)
			lines := greenRefLogLines(fx.logs)
			if len(lines) != 1 || !strings.Contains(lines[0], tc.want) {
				t.Fatalf("green ref log = %q, want one line naming %q", lines, tc.want)
			}
			if strings.Contains(fx.logs.String(), "something went wrong") {
				t.Errorf("logs = %q, want the error's text kept out", fx.logs.String())
			}
		})
	}
}

// A remote's message is cut to 200 runes in the log line, so one long message
// cannot make an hourly line unbounded. The cut counts runes: the message is
// multi-byte, so a byte cut would keep only 100 of them (gt-xmomer).
func TestGreenRefPushFailureReasonIsCutToTwoHundredRunes(t *testing.T) {
	t.Parallel()
	fx := newGreenRefFixture(t, atHour(time.Now(), 15))
	sha := fx.commit(t, "main")
	fx.git.pushErr = errors.New("rejected\nremote: " + strings.Repeat("é", 201) + "TAIL")
	fx.rec.green()
	fx.runCycle(t, sha)

	lines := greenRefLogLines(fx.logs)
	if len(lines) != 1 || !strings.Contains(lines[0], strings.Repeat("é", 200)) {
		t.Fatalf("green ref log = %q, want one line holding the first 200 runes of the remote's message", lines)
	}
	if strings.Contains(lines[0], strings.Repeat("é", 201)) || strings.Contains(lines[0], "TAIL") {
		t.Errorf("green ref log = %q, want the remote's message cut at 200 runes", lines)
	}
}

// A publish retains the newest greenRefKeep refs, oldest first, deletes one
// ref at a time, counts a failed delete without stopping the rest, and never
// touches a ref outside refs/heads/green/ (gt-gg9hv.61).
func TestGreenRefRetentionKeepsTheNewest20(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)

	t.Run("the oldest refs are deleted one by one", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		old := fx.seedGreenRefs(t, 24)
		sha := fx.commit(t, "main")
		fx.f.SetRef(t, fx.remote, "refs/heads/main", old[0])
		fx.f.SetRef(t, fx.remote, "refs/heads/land/gt-x", old[0])
		fx.rec.green()
		fx.runCycle(t, sha)

		if len(fx.git.pushes) != 1 {
			t.Fatalf("pushes = %+v, want the green tip published", fx.git.pushes)
		}
		if len(fx.git.deletes) != 5 {
			t.Fatalf("deletes = %v, want the 5 oldest of the 25 refs", fx.git.deletes)
		}
		for i, tip := range old {
			got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(tip))
			if i < 5 && got != "" {
				t.Errorf("old ref %d = %q, want it deleted", i, got)
			}
			if i >= 5 && got != tip {
				t.Errorf("old ref %d = %q, want it kept at %q", i, got, tip)
			}
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/main"); got != old[0] {
			t.Errorf("refs/heads/main = %q, want it untouched", got)
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/land/gt-x"); got != old[0] {
			t.Errorf("refs/heads/land/gt-x = %q, want it untouched", got)
		}
	})

	t.Run("a failed delete is counted and the rest still go", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		old := fx.seedGreenRefs(t, 24)
		sha := fx.commit(t, "main")
		stuck := old[1]
		fx.git.deleteErr = func(branch string) error {
			if branch == greenRefName(stuck) {
				return errors.New("push: remote rejected the delete")
			}
			return nil
		}
		fx.rec.green()
		fx.runCycle(t, sha)

		if len(fx.git.deletes) != 5 {
			t.Fatalf("deletes = %v, want one attempt per ref past the newest 20", fx.git.deletes)
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(stuck)); got != stuck {
			t.Errorf("the ref whose delete failed = %q, want it left at %q", got, stuck)
		}
		for i, tip := range old {
			if i >= 5 || i == 1 {
				continue
			}
			if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(tip)); got != "" {
				t.Errorf("old ref %d = %q, want it deleted", i, got)
			}
		}
		if !strings.Contains(fx.logs.String(), "green ref retention: deleting") {
			t.Errorf("logs = %q, want the failed delete logged", fx.logs.String())
		}
	})

	t.Run("an unreadable ref listing keeps every ref and is logged", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		old := fx.seedGreenRefs(t, 24)
		sha := fx.commit(t, "main")
		fx.git.listErr = func(prefix string) error {
			if prefix == greenRefPrefix {
				return errors.New("ls-remote: remote hung up")
			}
			return nil
		}
		fx.rec.green()
		fx.runCycle(t, sha)

		if len(fx.git.deletes) != 0 {
			t.Errorf("deletes = %v, want none: the refs could not be listed", fx.git.deletes)
		}
		for _, tip := range old {
			if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(tip)); got != tip {
				t.Errorf("old ref = %q, want it kept at %q", got, tip)
			}
		}
		if !strings.Contains(fx.logs.String(), "green ref retention: reading the green refs") {
			t.Errorf("logs = %q, want the unreadable listing logged", fx.logs.String())
		}
	})

	t.Run("a ref whose commit cannot be dated is kept", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		old := fx.seedGreenRefs(t, 20)
		undated := old[0]
		sha := fx.commit(t, "main")
		fx.git.commitErr = func(hash string) error {
			if hash == undated {
				return errors.New("fatal: bad object")
			}
			return nil
		}
		fx.rec.green()
		fx.runCycle(t, sha)

		if len(fx.git.deletes) != 0 {
			t.Errorf("deletes = %v, want none: the datable refs are within the newest 20", fx.git.deletes)
		}
		if got := fx.f.Ref(fx.remote, "refs/heads/"+greenRefName(undated)); got != undated {
			t.Errorf("the undated ref = %q, want it kept at %q", got, undated)
		}
		if !strings.Contains(fx.logs.String(), "its commit could not be dated") {
			t.Errorf("logs = %q, want the undated ref reported", fx.logs.String())
		}
	})

	t.Run("a ref the listing carries outside green/ is never deleted", func(t *testing.T) {
		t.Parallel()
		fx := newGreenRefFixture(t, now)
		old := fx.seedGreenRefs(t, 20)
		sha := fx.commit(t, "main")
		// The injected ref is the oldest commit, so an unfiltered listing
		// would pick it for deletion ahead of the green refs.
		fx.git.injectRef = func(prefix string) []git.RemoteRef {
			if prefix != greenRefPrefix {
				return nil
			}
			return []git.RemoteRef{{Hash: old[0], Name: "refs/heads/not-green/x"}}
		}
		fx.rec.green()
		fx.runCycle(t, sha)

		for _, branch := range fx.git.deletes {
			if branch == "not-green/x" {
				t.Fatalf("deletes = %v, want another namespace's ref left alone", fx.git.deletes)
			}
		}
		if len(fx.git.deletes) != 1 {
			t.Errorf("deletes = %v, want the one green ref past the newest 20", fx.git.deletes)
		}
	})
}

// Retention runs only after a push. The unchanged-main heartbeat retries the
// publish, and with the ref already at the commit there is no write, so the
// listing that dates every green ref is not run (gt-atcddq).
func TestGreenRefUnchangedHeartbeatSkipsRetention(t *testing.T) {
	t.Parallel()
	fx := newGreenRefFixture(t, atHour(time.Now(), 15))
	sha := fx.commit(t, "main")
	fx.rec.green()
	if !fx.runCycle(t, sha) {
		t.Fatal("the cycle deferred")
	}
	if len(fx.git.pushes) != 1 {
		t.Fatalf("pushes = %+v, want the first cycle's push", fx.git.pushes)
	}

	// The next heartbeat sees the same origin/main: the publish is retried,
	// finds the ref at the commit, and writes nothing.
	fx.git.lists = nil
	fx.git.dated = nil
	if !fx.runCycle(t, sha) {
		t.Fatal("the unchanged heartbeat changed the cycle's verdict")
	}
	if len(fx.git.pushes) != 1 {
		t.Errorf("pushes = %d, want the idempotent retry to push nothing", len(fx.git.pushes))
	}
	for _, prefix := range fx.git.lists {
		if prefix == greenRefPrefix {
			t.Errorf("listed %q on an unchanged heartbeat: no push was made, so retention has nothing to prune", prefix)
		}
	}
	if len(fx.git.dated) != 0 {
		t.Errorf("dated %v on an unchanged heartbeat, want no retention work", fx.git.dated)
	}
}

// seedGreenRefs publishes n older green refs in the rig's repository and on
// the remote, oldest first, so commit age orders them the way a real town's
// sweeps would.
func (fx *greenRefFixture) seedGreenRefs(t *testing.T, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		sha := fx.f.Commit(t, fx.repo, fmt.Sprintf("green/seed-%02d", i),
			fmt.Sprintf("seed %d", i), map[string]string{"seed.txt": fmt.Sprintf("%d\n", i)})
		fx.f.SetRef(t, fx.remote, "refs/heads/"+greenRefName(sha), sha)
		out = append(out, sha)
	}
	return out
}
