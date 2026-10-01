package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/steward"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// Steward monitoring (gt-9bioi.3): the daemon reads the job ledger each scan
// and raises one escalation per condition the overseer must see, recording
// each in the alert record so a condition escalates once.

// stewardProNoticeWindow bounds how far back a hard-preset job still earns
// its notice, so a daemon that was down for a while catches up without
// replaying the ledger's whole retention.
const stewardProNoticeWindow = 24 * time.Hour

// StewardLimits is the job timeout and the hard preset the town's patrol
// config resolves to: what gt steward status needs to read the ledger the way
// the daemon does.
func StewardLimits(config *DaemonPatrolConfig) (timeout time.Duration, hardAgent string) {
	c := stewardConfig(config)
	return stewardJobTimeout(c), stewardHardAgent(c)
}

// healthThresholds is the town's health thresholds, the defaults when the
// config block is broken (the config field reports that).
func (d *Daemon) healthThresholds() (townhealth.Thresholds, time.Duration) {
	th, stale, err := d.loadOperationalConfig().GetHealthSettings().Resolve()
	if err != nil {
		return townhealth.DefaultThresholds(), townhealth.DefaultStaleAfter
	}
	return th, stale
}

func (d *Daemon) stewardEscalator() func(severity, key, source, message string) error {
	if d.stewardEscalate != nil {
		return d.stewardEscalate
	}
	return d.escalateAlertSeverity
}

// stewardStats is the ledger summarized over the health window.
func (d *Daemon) stewardStats(rows []steward.Job, now time.Time) steward.Stats {
	cfg := stewardConfig(d.patrolConfig)
	return steward.Summarize(rows, steward.StatsOptions{
		Since: now.Add(-steward.DefaultStatusWindow), Now: now,
		Timeout: stewardJobTimeout(cfg), HardAgent: stewardHardAgent(cfg), Last: -1,
	})
}

// monitorSteward raises the steward's escalations: a notice for each job on
// the hard preset, an alert for each job stuck past its timeout, and one for
// an error rate over the threshold. Each escalation that fails is retried by
// the next scan, because its key is recorded only once it was raised.
func (d *Daemon) monitorSteward() {
	if !d.isPatrolActive("steward") || d.config == nil {
		return
	}
	townRoot := d.config.TownRoot
	now := d.clk().Now()
	rows, err := steward.NewLedger(steward.LedgerPath(townRoot)).Latest()
	if err != nil {
		d.logger.Printf("steward: monitor: reading the ledger: %v", err)
		return
	}
	store := steward.NewAlerts(steward.AlertsPath(townRoot))
	prior, err := store.Read()
	if err != nil {
		// Escalating without the record could repeat every alert each scan.
		d.logger.Printf("steward: monitor: reading the alert record: %v", err)
		return
	}
	raised := steward.Raised(prior)
	escalate := d.stewardEscalator()
	raise := func(a steward.Alert, severity, message string) {
		if raised[a.Key] {
			return
		}
		if err := escalate(severity, "steward:"+a.Key, "steward", message); err != nil {
			d.logger.Printf("steward: monitor: escalating %s: %v", a.Key, err)
			return
		}
		a.At = now
		if err := store.Record(a); err != nil {
			d.logger.Printf("steward: monitor: recording %s: %v", a.Key, err)
		}
		raised[a.Key] = true
		d.logger.Printf("steward: escalated %s kind=%s severity=%s", a.Key, a.Kind, severity)
	}

	cfg := stewardConfig(d.patrolConfig)
	hard, timeout := stewardHardAgent(cfg), stewardJobTimeout(cfg)
	byKey := map[string][]steward.Job{}
	for _, j := range rows {
		byKey[j.Key()] = append(byKey[j.Key()], j)
	}
	for _, j := range rows {
		if j.Model != hard || j.Started.Before(now.Add(-stewardProNoticeWindow)) {
			continue
		}
		raise(steward.Alert{Key: steward.AlertPro + ":" + j.ID, Kind: steward.AlertPro, Job: j.ID, Bead: j.Bead},
			"low", proNotice(j, byKey[j.Key()], hard))
	}

	stats := d.stewardStats(rows, now)
	for _, j := range stats.Stuck {
		raise(steward.Alert{Key: steward.AlertStuck + ":" + j.ID, Kind: steward.AlertStuck, Job: j.ID, Bead: j.Bead},
			"medium", fmt.Sprintf("Steward job %s (%s of %s on %s, model %s) has run %s, past its %s timeout. The runner kills a job at its timeout, so this one is stuck: check `gt steward status` and the job's process group %d.",
				j.ID, j.Event, j.Bead, j.Rig, j.Model, now.Sub(j.Started).Round(time.Second), timeout, j.Pgid))
	}

	th, _ := d.healthThresholds()
	c := stewardCounters(stats)
	if c.ErrorRateTripped(th) && len(stats.Broken) > 0 {
		// One alert per episode: while the same broken job stays in the
		// window the rate is the same trouble, not a new one.
		first, latest := stats.Broken[0], stats.Broken[len(stats.Broken)-1]
		raise(steward.Alert{Key: steward.AlertErrorRate + ":" + first.ID, Kind: steward.AlertErrorRate, Job: first.ID, Bead: first.Bead},
			"medium", fmt.Sprintf("Steward jobs are failing: %d of %d attempted jobs ended in error or timeout in the last %s (alert above %.0f%%). Latest: %s on %s: %s. See `gt steward status`.",
				stats.Broke, stats.Attempted, c.Window, th.StewardErrorRate*100, latest.ID, latest.Bead, latest.Summary))
	}
}

// proNotice is the message that tells the overseer a job ran on the hard
// preset: every such job is visible, with why it was the hard one.
func proNotice(j steward.Job, history []steward.Job, hard string) string {
	why := "it started on the hard preset (a conflict rejection)"
	for _, h := range history {
		if h.ID != j.ID && h.Model != hard && h.Outcome.Failed() && h.Started.Before(j.Started) {
			why = fmt.Sprintf("it retries job %s, which ended %s on %s", h.ID, h.Outcome, h.Model)
			break
		}
	}
	return fmt.Sprintf("Steward job %s ran on the hard preset %s: %s of %s (%s @ %s); %s. Pro usage is always reported; see `gt steward status`.",
		j.ID, hard, j.Event, j.Bead, j.Branch, steward.ShortHead(j.Head), why)
}

// stewardCounters maps a window of stats to the counters the health report
// publishes.
func stewardCounters(s steward.Stats) townhealth.StewardCounters {
	return townhealth.StewardCounters{
		Window:  townhealth.Short(steward.DefaultStatusWindow),
		Running: len(s.Running), Stuck: len(s.Stuck),
		Jobs: s.Jobs, Finished: s.Finished, Attempted: s.Attempted,
		Broke: s.Broke, Escalated: s.Escalated, Pro: s.Pro,
	}
}

// Steward answers the health report's steward source from the job ledger. A
// town without the steward patrol is not unhealthy: the report omits the
// field.
func (s *healthSources) Steward(ctx context.Context) (townhealth.StewardSnapshot, error) {
	if !s.d.isPatrolActive("steward") {
		return townhealth.StewardSnapshot{}, nil
	}
	if err := ctx.Err(); err != nil {
		return townhealth.StewardSnapshot{}, err
	}
	rows, err := steward.NewLedger(steward.LedgerPath(s.townRoot())).Latest()
	if err != nil {
		return townhealth.StewardSnapshot{}, err
	}
	return townhealth.StewardSnapshot{Enabled: true, Counters: stewardCounters(s.d.stewardStats(rows, s.now))}, nil
}
