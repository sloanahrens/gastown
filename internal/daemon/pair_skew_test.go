package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdskew"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// pairBdPin is the bd commit the fixture's image-pins file names.
	pairBdPin   = "dddddddddddddddddddddddddddddddddddddddd"
	pairBdPin12 = "dddddddddddd"
	// pairSchemaLevel is the schema level the fixture's pin and database agree
	// on unless a test says otherwise.
	pairSchemaLevel = 66
)

// pairImagePins is a valid image-pins file naming bd's commit as bd, the shape
// deploy/daemon/image-pins takes.
func pairImagePins(bd string) string {
	return fmt.Sprintf("bd https://forgejo.test/town/beads.git %s\nom https://forgejo.test/town/om.git %s\nhm https://forgejo.test/town/hm.git %s\ndevops https://forgejo.test/town/devops.git %s\n",
		bd, pairOther, pairOther, pairOther)
}

// pairAgreeingSkewReads is the skew guard's reads wired to agree at level, so a
// fixture that is not about the guard raises the pair-ready item as before.
func pairAgreeingSkewReads(level int) *pairReadySkewReads {
	return &pairReadySkewReads{
		LevelAt:  func(context.Context, string) (int, error) { return level, nil },
		Database: func(context.Context) (int, error) { return level, nil },
	}
}

// fakeSkewStore is the handshake store's schema level read.
type fakeSkewStore struct {
	level int
	err   error
}

func (f fakeSkewStore) SchemaLevel() (int, error) { return f.level, f.err }

// pairSkewItemKinds is the kinds the check holds.
func pairSkewItemKinds(items []attention.Item) []attention.Kind {
	var out []attention.Kind
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

// TestPreRollGuardASkewedPinRaisesTheSkewItem: a green commit whose pair is
// published but whose image pins name a bd commit at schema level 67, against a
// database at 66, raises one deploy-pair-skew item naming the pin and both
// levels, and no deploy-pair-ready item beside it.
func TestPreRollGuardASkewedPinRaisesTheSkewItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	var readPin string
	fx.check.skew = &pairReadySkewReads{
		LevelAt: func(_ context.Context, commit string) (int, error) {
			readPin = commit
			return 67, nil
		},
		Database: func(context.Context) (int, error) { return 66, nil },
	}

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1, "a skewed pair must raise its own item and no pair-ready item beside it: %v", pairSkewItemKinds(items))
	it := items[0]
	assert.Equal(t, attention.KindDeployPairSkew, it.Kind)
	assert.Equal(t, "deploy-pair-skew:gastown:"+pairGreen12, it.Key)
	assert.Equal(t, "gastown", it.Rig)
	assert.Equal(t, pairGreen, it.SHA)
	assert.Equal(t, attention.SeverityHigh, it.Severity)
	assert.Contains(t, it.Summary, "green/"+pairGreen12)
	assert.Contains(t, it.Summary, pairBdPin12, "the item does not name the bd pin the green commit carries")
	assert.Contains(t, it.Summary, "schema level 67")
	assert.Contains(t, it.Summary, "database is at 66")
	assert.Equal(t, pairBdPin, readPin, "the level was read for a commit other than the bd pin")
}

// TestPreRollGuardAgreeingLevelsLeaveThePairReadyItem: a pin and a database at
// the same level raise no skew item and leave the pair-ready item as it was.
func TestPreRollGuardAgreeingLevelsLeaveThePairReadyItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1, "agreeing levels raised an extra item: %v", pairSkewItemKinds(items))
	assert.Equal(t, attention.KindDeployPairReady, items[0].Kind)
}

// TestPreRollGuardTheSkewItemClearsWhenTheLevelsAgree: a skew raised on one pass
// is gone on the next, once the database has migrated to the pin's level.
func TestPreRollGuardTheSkewItemClearsWhenTheLevelsAgree(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	database := 66
	fx.check.skew = &pairReadySkewReads{
		LevelAt:  func(context.Context, string) (int, error) { return 67, nil },
		Database: func(context.Context) (int, error) { return database, nil },
	}

	fx.run(pairT0)
	items := fx.check.observed()
	require.Len(t, items, 1)
	require.Equal(t, attention.KindDeployPairSkew, items[0].Kind)

	database = 67
	fx.run(pairT0.Add(pairReadyInterval))

	items = fx.check.observed()
	require.Len(t, items, 1, "the skew item did not clear when the levels agreed: %v", pairSkewItemKinds(items))
	assert.Equal(t, attention.KindDeployPairReady, items[0].Kind)
}

// TestPreRollGuardTheSkewItemClearsWhenNoLongerNewer: a skew raised for a green
// commit is gone once the running commit reaches it, since the commit is no
// longer newer and no roll would start from it.
func TestPreRollGuardTheSkewItemClearsWhenNoLongerNewer(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.skew = &pairReadySkewReads{
		LevelAt:  func(context.Context, string) (int, error) { return 67, nil },
		Database: func(context.Context) (int, error) { return 66, nil },
	}

	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	fx.writeRunning(t, pairGreen)
	fx.run(pairT0.Add(pairReadyInterval))

	assert.Empty(t, fx.check.observed(), "a commit the running one reached kept its skew item")
}

// TestPreRollGuardAnUnreadableLevelRaisesTheUnreadableItemWithNoReady: a pin or
// level the guard cannot read raises the guard's own per-commit unreadable item
// and no pair-ready item and never a skew item, so an unread level is never taken
// for agreement or rolled. The failed read is logged.
func TestPreRollGuardAnUnreadableLevelRaisesTheUnreadableItemWithNoReady(t *testing.T) {
	t.Parallel()
	failRead := errors.New("the clone cannot read that commit")
	rows := []struct {
		name   string
		mutate func(fx *pairFixture)
	}{
		{
			name:   "the rig has no clone",
			mutate: func(fx *pairFixture) { fx.pins = nil },
		},
		{
			name: "the pins read fails",
			mutate: func(fx *pairFixture) {
				fx.pins = func(string) (string, error) { return "", failRead }
			},
		},
		{
			name: "the pins file does not parse",
			mutate: func(fx *pairFixture) {
				fx.pins = func(string) (string, error) { return "not a pins file", nil }
			},
		},
		{
			name: "the bd level read fails",
			mutate: func(fx *pairFixture) {
				fx.check.skew = &pairReadySkewReads{
					LevelAt:  func(context.Context, string) (int, error) { return 0, failRead },
					Database: func(context.Context) (int, error) { return 66, nil },
				}
			},
		},
		{
			name: "the database level read fails",
			mutate: func(fx *pairFixture) {
				fx.check.skew = &pairReadySkewReads{
					LevelAt:  func(context.Context, string) (int, error) { return 67, nil },
					Database: func(context.Context) (int, error) { return 0, failRead },
				}
			},
		},
		{
			name: "the bd level read is unwired",
			mutate: func(fx *pairFixture) {
				fx.check.skew = &pairReadySkewReads{
					Database: func(context.Context) (int, error) { return 66, nil },
				}
			},
		},
		{
			name: "the database level read is unwired",
			mutate: func(fx *pairFixture) {
				fx.check.skew = &pairReadySkewReads{
					LevelAt: func(context.Context, string) (int, error) { return 67, nil },
				}
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			row.mutate(fx)

			fx.run(pairT0)

			items := fx.check.observed()
			require.Len(t, items, 1, "an unreadable level raised %v; want the unreadable item only", pairSkewItemKinds(items))
			it := items[0]
			assert.NotEqual(t, attention.KindDeployPairSkew, it.Kind, "an unread level was raised as a skew")
			assert.NotEqual(t, attention.KindDeployPairReady, it.Kind, "an unread level was raised as a ready pair")
			assert.Equal(t, attention.KindDeployPairUnreadable, it.Kind, "an unread level was not raised as unread")
			assert.Equal(t, "deploy-pair-unreadable:skew:gastown:"+pairGreen12, it.Key)
			assert.Equal(t, attention.SeverityMedium, it.Severity)
			assert.Equal(t, pairGreen, it.SHA)
			assert.NotEmpty(t, fx.logs, "the unreadable read was not logged")
			logged := false
			for _, line := range fx.logs {
				if strings.Contains(line, "green/"+pairGreen12) {
					logged = true
					break
				}
			}
			assert.True(t, logged, "no log line names the green commit the guard could not read: %q", fx.logs)
		})
	}
}

// TestPreRollGuardTheUnreadableItemClearsWhenTheLevelReadsAgain: the unreadable
// item raised for an unread level is gone on the next pass once the level reads,
// and the pair-ready item the pass raises in its place is the only one.
func TestPreRollGuardTheUnreadableItemClearsWhenTheLevelReadsAgain(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	unreadable := true
	fx.check.skew = &pairReadySkewReads{
		LevelAt: func(context.Context, string) (int, error) {
			if unreadable {
				return 0, errors.New("the clone cannot read that commit")
			}
			return pairSchemaLevel, nil
		},
		Database: func(context.Context) (int, error) { return pairSchemaLevel, nil },
	}

	fx.run(pairT0)
	items := fx.check.observed()
	require.Len(t, items, 1, "an unread level did not raise the unreadable item alone: %v", pairSkewItemKinds(items))
	require.Equal(t, attention.KindDeployPairUnreadable, items[0].Kind)

	unreadable = false
	fx.run(pairT0.Add(pairReadyInterval))

	items = fx.check.observed()
	require.Len(t, items, 1, "the unreadable item did not clear when the level read: %v", pairSkewItemKinds(items))
	assert.Equal(t, attention.KindDeployPairReady, items[0].Kind)
}

// TestPreRollGuardASharedPinIsListedOnce: two green commits whose pins name the
// same bd commit list that commit's migrations once in a pass and not again on
// the next, since a commit's level cannot change, while the database is read
// once on each pass because a migration can move it.
func TestPreRollGuardASharedPinIsListedOnce(t *testing.T) {
	t.Parallel()
	clone, c := pairCloneHistory(t)
	fx := newPairCloneFixture(t, clone, c[0], c[1])
	fx.api.branches = []forgejo.Branch{pairBranch(c[1]), pairBranch(c[2])}
	fx.reg = &fakePairRegistry{digests: map[string]string{
		imagevalues.DaemonImage + "@" + c[1][:12]:  fx.daemon,
		imagevalues.PolecatImage + "@" + c[1][:12]: fx.polecat,
		imagevalues.DaemonImage + "@" + c[2][:12]:  fx.daemon,
		imagevalues.PolecatImage + "@" + c[2][:12]: fx.polecat,
	}}
	levels, databases := 0, 0
	fx.check.skew = &pairReadySkewReads{
		LevelAt: func(context.Context, string) (int, error) {
			levels++
			return 67, nil
		},
		Database: func(context.Context) (int, error) {
			databases++
			return 66, nil
		},
	}
	fx.pins = func(string) (string, error) { return pairImagePins(pairBdPin), nil }

	fx.run(pairT0)
	assert.Equal(t, 1, levels, "one bd commit was listed more than once in a pass")
	assert.Equal(t, 1, databases, "the database was read more than once in a pass")
	require.Len(t, fx.check.observed(), 2, "both green commits should raise their skew item")

	fx.run(pairT0.Add(pairReadyInterval))
	assert.Equal(t, 1, levels, "a cached bd commit level was listed again on a later pass")
	assert.Equal(t, 2, databases, "the database should be read once on each pass")
}

// TestPreRollGuardTheProductionSkewReads: the production reads ask the beads
// rig's bare clone at the pin commit through the lister, and take the database
// level from the handshake store.
func TestPreRollGuardTheProductionSkewReads(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var dirs []string
	var calls [][]string
	run := func(_ context.Context, dir string, args ...string) ([]byte, error) {
		dirs = append(dirs, dir)
		calls = append(calls, append([]string(nil), args...))
		return []byte("0066_a.up.sql\x000067_b.up.sql\x00"), nil
	}
	reads := pairReadySkewReadsAt(town, run, func(context.Context) (pairReadySchemaStore, error) {
		return fakeSkewStore{level: 67}, nil
	})
	require.NotNil(t, reads.LevelAt)
	require.NotNil(t, reads.Database)

	level, err := reads.LevelAt(context.Background(), pairBdPin)
	require.NoError(t, err)
	assert.Equal(t, 67, level)
	require.Len(t, calls, 1)
	assert.Equal(t, bdskew.BeadsRigRepo(town), dirs[0], "the lister did not read the beads rig's bare clone")
	assert.Contains(t, calls[0], "ls-tree")
	assert.Contains(t, calls[0], pairBdPin, "the lister did not ask git for the pin commit")
	assert.Contains(t, calls[0], bdskew.MigrationsDir+"/")

	database, err := reads.Database(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 67, database)
}

// TestPreRollGuardTheProductionReadsRefuseWhatTheyCannotRead: a nil run, a nil
// open, an opener that answers no store and a store whose read fails are all
// errors, never an agreeing level.
func TestPreRollGuardTheProductionReadsRefuseWhatTheyCannotRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	run := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("git failed") }
	rows := []struct {
		name  string
		reads *pairReadySkewReads
	}{
		{
			name:  "no git runner and no store opener",
			reads: pairReadySkewReadsAt(town, nil, nil),
		},
		{
			name: "the open answers no store",
			reads: pairReadySkewReadsAt(town, run, func(context.Context) (pairReadySchemaStore, error) {
				return nil, nil
			}),
		},
		{
			name: "the open fails",
			reads: pairReadySkewReadsAt(town, run, func(context.Context) (pairReadySchemaStore, error) {
				return nil, errors.New("the store cannot be opened")
			}),
		},
		{
			name: "the store read fails",
			reads: pairReadySkewReadsAt(town, run, func(context.Context) (pairReadySchemaStore, error) {
				return fakeSkewStore{err: errors.New("the database is unreachable")}, nil
			}),
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			pass := &pairReadySkewPass{reads: row.reads, cache: &pairReadySkewCache{}}
			_, err := pass.levelAt(context.Background(), pairBdPin)
			assert.Error(t, err, "a failed level read was returned as a level")
			_, err = pass.database(context.Background())
			assert.Error(t, err, "a failed database read was returned as a level")
		})
	}
}

// TestPreRollGuardTheCacheStartsOverAtItsBound: a cache at its bound drops every
// entry and starts again, so a town's history of greens and pins cannot grow it
// without end. The read that follows a reset asks again.
func TestPreRollGuardTheCacheStartsOverAtItsBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("levels", func(t *testing.T) {
		t.Parallel()
		reads, cache := 0, &pairReadySkewCache{}
		pass := &pairReadySkewPass{
			reads: &pairReadySkewReads{LevelAt: func(context.Context, string) (int, error) {
				reads++
				return 66, nil
			}},
			cache: cache,
		}
		for i := 0; i < pairReadySkewKeep; i++ {
			_, err := pass.levelAt(ctx, fmt.Sprintf("%040d", i))
			require.NoError(t, err)
		}
		require.Equal(t, pairReadySkewKeep, reads)
		// The entry past the bound starts the map over.
		_, err := pass.levelAt(ctx, fmt.Sprintf("%040d", pairReadySkewKeep))
		require.NoError(t, err)
		// The first commit was dropped with the map, so its next read asks again.
		_, err = pass.levelAt(ctx, fmt.Sprintf("%040d", 0))
		require.NoError(t, err)
		assert.Equal(t, pairReadySkewKeep+2, reads)
	})

	t.Run("pins", func(t *testing.T) {
		t.Parallel()
		pins, cache := 0, &pairReadySkewCache{}
		pass := &pairReadySkewPass{
			reads: &pairReadySkewReads{},
			cache: cache,
			owner: func() (string, error) { return "town", nil },
		}
		r := pairReadyRig{Rig: "gastown", Pins: func(string) (string, error) {
			pins++
			return pairImagePins(pairBdPin), nil
		}}
		for i := 0; i < pairReadySkewKeep; i++ {
			_, err := pass.bdPin(r, pairReadyGreen{sha: fmt.Sprintf("%040d", i)})
			require.NoError(t, err)
		}
		require.Equal(t, pairReadySkewKeep, pins)
		_, err := pass.bdPin(r, pairReadyGreen{sha: fmt.Sprintf("%040d", pairReadySkewKeep)})
		require.NoError(t, err)
		_, err = pass.bdPin(r, pairReadyGreen{sha: fmt.Sprintf("%040d", 0)})
		require.NoError(t, err)
		assert.Equal(t, pairReadySkewKeep+2, pins)
	})
}

// TestPreRollGuardASkewedPairAnswersPublished: the tier sweep asks the pair
// check whether a commit's images are published before it re-runs the build. A
// skewed pair's images are published, so the answer is published and the sweep
// does not treat the commit as one the pair check never answered.
func TestPreRollGuardASkewedPairAnswersPublished(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.skew = &pairReadySkewReads{
		LevelAt:  func(context.Context, string) (int, error) { return 67, nil },
		Database: func(context.Context) (int, error) { return 66, nil },
	}
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)
	require.Equal(t, attention.KindDeployPairSkew, fx.check.observed()[0].Kind)

	failed, published := fx.check.imagesAnswerFor("gastown", pairGreen)
	assert.False(t, failed)
	assert.True(t, published, "a skewed pair's images are published, so the sweep must not read the commit as unanswered")
}

// TestPreRollGuardTheSkewItemHasItsOwnCollector: the skew item the check holds
// reaches the queue through a collector registered for its kind, and that
// collector returns only its own kind.
func TestPreRollGuardTheSkewItemHasItsOwnCollector(t *testing.T) {
	t.Parallel()
	skew := attention.Item{Key: "deploy-pair-skew:gastown:" + pairGreen12, Kind: attention.KindDeployPairSkew}
	ready := attention.Item{Key: "deploy-pair-ready:gastown:" + pairGreen12, Kind: attention.KindDeployPairReady}
	s := &attentionSources{pairReady: func() []attention.Item { return []attention.Item{ready, skew} }}

	var collected []attention.Item
	registered := false
	for _, c := range s.collectors() {
		if c.kind != attention.KindDeployPairSkew {
			continue
		}
		registered = true
		items, err := c.collect(context.Background())
		require.NoError(t, err)
		collected = items
	}
	require.True(t, registered, "no collector is registered for deploy-pair-skew, so the item never reaches the queue")
	require.Len(t, collected, 1)
	assert.Equal(t, skew, collected[0])
}

// TestPreRollGuardTheGitRunnerRefusesAFailedGit: a git command that cannot run
// is an error naming the call, never an empty listing the level reader would
// take for level zero. The command has no program to run, so the failure is
// driven in-process.
func TestPreRollGuardTheGitRunnerRefusesAFailedGit(t *testing.T) {
	t.Parallel()
	out, err := pairReadyGitOutput(&exec.Cmd{Args: []string{"git", "ls-tree", pairBdPin}})
	assert.Nil(t, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git ls-tree "+pairBdPin, "the error does not name the git call")
}

// pairPinsClone is a bare clone that answers ShowFileAtRev from a map and
// records the path it was asked for.
type pairPinsClone struct {
	pairReadyGit
	files map[string]string
	paths []string
}

func (c *pairPinsClone) ShowFileAtRev(ref, path string) (string, error) {
	c.paths = append(c.paths, path)
	text, ok := c.files[ref]
	if !ok {
		return "", errors.New("not at ref")
	}
	return text, nil
}

// TestPreRollGuardThePinsReaderRefusesWhatItCannotRead: the production pin
// reader asks the clone for the image-pins file at the green commit, and a nil
// clone or one that cannot read a file at a commit is no reader at all.
func TestPreRollGuardThePinsReaderRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	clone := &pairPinsClone{files: map[string]string{pairGreen: pairImagePins(pairBdPin)}}
	read := pairReadyPinsAt(clone)
	require.NotNil(t, read, "a clone that reads files at a commit was refused")
	text, err := read(pairGreen)
	require.NoError(t, err)
	assert.Equal(t, pairImagePins(pairBdPin), text)
	assert.Equal(t, []string{daemonimage.PinsPath}, clone.paths)

	assert.Nil(t, pairReadyPinsAt(nil), "a nil clone was read as a reader")
	assert.Nil(t, pairReadyPinsAt(pairUndatedClone{}), "a clone without ShowFileAtRev was read as a reader")

	_, err = read(pairOther)
	assert.Error(t, err, "a pins read off the clone's history was answered")
}

// TestPreRollGuardAJudgedCommitClearsItsSkewItem: a commit the pass judged
// drops the skew item it held, even when the same pass broke before reaching
// another commit, so a skew that has cleared does not linger on a branch the
// scan still cannot read.
func TestPreRollGuardAJudgedCommitClearsItsSkewItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.api.branches = append(fx.api.branches, pairBranch(pairOther))
	fx.api.compares[pairOther+"..."+pairRunning] = 0
	fx.api.compares[pairRunning+"..."+pairOther] = 7
	fx.reg.(*fakePairRegistry).digests[imagevalues.DaemonImage+"@"+pairOther12] = digestOf([]byte("other daemon"))
	fx.reg.(*fakePairRegistry).digests[imagevalues.PolecatImage+"@"+pairOther12] = digestOf([]byte("other polecat"))
	database := 66
	fx.check.skew = &pairReadySkewReads{
		LevelAt:  func(context.Context, string) (int, error) { return 67, nil },
		Database: func(context.Context) (int, error) { return database, nil },
	}

	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 2, "both green commits should raise their skew item")

	// The levels agree now, and the pass breaks before judging the second
	// branch: the first commit's skew item must be gone, not kept as unread.
	database = 67
	delete(fx.api.compares, pairRunning+"..."+pairOther)
	fx.run(pairT0.Add(pairReadyInterval))

	var skewKeys []string
	for _, it := range fx.check.observed() {
		if it.Kind == attention.KindDeployPairSkew {
			skewKeys = append(skewKeys, it.Key)
		}
	}
	assert.Equal(t, []string{"deploy-pair-skew:gastown:" + pairOther12}, skewKeys,
		"the judged commit's cleared skew item was kept, or the unjudged branch's item was dropped")
}

// TestPreRollGuardTheUnreadableItemSurvivesAnIncompleteScan: a scan that broke
// before reaching a commit keeps the guard's unreadable item for it, so a
// transiently unread level is not silently dropped from the queue.
func TestPreRollGuardTheUnreadableItemSurvivesAnIncompleteScan(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.skew = &pairReadySkewReads{
		LevelAt:  func(context.Context, string) (int, error) { return 0, errors.New("the level is unreadable") },
		Database: func(context.Context) (int, error) { return 66, nil },
	}
	fx.run(pairT0)
	keys := make(map[string]bool)
	for _, it := range fx.check.observed() {
		keys[it.Key] = true
	}
	require.True(t, keys["deploy-pair-unreadable:skew:gastown:"+pairGreen12], "the guard's unreadable item was not raised: %v", fx.check.observed())

	// The next pass fails before judging the branch: the items it could not
	// re-confirm are kept beside the rig's unreadable item.
	fx.api.branchErr = errors.New("forgejo is down")
	fx.run(pairT0.Add(pairReadyInterval))

	keys = make(map[string]bool)
	for _, it := range fx.check.observed() {
		keys[it.Key] = true
	}
	assert.True(t, keys["deploy-pair-unreadable:skew:gastown:"+pairGreen12],
		"the guard's unreadable item was dropped by an incomplete scan: %v", fx.check.observed())
	assert.False(t, keys["deploy-pair-ready:gastown:"+pairGreen12],
		"an incomplete scan raised a ready item beside the unread level: %v", fx.check.observed())
	assert.True(t, keys["deploy-pair-unreadable:rig:gastown"],
		"the rig's unreadable item was not raised: %v", fx.check.observed())
}
