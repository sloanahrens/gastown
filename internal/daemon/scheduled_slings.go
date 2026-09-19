package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ScheduledSlingsConfig is the opt-in scheduled_slings patrol: each entry is a
// formula slung onto a rig on an interval, one bead per run (gt-nj23).
type ScheduledSlingsConfig struct {
	Enabled bool                  `json:"enabled"`
	Entries []ScheduledSlingEntry `json:"entries,omitempty"`
}

// ScheduledSlingEntry is one scheduled formula. Name is the schedule's
// identity: runs are found by the label "scheduled:<name>".
type ScheduledSlingEntry struct {
	Name        string            `json:"name"`
	Rig         string            `json:"rig"`
	Formula     string            `json:"formula"`
	Agent       string            `json:"agent,omitempty"`
	IntervalStr string            `json:"interval"`
	Priority    int               `json:"priority,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"`
}

const defaultScheduledSlingPriority = 3

var scheduledSlingNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (e ScheduledSlingEntry) validate() error {
	if !scheduledSlingNameRe.MatchString(e.Name) {
		return fmt.Errorf("scheduled_slings: name %q must match %s", e.Name, scheduledSlingNameRe)
	}
	if e.Rig == "" || e.Formula == "" {
		return fmt.Errorf("scheduled_slings[%s]: rig and formula are required", e.Name)
	}
	d, err := time.ParseDuration(e.IntervalStr)
	if err != nil || d <= 0 {
		return fmt.Errorf("scheduled_slings[%s]: interval %q must be a positive Go duration", e.Name, e.IntervalStr)
	}
	return nil
}

func (e ScheduledSlingEntry) interval() time.Duration {
	d, _ := time.ParseDuration(e.IntervalStr)
	return d
}

func (e ScheduledSlingEntry) label() string { return "scheduled:" + e.Name }

func (e ScheduledSlingEntry) priority() int {
	if e.Priority == 0 {
		return defaultScheduledSlingPriority
	}
	return e.Priority
}

// scheduledBead is the slice of a run bead the decision needs.
type scheduledBead struct {
	ID        string
	Status    string
	CreatedAt time.Time
}

type scheduledAction int

const (
	scheduledSkipOpen   scheduledAction = iota // a run is in flight or its MR is queued
	scheduledSkipRecent                        // the newest run started less than an interval ago
	scheduledDispatch
)

func (a scheduledAction) String() string {
	switch a {
	case scheduledSkipOpen:
		return "skip-open"
	case scheduledSkipRecent:
		return "skip-recent"
	default:
		return "dispatch"
	}
}

// decideScheduledSling is pure: the beads carrying the entry's label, the
// entry's interval, and now. Any open bead wins over recency so a run whose
// MR is still in the queue is never doubled.
func decideScheduledSling(beads []scheduledBead, interval time.Duration, now time.Time) scheduledAction {
	var newest time.Time
	for _, b := range beads {
		if b.Status != "closed" {
			return scheduledSkipOpen
		}
		if b.CreatedAt.After(newest) {
			newest = b.CreatedAt
		}
	}
	if !newest.IsZero() && now.Sub(newest) < interval {
		return scheduledSkipRecent
	}
	return scheduledDispatch
}

// parseScheduledBeads reads `bd list --json` output.
func parseScheduledBeads(data []byte) ([]scheduledBead, error) {
	var rows []struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}
	out := make([]scheduledBead, 0, len(rows))
	for _, r := range rows {
		out = append(out, scheduledBead{ID: r.ID, Status: r.Status, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// parseCreatedBeadID reads `bd create --json`, which some bd versions print
// as an object and others as a one-element array.
func parseCreatedBeadID(data []byte) (string, error) {
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && obj.ID != "" {
		return obj.ID, nil
	}
	var arr []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 && arr[0].ID != "" {
		return arr[0].ID, nil
	}
	return "", errors.New("bd create output has no id")
}

// scheduledSlingRunner is the side-effect boundary: bd list, bd create, gt sling.
type scheduledSlingRunner interface {
	listBeads(ctx context.Context, rig, label string) ([]scheduledBead, error)
	createBead(ctx context.Context, rig, title, label, description string, priority int) (string, error)
	sling(ctx context.Context, beadID string, e ScheduledSlingEntry) error
}

const (
	scheduledSlingsTickInterval  = 15 * time.Minute
	scheduledSlingCommandTimeout = 5 * time.Minute
	scheduledSlingEscalateAfter  = 3
)

type execScheduledSlingRunner struct {
	townRoot, bdPath, gtPath string
}

func (r *execScheduledSlingRunner) runBd(ctx context.Context, rig string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.bdPath, args...)
	// ConfigureCommand sets cmd.Dir to the rig dir, so bd's cwd routing lands
	// on the rig database (never --repo: see the bd-create-repo memory).
	rigDir := filepath.Join(r.townRoot, rig)
	beads.ConfigureCommand(cmd, rigDir, filepath.Join(rigDir, ".beads"), beads.SubprocessModeForArgs(args))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (r *execScheduledSlingRunner) listBeads(ctx context.Context, rig, label string) ([]scheduledBead, error) {
	out, err := r.runBd(ctx, rig, "list", "--label", label, "--json", "--limit", "0", "--brief")
	if err != nil {
		return nil, err
	}
	return parseScheduledBeads(out)
}

func (r *execScheduledSlingRunner) createBead(ctx context.Context, rig, title, label, description string, priority int) (string, error) {
	out, err := r.runBd(ctx, rig, "create", "--title", title, "--type", "task",
		"--priority", fmt.Sprint(priority), "--labels", label, "--description", description, "--json")
	if err != nil {
		return "", err
	}
	return parseCreatedBeadID(out)
}

func (r *execScheduledSlingRunner) slingArgs(beadID string, e ScheduledSlingEntry) []string {
	args := []string{"sling", beadID, e.Rig, "--formula=" + e.Formula}
	if e.Agent != "" {
		args = append(args, "--agent="+e.Agent)
	}
	args = append(args, "--actor=daemon/scheduled:"+e.Name, "--no-boot")
	keys := make([]string, 0, len(e.Vars))
	for k := range e.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--var", k+"="+e.Vars[k])
	}
	return args
}

func (r *execScheduledSlingRunner) sling(ctx context.Context, beadID string, e ScheduledSlingEntry) error {
	cmd := exec.CommandContext(ctx, r.gtPath, r.slingArgs(beadID, e)...)
	cmd.Dir = r.townRoot
	cmd.Env = bdMutationRoutingEnv(r.townRoot)
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gt sling %s: %w: %s", beadID, err, slingErrorLine(stderr.String()))
	}
	return nil
}

func (d *Daemon) scheduledRunner() scheduledSlingRunner {
	if d.scheduledSlingRunner == nil {
		d.scheduledSlingRunner = &execScheduledSlingRunner{townRoot: d.config.TownRoot, bdPath: d.bdPath, gtPath: d.gtPath}
	}
	return d.scheduledSlingRunner
}

// triggerScheduledSlings starts a cycle on its own goroutine; an overlapping
// tick is skipped. Returns true if a cycle started.
func (d *Daemon) triggerScheduledSlings() bool {
	if !d.scheduledSlingsRunning.CompareAndSwap(false, true) {
		d.logger.Printf("scheduled_slings: previous cycle still running, skipping this tick")
		return false
	}
	go func() {
		defer d.scheduledSlingsRunning.Store(false)
		d.runScheduledSlings()
	}()
	return true
}

func (d *Daemon) runScheduledSlings() {
	if !d.isPatrolActive("scheduled_slings") {
		return
	}
	cfg := d.patrolConfig.Patrols.ScheduledSlings
	if d.scheduledSlingFailures == nil {
		d.scheduledSlingFailures = map[string]int{}
	}
	if d.scheduledSlingEscalate == nil {
		d.scheduledSlingEscalate = d.escalate
	}
	now := time.Now()
	for _, e := range cfg.Entries {
		if err := e.validate(); err != nil {
			d.logger.Printf("scheduled_slings: %v (entry skipped)", err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), scheduledSlingCommandTimeout)
		err := d.runScheduledSlingEntry(ctx, e, now)
		cancel()
		if err == nil {
			d.scheduledSlingFailures[e.Name] = 0
			continue
		}
		d.scheduledSlingFailures[e.Name]++
		n := d.scheduledSlingFailures[e.Name]
		d.logger.Printf("scheduled_slings: %s: failure %d: %v", e.Name, n, err)
		if n == scheduledSlingEscalateAfter {
			d.scheduledSlingEscalate("scheduled_slings",
				fmt.Sprintf("scheduled sling %s (%s on %s) failed %d ticks in a row; last error: %v",
					e.Name, e.Formula, e.Rig, n, err))
		}
	}
}

// runScheduledSlingEntry evaluates one entry and dispatches when due.
func (d *Daemon) runScheduledSlingEntry(ctx context.Context, e ScheduledSlingEntry, now time.Time) error {
	r := d.scheduledRunner()
	beadsForLabel, err := r.listBeads(ctx, e.Rig, e.label())
	if err != nil {
		return err
	}
	action := decideScheduledSling(beadsForLabel, e.interval(), now)
	if action != scheduledDispatch {
		d.logger.Printf("scheduled_slings: %s: %s", e.Name, action)
		return nil
	}
	title := fmt.Sprintf("%s %s", e.Name, now.UTC().Format("2006-01-02"))
	desc := fmt.Sprintf("Scheduled run of formula %s on rig %s, created by the daemon's scheduled_slings patrol. Label %s is the run's identity; while this bead is open no second run is dispatched.", e.Formula, e.Rig, e.label())
	id, err := r.createBead(ctx, e.Rig, title, e.label(), desc, e.priority())
	if err != nil {
		return err
	}
	d.logger.Printf("scheduled_slings: %s: created %s, slinging %s to %s (agent %q)", e.Name, id, e.Formula, e.Rig, e.Agent)
	if err := r.sling(ctx, id, e); err != nil {
		return err
	}
	return nil
}
