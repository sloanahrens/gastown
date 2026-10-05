package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// The landing_infra patrol is the landing worker's companion (gt-fn9e6.11).
// A Forgejo instance that is down, or a CI runner that is offline, does not
// change what landing does: the candidate gate waits out CI silence and the
// worker retries it on the infrastructure path. What it does change is that
// nothing is visibly wrong, so the town looks idle while every landing sits
// in a wait. This patrol asks the two questions that name that state — does
// the instance answer, does the repository have a runner online — records
// them for the town health line, and raises one escalation per condition once
// the failure has lasted long enough to be an outage rather than a blip.

// defaultLandingInfraInterval is how often the probe asks. A landing gate
// waits minutes, so a minute between observations sees an outage well before
// it has stranded much work, and two cheap requests a minute is nothing.
const defaultLandingInfraInterval = 60 * time.Second

// landingInfraProbeTimeout bounds one probe's single HTTP request. A probe
// makes one request and never retries: a retry inside a probe would make one
// observation take longer than the interval that schedules the next.
const landingInfraProbeTimeout = 5 * time.Second

// The escalation fingerprints, one per condition: a condition that persists
// across passes keeps one open escalation (gt-vwry), and a recovery can close
// exactly the condition it names (gt-fn9e6.11).
const (
	alertKeyForgejoDown   = "landing-forgejo-down"
	alertKeyRunnerOffline = "landing-runner-offline"
)

// landingInfraSource names this patrol in its escalations and log lines.
const landingInfraSource = "landing_infra"

// landingInfraInterval is the wait between probe passes. Default 60s.
func landingInfraInterval(config *DaemonPatrolConfig) time.Duration {
	if c := landingInfraConfig(config); c != nil && c.IntervalStr != "" {
		if d, err := time.ParseDuration(c.IntervalStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultLandingInfraInterval
}

// landingInfraConfig is daemon.json's patrols.landing_infra block, or nil.
func landingInfraConfig(config *DaemonPatrolConfig) *LandingInfraConfig {
	if config == nil || config.Patrols == nil {
		return nil
	}
	return config.Patrols.LandingInfra
}

// landingInfraTarget is one rig the probe covers: the Forgejo instance its
// landing path uses and the repository its CI runners serve.
//
// It carries paths, never secrets: the token file's name, not its contents.
// The token is read inside a pass and lives only in that pass's locals, so no
// token can reach the probe's recorded state (gt-fn9e6.11).
type landingInfraTarget struct {
	Rig string
	// Owner and Repo are the rig's Forgejo repository, read from the same
	// remote URL the landing path pushes to.
	Owner, Repo string
	// APIBase is the instance's API root; empty when the rig's block names no
	// usable remote URL, and then BaseErr says why.
	APIBase string
	// BaseErr is why the remote URL could not be resolved to an instance.
	BaseErr error
	// TokenPath is the landing bot's token file, the way the Forgejo client
	// reads it. Empty with TokenErr set when the directory cannot be found.
	TokenPath string
	// TokenErr is why the token path could not be resolved.
	TokenErr error
}

// landingInfraTargets lists the rigs the probe covers: every known rig whose
// merge_queue.forgejo block resolves (rig.ResolveForgejoConfig). That is the
// same block the landing worker lands through, so the probe watches the
// instance the rig's CI actually gates; a rig without it lands through
// GitHub and has no runner to be offline (gt-fn9e6.11).
func (d *Daemon) landingInfraTargets() []landingInfraTarget {
	if d.landingInfraRigs != nil {
		return d.landingInfraRigs()
	}
	tokenDir, dirErr := landingWorkerConfig(d.patrolConfig).ForgejoTokenDir()
	var out []landingInfraTarget
	for _, name := range d.getKnownRigs() {
		fj := rig.ResolveForgejoConfig(d.config.TownRoot, name)
		if fj == nil {
			continue
		}
		t := landingInfraTarget{Rig: name, TokenErr: dirErr}
		if dirErr == nil {
			t.TokenPath = filepath.Join(tokenDir, "forgejo-"+config.ForgejoRoleLanding+".env")
		}
		out = append(out, t.withRemote(fj.RemoteURL))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rig < out[j].Rig })
	return out
}

// withRemote fills in the repository and API root the rig's remote URL names,
// or BaseErr when it names neither.
func (t landingInfraTarget) withRemote(remoteURL string) landingInfraTarget {
	owner, repo, err := land.RepoFromRemoteURL(remoteURL)
	if err != nil {
		t.BaseErr = err
		return t
	}
	base, err := land.APIBaseFromRemoteURL(remoteURL)
	if err != nil {
		t.BaseErr = err
		return t
	}
	t.Owner, t.Repo, t.APIBase = owner, repo, base
	return t
}

// infraProber makes the probe's two HTTP calls. The real one goes through the
// Forgejo API client; tests inject a fake, so no test reaches the network
// (gt-fn9e6.11).
type infraProber interface {
	// Version asks the instance its version, the unauthenticated call.
	Version(ctx context.Context, apiBase string) error
	// Runners counts the runners registered to owner/repo and how many of
	// them are online.
	Runners(ctx context.Context, apiBase, token, owner, repo string) (online, total int, err error)
}

// forgejoProber is infraProber over the Forgejo client. Every probe call gets
// the one HTTP client whose timeout is the whole budget of a request.
type forgejoProber struct {
	http *http.Client
}

// newForgejoProber builds the real prober.
func newForgejoProber() infraProber {
	return forgejoProber{http: &http.Client{Timeout: landingInfraProbeTimeout}}
}

func (p forgejoProber) Version(ctx context.Context, apiBase string) error {
	c, err := forgejo.NewClient("",
		forgejo.WithBaseURL(apiBase),
		forgejo.WithoutToken(),
		forgejo.WithHTTPClient(p.http))
	if err != nil {
		return err
	}
	_, err = c.Version(ctx)
	return err
}

func (p forgejoProber) Runners(ctx context.Context, apiBase, token, owner, repo string) (int, int, error) {
	c, err := forgejo.NewClient("",
		forgejo.WithBaseURL(apiBase),
		forgejo.WithToken(token),
		forgejo.WithHTTPClient(p.http))
	if err != nil {
		return 0, 0, err
	}
	runners, err := c.ListRepoRunners(ctx, owner, repo)
	if err != nil {
		return 0, 0, err
	}
	online := 0
	for _, r := range runners {
		if r.Online() {
			online++
		}
	}
	return online, len(runners), nil
}

// infraResult is one probe's raw outcome for one rig, before the recorder
// turns it into the timed observation the fields judge.
type infraResult struct {
	// available is false when the probe could not be made at all; reason then
	// says why and the field reads UNKNOWN.
	available bool
	reason    string
	ok        bool
	detail    string
	online    int
}

// unavailable is a probe that could not be made.
func unavailable(reason string) infraResult { return infraResult{reason: reason} }

// infraObservation is one probe's outcome for one rig as the state holds it,
// with the clock the field's verdict is judged by.
type infraObservation struct {
	infraResult
	// since is when the current failing run began; zero when OK or when the
	// probe could not be made.
	since time.Time
}

// probe renders the observation as the townhealth field input.
func (o infraObservation) probe() townhealth.InfraProbe {
	if !o.available {
		reason := o.reason
		if reason == "" {
			reason = "the probe has not run yet"
		}
		return townhealth.InfraProbe{Unavailable: reason}
	}
	return townhealth.InfraProbe{OK: o.ok, Since: o.since, Detail: o.detail, Online: o.online}
}

// landingInfraState is the probe's last observation, guarded by its own
// mutex: the patrol writes it on its goroutine and the town health
// computation reads it on the heartbeat's.
type landingInfraState struct {
	mu sync.Mutex
	// rigs is every covered rig, sorted; empty until the first pass, and
	// empty again when no rig lands through Forgejo — which is what makes
	// the forgejo and runner fields absent rather than unknown.
	rigs    []string
	forgejo map[string]infraObservation
	runner  map[string]infraObservation
	// alertedForgejo and alertedRunner record that an escalation is open for
	// that condition, so a pass while it holds raises nothing further.
	alertedForgejo bool
	alertedRunner  bool
}

// report is the probe's last observation as townhealth reads it.
func (st *landingInfraState) report() townhealth.LandingInfraReport {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.rigs) == 0 {
		return townhealth.LandingInfraReport{}
	}
	rep := townhealth.LandingInfraReport{Configured: true, Rigs: make([]townhealth.RigInfra, 0, len(st.rigs))}
	for _, name := range st.rigs {
		rep.Rigs = append(rep.Rigs, townhealth.RigInfra{
			Rig:     name,
			Forgejo: st.forgejo[name].probe(),
			Runner:  st.runner[name].probe(),
		})
	}
	return rep
}

// infraAlertAction is one escalation change a pass decided. Actions are
// applied by the caller after the state lock is released: gt escalate and
// gt escalate clear are subprocesses that can take seconds, and the health
// computation reads this state (gt-fn9e6.11).
type infraAlertAction struct {
	key    string
	raise  bool
	detail string
}

// triggerLandingInfraProbe runs one pass on its own goroutine when the
// patrol is on. A pass makes at most one request per instance plus one per
// rig, each bounded by landingInfraProbeTimeout, so inline it could stall the
// heartbeat by seconds; the single-flight guard drops a tick that arrives
// while a pass is still running rather than queueing it (gt-fn9e6.11).
func (d *Daemon) triggerLandingInfraProbe() {
	if !d.isPatrolActive("landing_infra") {
		return
	}
	if !d.landingInfraRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.landingInfraRunning.Store(false)
		d.runLandingInfraProbe(d.clk().Now())
	}()
}

// runLandingInfraProbe makes one pass: probe every covered rig, record what
// it saw, and raise or clear the escalations the pass decided.
func (d *Daemon) runLandingInfraProbe(now time.Time) {
	d.applyInfraAlerts(d.probeLandingInfra(now, d.landingInfraTargets()))
}

// applyInfraAlerts raises and clears the escalations a pass decided.
func (d *Daemon) applyInfraAlerts(actions []infraAlertAction) {
	for _, a := range actions {
		if a.raise {
			d.escalateAlert(a.key, landingInfraSource, a.detail)
			continue
		}
		d.clearAlerts(a.detail, a.key)
	}
}

// probeLandingInfra runs one pass over tgts and folds the outcome into the
// daemon's state, returning the escalation changes it decided. It holds the
// state lock only around the fold: the probes run before it is taken, and the
// escalations are left to the caller, so neither a slow request nor a slow
// gt escalate can stall the heartbeat that reads this state.
func (d *Daemon) probeLandingInfra(now time.Time, tgts []landingInfraTarget) []infraAlertAction {
	// A town where no rig lands through Forgejo has nothing to probe: the
	// state empties, the fields go absent, and an escalation left open for a
	// condition nothing observes any more is cleared.
	if len(tgts) == 0 {
		return d.landingInfra.observeNothing()
	}
	prober := d.landingInfraProber
	if prober == nil {
		prober = newForgejoProber()
	}
	// The role's token file is one path for the town; every target carries
	// the same one (landingInfraTargets). It is read here, per pass, into a
	// local: nothing that outlives the pass holds it.
	token, tokenErr := readLandingInfraToken(tgts[0])
	// One instance is asked once per pass: the version probe answers a
	// question about the instance, not about the rig that named it.
	versions := make(map[string]infraResult)
	forgejoObs := make(map[string]infraResult, len(tgts))
	runnerObs := make(map[string]infraResult, len(tgts))
	for _, t := range tgts {
		forgejoObs[t.Rig] = probeForgejo(prober, versions, t)
		runnerObs[t.Rig] = probeRunners(prober, t, token, tokenErr)
	}
	return d.landingInfra.record(now, tgts, forgejoObs, runnerObs)
}

// readLandingInfraToken reads the landing bot's token, the one secret the
// probe needs, the way the landing worker reads it. A missing or unreadable
// file is not an error the probe invents a token for: it is the reason the
// runner probe cannot be made, and the field then reads unknown.
func readLandingInfraToken(t landingInfraTarget) (string, error) {
	if t.TokenErr != nil {
		return "", t.TokenErr
	}
	if t.TokenPath == "" {
		return "", errors.New("no landing bot token file is configured")
	}
	return forgejo.ReadTokenFile(t.TokenPath)
}

// probeForgejo asks the rig's instance its version, reusing the answer a
// previous target already got for the same instance.
func probeForgejo(prober infraProber, versions map[string]infraResult, t landingInfraTarget) infraResult {
	if t.BaseErr != nil {
		return unavailable(fmt.Sprintf("the rig's Forgejo settings name no usable remote URL: %v", t.BaseErr))
	}
	if seen, ok := versions[t.APIBase]; ok {
		return seen
	}
	res := infraResult{available: true}
	if err := prober.Version(context.Background(), t.APIBase); err != nil {
		res.detail = err.Error()
	} else {
		res.ok = true
	}
	versions[t.APIBase] = res
	return res
}

// probeRunners asks the rig's repository for its runners and counts the
// online ones. A rig with no registered runner is a failure, not an unknown:
// the repository is readable and its answer is that nothing can pick a run up.
func probeRunners(prober infraProber, t landingInfraTarget, token string, tokenErr error) infraResult {
	if t.BaseErr != nil {
		return unavailable(fmt.Sprintf("the rig's Forgejo settings name no usable remote URL: %v", t.BaseErr))
	}
	if tokenErr != nil {
		return unavailable("the landing bot's token is unusable: " + tokenErr.Error())
	}
	if token == "" {
		return unavailable("the landing bot's token file holds no token")
	}
	online, total, err := prober.Runners(context.Background(), t.APIBase, token, t.Owner, t.Repo)
	if err != nil {
		return infraResult{available: true, detail: err.Error()}
	}
	if online > 0 {
		return infraResult{available: true, ok: true, online: online}
	}
	detail := fmt.Sprintf("no runner is registered to %s/%s", t.Owner, t.Repo)
	if total > 0 {
		detail = fmt.Sprintf("%d of %d runners online", online, total)
	}
	return infraResult{available: true, detail: detail}
}

// record folds one pass into the state and returns the escalation changes:
// the forgejo condition is red while any covered rig's instance has failed
// for InfraRedAfter, the runner condition while any rig has had no online
// runner for that long. Each raises once and stays raised until the covered
// rigs all read green again.
func (st *landingInfraState) record(now time.Time, tgts []landingInfraTarget, forgejoObs, runnerObs map[string]infraResult) []infraAlertAction {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rigs = st.rigs[:0]
	for _, t := range tgts {
		st.rigs = append(st.rigs, t.Rig)
	}
	st.forgejo = advance(st.forgejo, st.rigs, forgejoObs, now)
	st.runner = advance(st.runner, st.rigs, runnerObs, now)

	var actions []infraAlertAction
	actions = append(actions, st.judge(now,
		alertKeyForgejoDown, st.forgejo, &st.alertedForgejo,
		"the Forgejo instance the landing path uses has not answered for over "+townhealth.Short(townhealth.InfraRedAfter),
		"the Forgejo instance answers again")...)
	actions = append(actions, st.judge(now,
		alertKeyRunnerOffline, st.runner, &st.alertedRunner,
		"no CI runner has been online for over "+townhealth.Short(townhealth.InfraRedAfter),
		"a CI runner is online again")...)
	return actions
}

// observeNothing empties the state when no rig lands through Forgejo: the
// fields go absent, and an escalation still open for a condition nothing
// observes any more is cleared rather than left to alarm forever.
func (st *landingInfraState) observeNothing() []infraAlertAction {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rigs = nil
	st.forgejo = nil
	st.runner = nil
	var actions []infraAlertAction
	if st.alertedForgejo {
		st.alertedForgejo = false
		actions = append(actions, infraAlertAction{key: alertKeyForgejoDown, detail: "no rig lands through Forgejo any more"})
	}
	if st.alertedRunner {
		st.alertedRunner = false
		actions = append(actions, infraAlertAction{key: alertKeyRunnerOffline, detail: "no rig lands through Forgejo any more"})
	}
	return actions
}

// judge decides one condition's escalation: raise it when any covered rig is
// red and none is open, clear it when every covered rig is green and one is
// open, and otherwise leave it alone. A degraded or unanswerable probe is
// neither: clearing on it would close an outage on evidence that does not say
// the outage ended.
func (st *landingInfraState) judge(now time.Time, key string, obs map[string]infraObservation, open *bool, why, recovered string) []infraAlertAction {
	red, green, reds := infraCondition(obs, st.rigs, now)
	switch {
	case red && !*open:
		*open = true
		return []infraAlertAction{{key: key, raise: true, detail: why + ": " + strings.Join(reds, "; ")}}
	case !red && green && *open:
		*open = false
		return []infraAlertAction{{key: key, detail: recovered}}
	default:
		return nil
	}
}

// infraCondition judges every covered rig's probe: red when some rig is red,
// green when every rig is green, and the red rigs' reasons for the alert to
// name.
func infraCondition(obs map[string]infraObservation, rigs []string, now time.Time) (red, green bool, reds []string) {
	green = true
	for _, rig := range rigs {
		o := obs[rig]
		switch townhealth.InfraVerdict(o.probe(), now) {
		case townhealth.Red:
			red, green = true, false
			reds = append(reds, fmt.Sprintf("%s: %s", rig, firstLine(o.detail)))
		case townhealth.Green:
		default:
			green = false
		}
	}
	return red, green, reds
}

// firstLine keeps one client error's first line. A Forgejo error carries the
// server's response body, and a message whose title is cut at the first
// newline should at least be cut at a complete sentence.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// advance folds one pass's results into the previous observations: a failure
// that was already failing keeps the time its run began, and a healthy or
// unanswerable probe clears it.
func advance(prev map[string]infraObservation, rigs []string, results map[string]infraResult, now time.Time) map[string]infraObservation {
	next := make(map[string]infraObservation, len(rigs))
	for _, rig := range rigs {
		res := results[rig]
		o := infraObservation{infraResult: res}
		if res.available && !res.ok {
			if before, ok := prev[rig]; ok && before.available && !before.ok {
				o.since = before.since
			}
			if o.since.IsZero() {
				o.since = now
			}
		}
		next[rig] = o
	}
	return next
}
