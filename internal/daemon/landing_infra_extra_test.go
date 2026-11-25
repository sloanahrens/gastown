package daemon

import (
	"bytes"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// extraRepoDaemon is a daemon whose rigs land through Forgejo, with extra_repos given.
func extraRepoDaemon(t *testing.T, rigNames, extras []string) *Daemon {
	t.Helper()
	d, _ := daemonWithRecorder(t)
	f := useGitfake(t, d)
	d.knownRigsCache, d.knownRigsCacheValid = rigNames, true
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		LandingWorker: &LandingWorkerConfig{},
		LandingInfra:  &LandingInfraConfig{ExtraRepos: extras},
	}}
	for _, rigName := range rigNames {
		forgejoRigConfig(t, d.config.TownRoot, rigName)
		f.InitBare(t, filepath.Join(d.config.TownRoot, rigName, ".repo.git"))
	}
	return d
}

// TestLandingInfraExtraTargets: the rigs come first, then each extra repository
// in config order, named app:<owner>/<repo> and on the first rig's instance.
func TestLandingInfraExtraTargets(t *testing.T) {
	t.Parallel()
	d := extraRepoDaemon(t, []string{"A", "B"}, []string{"acme/app", "acme/tools"})

	got := d.landingInfraTargets()

	if want := []string{"A", "B", "app:acme/app", "app:acme/tools"}; !slices.Equal(targetRigs(got), want) {
		t.Fatalf("landingInfraTargets() rigs = %v, want %v", targetRigs(got), want)
	}
	rig := got[0]
	if rig.BaseErr != nil || rig.APIBase == "" {
		t.Fatalf("first rig target = %+v, want a resolved instance to copy", rig)
	}
	for i, tgt := range got {
		if tgt.Extra != (i >= 2) {
			t.Errorf("target %s Extra = %v, want %v", tgt.Rig, tgt.Extra, i >= 2)
		}
	}
	for _, ext := range got[2:] {
		if ext.Rig != "app:"+ext.Owner+"/"+ext.Repo || ext.BaseErr != nil || ext.APIBase != rig.APIBase || ext.TokenPath != rig.TokenPath || ext.ViewerTokenPath != rig.ViewerTokenPath {
			t.Errorf("target %+v, want app:owner/repo from its entry and the first rig's instance and tokens", ext)
		}
	}
}

// TestLandingInfraExtraTargetsRefuseWithoutRig: with no rig to take an instance
// from, the extra repository's target carries a BaseErr and no API base.
func TestLandingInfraExtraTargetsRefuseWithoutRig(t *testing.T) {
	t.Parallel()
	d := extraRepoDaemon(t, nil, []string{"acme/app"})

	got := d.landingInfraTargets()

	if len(got) != 1 || got[0].Rig != "app:acme/app" {
		t.Fatalf("landingInfraTargets() = %+v, want the one extra target", got)
	}
	if got[0].BaseErr == nil || got[0].APIBase != "" {
		t.Errorf("extra target = %+v, want a BaseErr and no API base", got[0])
	}
}

// TestLandingInfraExtraTargetsRefuseMalformedEntry: one malformed entry refuses
// the whole list, so only the rigs are watched, and the refusal is logged once
// across passes and names the entry.
func TestLandingInfraExtraTargetsRefuseMalformedEntry(t *testing.T) {
	t.Parallel()
	d := extraRepoDaemon(t, []string{"A"}, []string{"acme/app", "not-a-repo"})
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)

	first, second := d.landingInfraTargets(), d.landingInfraTargets()

	if want := []string{"A"}; !slices.Equal(targetRigs(first), want) || !slices.Equal(targetRigs(second), want) {
		t.Errorf("landingInfraTargets() rigs = %v then %v, want only %v", targetRigs(first), targetRigs(second), want)
	}
	if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "not-a-repo") {
		t.Errorf("malformed entry logged %q over two passes, want one line naming it", logs.String())
	}
}

// infraExtraTarget is an extra_repos app repository on rig's instance.
func infraExtraTarget(rig landingInfraTarget, owner, repo string) landingInfraTarget {
	t := rig
	t.Rig = "app:" + owner + "/" + repo
	t.Owner, t.Repo, t.Extra = owner, repo, true
	return t
}

// TestLandingInfraExtraRunnerRed: an app repository's run waiting 7 minutes is
// red and the rig's is not; 4 minutes is red for neither (gt-9mr11b).
func TestLandingInfraExtraRunnerRed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		rigWait, appWait time.Duration
		wantRed, notRed  string // the run text that must and must not appear; "" when nothing raises
	}{
		{"app 7 minutes, rig 7 minutes", 7 * time.Minute, 7 * time.Minute, "app:acme/app: run 92", "gastown: run 91"},
		{"app 4 minutes, rig 4 minutes", 4 * time.Minute, 4 * time.Minute, "", ""},
		{"app 4 minutes, rig 10 minutes", 10 * time.Minute, 4 * time.Minute, "gastown: run 91", "app:acme/app: run 92"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := infraTargets(t)[0]
			prober := &fakeProber{versions: map[string]error{}, pickups: map[string]fakeRunPickup{
				"gastown/gastown": {waiting: true, since: infraNow.Add(-tc.rigWait), runID: 91},
				"acme/app":        {waiting: true, since: infraNow.Add(-tc.appWait), runID: 92},
			}}
			d, _, rec := infraDaemon(t, []landingInfraTarget{rig, infraExtraTarget(rig, "acme", "app")}, prober)
			d.runLandingInfraProbe(infraNow)

			got := rec.Escalations()
			if tc.wantRed == "" {
				if len(got) != 0 {
					t.Fatalf("escalations = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Escalation.Fingerprint != alertKeyRunnerOffline {
				t.Fatalf("escalations = %+v, want one keyed %s", got, alertKeyRunnerOffline)
			}
			reason := got[0].Escalation.Reason
			if !strings.Contains(reason, tc.wantRed) || strings.Contains(reason, tc.notRed) {
				t.Errorf("escalation reason = %q, want %q and not %q", reason, tc.wantRed, tc.notRed)
			}
		})
	}
}

// TestLandingInfraExtraRunnerRedClears: a stalled app repository's escalation
// names it, and the escalation clears once it reads green (gt-9mr11b).
func TestLandingInfraExtraRunnerRedClears(t *testing.T) {
	t.Parallel()
	rig := infraTargets(t)[0]
	prober := &fakeProber{versions: map[string]error{}, pickups: map[string]fakeRunPickup{
		"acme/app": {waiting: true, since: infraNow.Add(-7 * time.Minute), runID: 92},
	}}
	d, _, rec := infraDaemon(t, []landingInfraTarget{rig, infraExtraTarget(rig, "acme", "app")}, prober)

	d.runLandingInfraProbe(infraNow)
	got := rec.Escalations()
	if len(got) != 1 || !strings.Contains(got[0].Escalation.Reason, "app:acme/app") {
		t.Fatalf("escalations = %+v, want one that names app:acme/app", got)
	}

	prober.pickups["acme/app"] = fakeRunPickup{}
	d.runLandingInfraProbe(infraNow.Add(time.Minute))
	if clears := rec.Clears(); len(clears) != 1 || clears[0].Fingerprints[0] != alertKeyRunnerOffline {
		t.Fatalf("clears = %+v, want one for %s", clears, alertKeyRunnerOffline)
	}
}
