package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

const polecatAdmissionReservationTTL = 30 * time.Minute

var acquirePolecatAdmissionFn = acquirePolecatAdmission

type polecatCapacitySnapshot struct {
	Max             int `json:"max"`
	Working         int `json:"working"`
	RecoveryBlocked int `json:"recovery_blocked"`
	ReusableIdle    int `json:"reusable_idle"`
	Parked          int `json:"parked"`
	PendingMR       int `json:"pending_mr"`
	Reservations    int `json:"reservations"`
	Free            int `json:"free"`
	ActiveSessions  int `json:"active_sessions"`
	capacityUsed    int

	// rigUsed and rigReservations break the town totals down by rig so a per-rig
	// cap is checked from the same scan that answers the town cap (gt-1kbi).
	// Unexported: accounting detail, not part of the reported snapshot JSON.
	rigUsed         map[string]int
	rigReservations map[string]int
}

func (s polecatCapacitySnapshot) occupied() int {
	return s.capacityUsed + s.Reservations
}

// addRigOccupancy scans one rig's polecats into the snapshot and records how
// many of them belong to that rig, so a per-rig cap reads the same numbers the
// town total is built from.
func (s *polecatCapacitySnapshot) addRigOccupancy(townRoot, rigName string, sessions polecatSessionSet) error {
	usedBefore := s.capacityUsed
	if err := applyRigOccupancyToCapacitySnapshot(s, townRoot, rigName, sessions); err != nil {
		return err
	}
	s.trackRigUsage(rigName, s.capacityUsed-usedBefore)
	return nil
}

// trackRigUsage records that count of the polecats counted so far belong to
// rigName.
func (s *polecatCapacitySnapshot) trackRigUsage(rigName string, count int) {
	if rigName == "" || count == 0 {
		return
	}
	if s.rigUsed == nil {
		s.rigUsed = make(map[string]int)
	}
	s.rigUsed[rigName] += count
}

// trackReservations records the town's live admission reservations, in total
// and per rig: a reservation is a slot another sling is already taking, and a
// rig cap must see the ones aimed at its own rig.
func (s *polecatCapacitySnapshot) trackReservations(townRoot string) error {
	reservations, err := readPolecatAdmissionReservations(townRoot)
	if err != nil {
		return err
	}
	s.Reservations = len(reservations)
	for _, reservation := range reservations {
		s.trackRigReservation(reservation.Rig)
	}
	return nil
}

// trackRigReservation records one live admission reservation for rigName.
func (s *polecatCapacitySnapshot) trackRigReservation(rigName string) {
	if rigName == "" {
		return
	}
	if s.rigReservations == nil {
		s.rigReservations = make(map[string]int)
	}
	s.rigReservations[rigName]++
}

// rigOccupied counts the slots one rig holds: polecats there that consume
// capacity, plus live admission reservations naming that rig, so two poles
// racing to sling into one capped rig cannot both get in.
func (s polecatCapacitySnapshot) rigOccupied(rigName string) int {
	return s.rigUsed[rigName] + s.rigReservations[rigName]
}

func (s *polecatCapacitySnapshot) addWorking() {
	s.Working++
	s.capacityUsed++
}

func (s *polecatCapacitySnapshot) addRecoveryBlocked(countsTowardCapacity bool) {
	s.RecoveryBlocked++
	if countsTowardCapacity {
		s.capacityUsed++
	}
}

func (s *polecatCapacitySnapshot) addReusableIdle() {
	s.ReusableIdle++
}

// addParked counts an idle slot that would be reusable but for its park marker.
// It is kept apart from ReusableIdle so an operator reading free capacity is
// not told a parked slot can take work (gt-q6nrm), and apart from
// RecoveryBlocked because resuming the park, not recovering work, frees it.
func (s *polecatCapacitySnapshot) addParked() {
	s.Parked++
}

func (s *polecatCapacitySnapshot) addPendingMR() {
	s.PendingMR++
}

type polecatAdmissionReservation struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	Rig       string    `json:"rig,omitempty"`
	Bead      string    `json:"bead,omitempty"`
	Operation string    `json:"operation"`
	CreatedAt time.Time `json:"created_at"`
}

type polecatAdmissionHandle struct {
	townRoot string
	id       string
	path     string
	disabled bool
}

func (h *polecatAdmissionHandle) Release() {
	if h == nil || h.disabled || h.path == "" {
		return
	}
	_ = os.Remove(h.path)
}

type polecatCapacityAdmissionError struct {
	Snapshot polecatCapacitySnapshot
	Rig      string
	Bead     string
	Reason   string
	// RigMax is the rig's own max_polecats when that cap is what refused the
	// admission, and RigUsed how many slots the rig already held. Zero means
	// the town-wide scheduler.max_polecats cap refused instead.
	RigMax  int
	RigUsed int
}

func (e *polecatCapacityAdmissionError) Error() string {
	if e == nil {
		return "polecat admission denied"
	}
	if e.RigMax > 0 {
		return fmt.Sprintf(
			"polecat admission denied: %s (rig=%s max=%d occupied=%d free=%d). Raise the rig cap with `gt rig config set %s max_polecats N`, or cap the whole town instead with `gt config set scheduler.max_polecats N`; inspect with `gt polecat list %s`",
			e.Reason,
			e.Rig,
			e.RigMax,
			e.RigUsed,
			e.RigMax-e.RigUsed,
			e.Rig,
			e.Rig,
		)
	}
	if e.Snapshot.Max <= 0 {
		return fmt.Sprintf("polecat admission denied: %s", e.Reason)
	}
	return fmt.Sprintf(
		"polecat admission denied: %s (max=%d occupied=%d working=%d recovery_blocked=%d reservations=%d reusable_idle=%d parked=%d pending_mr=%d free=%d). Resolve recovery-needed polecats or raise scheduler.max_polecats; inspect with `gt scheduler status --json` or `gt polecat list --all --json`",
		e.Reason,
		e.Snapshot.Max,
		e.Snapshot.occupied(),
		e.Snapshot.Working,
		e.Snapshot.RecoveryBlocked,
		e.Snapshot.Reservations,
		e.Snapshot.ReusableIdle,
		e.Snapshot.Parked,
		e.Snapshot.PendingMR,
		e.Snapshot.Free,
	)
}

// acquirePolecatAdmission takes a capacity reservation for one polecat, or
// refuses with a polecatCapacityAdmissionError.
//
// Two caps can refuse: the town-wide scheduler.max_polecats, and the target
// rig's own max_polecats (gt-1kbi). Both are read here because this is the one
// admission chokepoint every sling and spawn path goes through, so a rig cap
// binds in direct dispatch as well as under a town cap.
func acquirePolecatAdmission(townRoot, rigName, beadID, operation string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error) {
	max, err := configuredSchedulerMaxPolecats(townRoot)
	if err != nil {
		return nil, polecatCapacitySnapshot{}, err
	}
	rigMax := configuredRigMaxPolecats(townRoot, rigName)
	if max <= 0 && rigMax <= 0 {
		return &polecatAdmissionHandle{disabled: true}, polecatCapacitySnapshot{Max: max, ActiveSessions: countActivePolecats()}, nil
	}

	lock, err := acquirePolecatAdmissionLock(townRoot)
	if err != nil {
		return nil, polecatCapacitySnapshot{}, err
	}
	defer func() { _ = lock.Unlock() }()

	if err := cleanupStalePolecatAdmissionReservations(townRoot, time.Now()); err != nil {
		return nil, polecatCapacitySnapshot{}, err
	}

	// A town cap needs the whole-town snapshot; a rig cap alone (the direct
	// dispatch case) only needs that rig counted, so it never pays for a scan
	// of every rig in the town.
	snapshot := polecatCapacitySnapshot{Max: max, ActiveSessions: countActivePolecats()}
	if max > 0 {
		snapshot, err = polecatCapacitySnapshotForTownNoCleanup(townRoot)
		if err != nil {
			return nil, polecatCapacitySnapshot{}, err
		}
		if snapshot.Free <= 0 {
			return nil, snapshot, &polecatCapacityAdmissionError{
				Snapshot: snapshot,
				Rig:      rigName,
				Bead:     beadID,
				Reason:   "configured scheduler.max_polecats capacity is full",
			}
		}
	}
	if rigMax > 0 {
		rigSnapshot := snapshot
		if max <= 0 {
			rigSnapshot, err = polecatRigOccupancySnapshot(townRoot, rigName)
			if err != nil {
				return nil, polecatCapacitySnapshot{}, err
			}
		}
		if occupied := rigSnapshot.rigOccupied(rigName); occupied >= rigMax {
			return nil, snapshot, &polecatCapacityAdmissionError{
				Snapshot: snapshot,
				Rig:      rigName,
				Bead:     beadID,
				RigMax:   rigMax,
				RigUsed:  occupied,
				Reason:   "configured rig max_polecats capacity is full",
			}
		}
	}

	reservation, path, err := writePolecatAdmissionReservation(townRoot, rigName, beadID, operation)
	if err != nil {
		return nil, snapshot, err
	}
	snapshot.Reservations++
	snapshot.trackRigReservation(rigName)
	if snapshot.Max > 0 {
		snapshot.Free--
	}
	return &polecatAdmissionHandle{townRoot: townRoot, id: reservation.ID, path: path}, snapshot, nil
}

func configuredSchedulerMaxPolecats(townRoot string) (int, error) {
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return 0, fmt.Errorf("loading town settings for polecat admission: %w", err)
	}
	schedulerCfg := settings.Scheduler
	if schedulerCfg == nil {
		schedulerCfg = capacity.DefaultSchedulerConfig()
	}
	return schedulerCfg.GetMaxPolecats(), nil
}

// configuredRigMaxPolecats returns rigName's own concurrency cap, or 0 when the
// rig has none. The value is the rig's `max_polecats` config key, which the
// operator sets with `gt rig config set <rig> max_polecats N`; its compiled-in
// default is 0, so an unset rig is uncapped and only a rig someone deliberately
// capped is throttled (gt-1kbi).
//
// A town or rigs.json that cannot be read counts as "no rig cap" rather than an
// error. Admission sits on every spawn path, and a rig whose config cannot be
// resolved must not be unable to start polecats; the town-wide
// scheduler.max_polecats cap still applies on its own.
func configuredRigMaxPolecats(townRoot, rigName string) int {
	if townRoot == "" || rigName == "" {
		return 0
	}
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return 0
	}
	entry, ok := rigsConfig.Rigs[rigName]
	if !ok {
		return 0
	}
	// Built the same way rig.Manager.loadRig builds it (name, path, beads
	// config), which is the identity `gt rig config show` resolves the three
	// config layers against.
	r := &rig.Rig{Name: rigName, Path: filepath.Join(townRoot, rigName), Config: entry.BeadsConfig}
	if cap := r.GetIntConfig("max_polecats"); cap > 0 {
		return cap
	}
	return 0
}

func polecatCapacitySnapshotForTown(townRoot string) (polecatCapacitySnapshot, error) {
	max, err := configuredSchedulerMaxPolecats(townRoot)
	if err != nil {
		return polecatCapacitySnapshot{}, err
	}
	if max > 0 {
		if err := cleanupStalePolecatAdmissionReservationsWithLock(townRoot, time.Now()); err != nil {
			return polecatCapacitySnapshot{}, err
		}
	}
	return polecatCapacitySnapshotForTownNoCleanup(townRoot)
}

func polecatCapacitySnapshotForTownNoCleanup(townRoot string) (polecatCapacitySnapshot, error) {
	max, err := configuredSchedulerMaxPolecats(townRoot)
	if err != nil {
		return polecatCapacitySnapshot{}, err
	}
	snapshot := polecatCapacitySnapshot{Max: max, ActiveSessions: countActivePolecats()}
	if max <= 0 {
		return snapshot, nil
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return snapshot, fmt.Errorf("loading rigs config for polecat capacity: %w", err)
	}

	sessions, err := currentPolecatSessions()
	if err != nil {
		return snapshot, err
	}
	for rigName := range rigsConfig.Rigs {
		if err := snapshot.addRigOccupancy(townRoot, rigName, sessions); err != nil {
			return snapshot, err
		}
	}

	if err := snapshot.trackReservations(townRoot); err != nil {
		return snapshot, err
	}
	if max > 0 {
		snapshot.Free = max - snapshot.occupied()
		if snapshot.Free < 0 {
			snapshot.Free = 0
		}
	}
	return snapshot, nil
}

// applyRigOccupancyToCapacitySnapshot adds one rig's polecats to snapshot, the
// same accounting the town-wide scan applies to every rig. It is also the whole
// scan for a single capped rig in direct dispatch (gt-1kbi), so it must not
// depend on any town-level state beyond townRoot.
func applyRigOccupancyToCapacitySnapshot(snapshot *polecatCapacitySnapshot, townRoot, rigName string, sessions polecatSessionSet) error {
	rigPath := filepath.Join(townRoot, rigName)
	if _, err := os.Stat(rigPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat rig path for %s capacity: %w", rigName, err)
		}
		return nil
	}
	polecatNames, err := listPolecatDirectoryNames(rigPath)
	if err != nil {
		return fmt.Errorf("listing polecat dirs for %s capacity: %w", rigName, err)
	}
	if len(polecatNames) == 0 {
		return nil
	}

	rigBeads := beads.New(rigPath)
	agents, err := rigBeads.ListAgentBeads()
	if err != nil {
		return fmt.Errorf("listing agent beads for %s capacity: %w", rigName, err)
	}
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentBeadID := func(name string) string { return beads.PolecatBeadIDWithPrefix(prefix, rigName, name) }
	activeWork, err := listActivePolecatWorkByName(rigBeads, rigName, polecatHookBeads(polecatNames, agentBeadID, agents))
	if err != nil {
		return fmt.Errorf("listing active polecat work for %s capacity: %w", rigName, err)
	}
	for _, name := range polecatNames {
		issue := agents[agentBeadID(name)]
		fields := parsePolecatAgentFields(issue)
		applyAgentFieldsToCapacitySnapshot(snapshot, townRoot, rigName, name, fields, activeWork[name], sessions)
	}
	return nil
}

// polecatRigOccupancySnapshot counts the slots one rig holds on its own, for a
// rig that carries a max_polecats cap while the town runs without a scheduler
// cap (direct dispatch). It is the same accounting the town snapshot does,
// scoped to one rig so direct mode pays for one rig's worth of scanning.
func polecatRigOccupancySnapshot(townRoot, rigName string) (polecatCapacitySnapshot, error) {
	snapshot := polecatCapacitySnapshot{}
	sessions, err := currentPolecatSessions()
	if err != nil {
		return snapshot, err
	}
	if err := snapshot.addRigOccupancy(townRoot, rigName, sessions); err != nil {
		return snapshot, err
	}
	if err := snapshot.trackReservations(townRoot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func currentPolecatSessions() (polecatSessionSet, error) {
	sessionNames, err := tmux.NewTmux().ListSessions()
	if err != nil {
		return nil, fmt.Errorf("listing tmux sessions for polecat capacity: %w", err)
	}
	return newPolecatSessionSet(session.DefaultRegistry(), sessionNames), nil
}

func listPolecatDirectoryNames(rigPath string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(rigPath, "polecats"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func applyAgentFieldsToCapacitySnapshot(snapshot *polecatCapacitySnapshot, townRoot, rigName, polecatName string, fields *beads.AgentFields, activeWork *beads.Issue, sessions polecatSessionSet) {
	// Near-zero env on purpose: the snapshot reports counts, not per-polecat
	// state, and a polecat inside its spawn grace produces the same
	// disposition as a stalled one (recovery-blocked, counts toward capacity).
	// Capacity also has no MR index — it never reports MR state. The park
	// marker is the one fact it does read: a parked slot is not free to take
	// work, and counting it reusable overstated available capacity (gt-q6nrm).
	env := polecatInventoryEnv{Parked: parkedReason(townRoot, rigName, polecatName)}
	item := buildPolecatInventoryItem(rigName, polecatName, fields, activeWork, sessions, env)
	applyWorkstateDispositionToCapacitySnapshot(snapshot, item.State, item.Disposition)
}

func applyWorkstateDispositionToCapacitySnapshot(snapshot *polecatCapacitySnapshot, state polecat.State, disposition polecat.WorkstateDisposition) {
	if disposition.ReuseStatus == "idle-pr-open" {
		snapshot.addPendingMR()
		return
	}
	if disposition.ReuseStatus == polecat.WorkstateReuseStatusParked {
		snapshot.addParked()
		return
	}
	if disposition.Reusable {
		snapshot.addReusableIdle()
		return
	}
	if disposition.NeedsRecovery {
		snapshot.addRecoveryBlocked(disposition.CountsTowardCapacity)
		return
	}
	if state == polecat.StateWorking || disposition.Verdict == polecat.WorkstateVerdictWorking {
		snapshot.addWorking()
		return
	}
	if disposition.CountsTowardCapacity {
		snapshot.addRecoveryBlocked(true)
	}
}

func acquirePolecatAdmissionLock(townRoot string) (*flock.Flock, error) {
	lockDir := filepath.Join(townRoot, ".runtime", "locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating polecat admission lock dir: %w", err)
	}
	lock := flock.New(filepath.Join(lockDir, "polecat-admission.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquiring polecat admission lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("polecat admission is busy; retry shortly")
	}
	return lock, nil
}

func polecatAdmissionDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "polecat-admission")
}

func writePolecatAdmissionReservation(townRoot, rigName, beadID, operation string) (polecatAdmissionReservation, string, error) {
	dir := polecatAdmissionDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return polecatAdmissionReservation{}, "", fmt.Errorf("creating polecat admission dir: %w", err)
	}
	now := time.Now().UTC()
	id := fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano())
	reservation := polecatAdmissionReservation{
		ID:        id,
		PID:       os.Getpid(),
		Rig:       rigName,
		Bead:      beadID,
		Operation: operation,
		CreatedAt: now,
	}
	path := filepath.Join(dir, id+".json")
	tmpPath := path + ".tmp"
	data, err := json.MarshalIndent(reservation, "", "  ")
	if err != nil {
		return polecatAdmissionReservation{}, "", err
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return polecatAdmissionReservation{}, "", fmt.Errorf("writing polecat admission reservation: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return polecatAdmissionReservation{}, "", fmt.Errorf("publishing polecat admission reservation: %w", err)
	}
	return reservation, path, nil
}

func readPolecatAdmissionReservations(townRoot string) ([]polecatAdmissionReservation, error) {
	dir := polecatAdmissionDir(townRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading polecat admission reservations: %w", err)
	}
	reservations := make([]polecatAdmissionReservation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			_ = os.Remove(path)
			continue
		}
		var reservation polecatAdmissionReservation
		if err := json.Unmarshal(data, &reservation); err != nil {
			_ = os.Remove(path)
			continue
		}
		if reservation.ID == "" || reservation.PID <= 0 || reservation.CreatedAt.IsZero() || reservation.ID+".json" != entry.Name() {
			_ = os.Remove(path)
			continue
		}
		reservations = append(reservations, reservation)
	}
	return reservations, nil
}

func cleanupStalePolecatAdmissionReservations(townRoot string, now time.Time) error {
	dir := polecatAdmissionDir(townRoot)
	reservations, err := readPolecatAdmissionReservations(townRoot)
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		if reservation.PID <= 0 {
			continue
		}
		age := now.Sub(reservation.CreatedAt)
		if processAlive(reservation.PID) {
			continue
		}
		if age < polecatAdmissionReservationTTL {
			continue
		}
		_ = os.Remove(filepath.Join(dir, reservation.ID+".json"))
	}
	return nil
}

func cleanupStalePolecatAdmissionReservationsWithLock(townRoot string, now time.Time) error {
	lock, err := acquirePolecatAdmissionLock(townRoot)
	if err != nil {
		if strings.Contains(err.Error(), "admission is busy") {
			return nil
		}
		return err
	}
	defer func() { _ = lock.Unlock() }()
	return cleanupStalePolecatAdmissionReservations(townRoot, now)
}
