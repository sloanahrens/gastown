package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/exectax"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// townHealthTimeout bounds one health computation; each bd read inside it
// has its own shorter bound.
const (
	townHealthTimeout   = 2 * time.Minute
	townHealthBDTimeout = 30 * time.Second
	// execTaxProbeTimeout bounds the probe's ten execs, so a hung exec is
	// an UNKNOWN field rather than a heartbeat that never finishes.
	execTaxProbeTimeout = 30 * time.Second
)

// writeTownHealth is the heartbeat's last step (gt-s3rec.2): it computes the
// town health report from the daemon's own records and live probes and
// writes it to the one health file gt status --line reads. It runs under the
// e-stop too: health is upkeep, and an e-stopped town still wants its line.
func (d *Daemon) writeTownHealth() {
	op := d.loadOperationalConfig()
	th, stale, err := op.GetHealthSettings().Resolve()
	if err != nil {
		// The config field reports the broken block; the computation still
		// runs on the defaults rather than leaving the file to go stale.
		th, stale = townhealth.DefaultThresholds(), townhealth.DefaultStaleAfter
	}
	src := &healthSources{d: d, evidence: th.SeatEvidence}
	if d.townHealthSources != nil {
		d.townHealthSources(src)
	}
	// The transition notice compares against the report the last tick left
	// on disk, so it is read before this tick replaces it.
	prevReport := d.previousHealth()
	ctx, cancel := context.WithTimeout(context.Background(), townHealthTimeout)
	defer cancel()
	r := townhealth.Compute(ctx, src.inputs(d.clk().Now(), th, d.lastTownHealth))
	// One line per change of the exec-tax verdict, not one per beat: the
	// tax is a property of the daemon's context, and a daemon that has it
	// is restarted, not repeated (gt-2ycne.1).
	prev, now := execTaxState(d.lastTownHealth, th), execTaxState(&r, th)
	if line := exectax.Transition(prev, now); line != "" {
		d.logger.Println(line)
	}
	d.lastTownHealth = &r
	if err := townhealth.Write(d.config.TownRoot, r); err != nil {
		d.logger.Printf("townhealth: writing %s: %v", townhealth.Path(d.config.TownRoot), err)
		return
	}
	line := townhealth.Line(r, r.At, stale)
	d.logger.Printf("townhealth: %s", line)
	d.notifyHealthTransition(prevReport, r, line)
}

// healthSources answers townhealth's sources from the daemon's records and
// clients. Each probe is a field so tests replace the ones that reach
// outside the town directory.
type healthSources struct {
	d *Daemon
	// evidence is how fresh a seat's liveness sample must be for the seat
	// to be judged.
	evidence time.Duration
	now      time.Time

	ping       func() (time.Duration, error)
	execTax    func(ctx context.Context) (time.Duration, error)
	backupRoot func() (string, error)
	slots      func() (slot.Report, error)
}

// execTaxState is what a report said about the exec tax, for the transition
// log. A nil report, or one from before the probe measured, is unknown.
func execTaxState(r *townhealth.Report, th townhealth.Thresholds) exectax.State {
	if r == nil {
		return exectax.State{}
	}
	return exectax.StateOf(r.ExecTaxMS, th.ExecTax.Red)
}

func (s *healthSources) inputs(now time.Time, th townhealth.Thresholds, prev *townhealth.Report) townhealth.Inputs {
	s.now = now
	return townhealth.Inputs{
		Now: now, Thresholds: th, Prev: prev,
		Dolt: s, ExecTax: s, Heartbeat: s, Ticks: s, Landings: s, Escalations: s, Slots: s,
		Backups: s, Mains: s, Config: s, NeedsHuman: s, Seats: s, Steward: s,
	}
}

func (s *healthSources) townRoot() string { return s.d.config.TownRoot }

func (s *healthSources) Ping(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.ping != nil {
		return s.ping()
	}
	return doltserver.MeasureQueryLatency(s.townRoot())
}

// ExecTax measures the daemon's own process tree, which is where the landing
// gate runs: a daemon whose tree pays a macOS scan per new executable makes
// every landing's gate pay it too (gt-2ycne.1).
func (s *healthSources) ExecTax(ctx context.Context) (time.Duration, error) {
	if s.execTax != nil {
		return s.execTax(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, execTaxProbeTimeout)
	defer cancel()
	res, err := exectax.Probe(ctx, exectax.Options{})
	if err != nil {
		return 0, err
	}
	return res.Median, nil
}

func (s *healthSources) Heartbeat() (townhealth.HeartbeatRecord, error) {
	st, err := LoadState(s.townRoot())
	if err != nil {
		return townhealth.HeartbeatRecord{}, err
	}
	return townhealth.HeartbeatRecord{At: st.LastHeartbeat, Count: st.HeartbeatCount}, nil
}

// Ticks lists the enabled patrols whose last run the daemon persists
// (patrol_last_run.json, gt-ima2), each with its run interval.
func (s *healthSources) Ticks() ([]townhealth.Tick, error) {
	cfg := s.d.patrolConfig
	pruneEvery, _ := eventsPruneSettings(cfg)
	all := []struct {
		name     string
		interval time.Duration
	}{
		{"jsonl_git_backup", jsonlGitBackupInterval(cfg)},
		{"wisp_reaper", wispReaperInterval(cfg)},
		{"compactor_dog", compactorDogInterval(cfg)},
		{"checkpoint_dog", checkpointDogInterval(cfg)},
		{"mayor_dispatch", mayorDispatchInterval(cfg)},
		{"git_hygiene", gitHygieneInterval(cfg)},
		{"events_prune", pruneEvery},
	}
	var out []townhealth.Tick
	for _, p := range all {
		if !s.d.isPatrolActive(p.name) {
			continue
		}
		last, _, err := loadPatrolLastRun(s.townRoot(), p.name)
		if err != nil {
			return nil, err
		}
		out = append(out, townhealth.Tick{Name: p.name, Interval: p.interval, LastFired: last})
	}
	return out, nil
}

// landingRigs are the rigs a landing worker serves; none when it is off.
func (s *healthSources) landingRigs() []string {
	if !s.d.isPatrolActive("landing_worker") {
		return nil
	}
	return landingWorkerRigs(s.d.patrolConfig, s.d.getKnownRigs())
}

func (s *healthSources) Landings(ctx context.Context, since time.Time) ([]townhealth.RigLandings, error) {
	var out []townhealth.RigLandings
	for _, rig := range s.landingRigs() {
		rl := townhealth.RigLandings{Rig: rig}
		if err := ctx.Err(); err != nil {
			rl.Err = err
			out = append(out, rl)
			continue
		}
		rl.Last, rl.Landed, rl.Err = s.landingHistory(rig, since)
		if rl.Err == nil {
			rl.Pending, rl.Err = s.countOpen(rig, land.LabelReadyToLand, nil)
		}
		out = append(out, rl)
	}
	return out, nil
}

// landingHistory reads a rig's landings file: the newest landing and how
// many landed at or after since.
func (s *healthSources) landingHistory(rig string, since time.Time) (time.Time, int, error) {
	path, err := landings.Path(s.townRoot(), rig)
	if err != nil {
		return time.Time{}, 0, err
	}
	recs, _, err := (&landings.Reader{Path: path}).ReadNew()
	if err != nil {
		return time.Time{}, 0, err
	}
	var last time.Time
	n := 0
	for _, r := range recs {
		if r.LandedAt.After(last) {
			last = r.LandedAt
		}
		if !r.LandedAt.Before(since) {
			n++
		}
	}
	return last, n, nil
}

// countOpen counts rig's pending beads carrying label, and reports each one's
// creation time to seen when it is set.
//
// Pending is beads.IsActionable, not non-closed: a deferred bead is parked by
// the operator, so counting it holds the field at a constant and hides the
// beads that do need attention (gt-tk2xd).
func (s *healthSources) countOpen(rig, label string, seen func(created time.Time)) (int, error) {
	issues, err := s.d.workBeads(s.d.workBeadsEnv(rig), townHealthBDTimeout).List(beads.ListOptions{Label: label, Priority: -1})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, is := range issues {
		if is == nil || !beads.IssueStatus(strings.TrimSpace(is.Status)).IsActionable() {
			continue
		}
		n++
		if seen != nil {
			if t, err := time.Parse(time.RFC3339, is.CreatedAt); err == nil {
				seen(t)
			}
		}
	}
	return n, nil
}

// OldestEscalation reads the town's open escalations the way `gt escalate list`
// reads them: both bead planes, minus the mail carriers routed for each
// escalation. A carrier carries gt:escalation and outlives the escalation it
// delivered, so counting one ages the field past every row the display path
// shows (gt-9k2bx).
//
// The read is pinned to the town database, where notify.Raise files every
// escalation.
func (s *healthSources) OldestEscalation(ctx context.Context) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	env := bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(s.townRoot()))
	issues, err := s.d.workBeads(env, townHealthBDTimeout).List(beads.ListOptions{
		Label: "gt:escalation", Status: "open", IncludeInfra: true, Priority: -1,
	})
	if err != nil {
		return time.Time{}, false, err
	}
	var oldest time.Time
	for _, is := range issues {
		if !beads.IsEscalationRecord(is) {
			continue
		}
		t, err := time.Parse(time.RFC3339, is.CreatedAt)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("escalation %s: created_at %q: %w", is.ID, is.CreatedAt, err)
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest, !oldest.IsZero(), nil
}

func (s *healthSources) SlotHolders() ([]townhealth.SlotHolder, error) {
	var rep slot.Report
	var err error
	if s.slots != nil {
		rep, err = s.slots()
	} else {
		pool := slot.PoolFromConfig(s.d.loadOperationalConfig().GetContainerGateConfig())
		rep, err = slot.StatusPoolLocksOnly(s.townRoot(), pool)
	}
	if err != nil {
		return nil, err
	}
	var out []townhealth.SlotHolder
	for _, st := range rep.Slots {
		if !st.Held {
			continue
		}
		h := townhealth.SlotHolder{Name: fmt.Sprintf("slot%d", st.Index)}
		if st.Owner != nil {
			h.Since = st.Owner.AcquiredAt
			if st.Owner.Role != "" {
				h.Name = st.Owner.Role
			}
		}
		if st.Name != "" {
			h.Name = st.Name
		}
		out = append(out, h)
	}
	return out, nil
}

func (s *healthSources) NewestBackup() (time.Time, bool, error) {
	rootFn := s.backupRoot
	if rootFn == nil {
		rootFn = doltbackup.DefaultRoot
	}
	root, err := rootFn()
	if err != nil {
		return time.Time{}, false, err
	}
	b, ok, err := doltbackup.Newest(root)
	if err != nil || !ok {
		return time.Time{}, ok, err
	}
	return b.Manifest.Finished, true, nil
}

func (s *healthSources) Mains() ([]townhealth.RigMain, error) {
	var out []townhealth.RigMain
	for _, rig := range s.landingRigs() {
		st, err := fileMainState{path: RedMainStatePath(s.townRoot(), rig)}.Load()
		out = append(out, townhealth.RigMain{Rig: rig, LastRun: st.LastRun, LastGreen: st.LastGreen, Err: err})
	}
	return out, nil
}

// Validate loads the town config kernel and resolves the health block.
func (s *healthSources) Validate() error {
	town, err := townconfig.Load(s.townRoot())
	if err != nil {
		return err
	}
	_, _, err = town.Operational().GetHealthSettings().Resolve()
	return err
}

// NeedsHuman counts the landing rigs' beads Land refused on a policy only
// the operator can lift (gt:needs-human).
func (s *healthSources) NeedsHuman(ctx context.Context) (int, time.Time, error) {
	total := 0
	var oldest time.Time
	for _, rig := range s.landingRigs() {
		if err := ctx.Err(); err != nil {
			return 0, time.Time{}, err
		}
		n, err := s.countOpen(rig, land.LabelNeedsHuman, func(t time.Time) {
			if oldest.IsZero() || t.Before(oldest) {
				oldest = t
			}
		})
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("%s: %w", rig, err)
		}
		total += n
	}
	return total, oldest, nil
}

// seatHome returns the directory a seat's agent lives in, and whether this
// walk knows that mapping. It tells a seat that exists from a record that
// outlived one, so the walk drops a record whose seat is gone rather than
// reporting it dead forever (gt-u7voe).
//
// Only the polecat role is mapped: a polecat's directory is created and
// destroyed with the seat, so its absence is decisive, where a witness or
// refinery directory belongs to the rig and a rig missing one is a problem
// this field must not report by dropping the seat.
func seatHome(townRoot, rig, stem string) (string, bool) {
	role, name, named := strings.Cut(stem, ".")
	if !named || rig == "" || role != constants.RolePolecat {
		return "", false
	}
	return filepath.Join(townRoot, rig, "polecats", name), true
}

// Seats reads every intent record under .runtime/agents, skipping the records
// of seats whose directory is gone. Only seats the daemon is sampling (a
// liveness sample fresher than the evidence window) carry stall evidence; a
// frozen seat is listed whatever its sample.
func (s *healthSources) Seats() ([]townhealth.Seat, error) {
	dir := filepath.Join(constants.TownRuntimePath(s.townRoot()), "agents")
	var out []townhealth.Seat
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == dir {
				return filepath.SkipDir
			}
			return err
		}
		if e.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rig := filepath.Dir(rel)
		if rig == "." {
			rig = ""
		}
		stem := strings.TrimSuffix(filepath.Base(rel), ".json")
		if home, known := seatHome(s.townRoot(), rig, stem); known {
			if _, serr := os.Stat(home); serr != nil {
				return nil // the seat is gone; the record outlived it
			}
		}
		name := strings.Replace(stem, ".", "/", 1)
		rec, rerr := intent.ReadPath(path)
		if rerr != nil {
			return fmt.Errorf("%s: %w", rel, rerr)
		}
		seat := townhealth.Seat{Rig: rig, Name: name, Run: rec.EffectiveDesired() == intent.DesiredRun, Frozen: rec.Frozen}
		if p := rec.Progress; p != nil {
			seat.Sampled, seat.Changed, seat.DeadSamples = p.SampledAt, p.ChangedAt, p.DeadSamples
		}
		sampling := !seat.Sampled.IsZero() && (s.evidence <= 0 || s.now.Sub(seat.Sampled) < s.evidence)
		if seat.Frozen || (seat.Run && sampling) {
			out = append(out, seat)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rig+"/"+out[i].Name < out[j].Rig+"/"+out[j].Name })
	return out, nil
}
