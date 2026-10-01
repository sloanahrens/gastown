package convoy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
)

// Ref names a convoy.
type Ref struct {
	ID, Title string
}

// CheckAll closes every open convoy whose tracked issues are all resolved and
// returns the convoys it closed (or, with dryRun, would close).
func (t Town) CheckAll(ctx context.Context, dryRun bool) ([]Ref, error) {
	var closed []Ref

	convoys, err := t.ListConvoys("open", false)
	if err != nil {
		return nil, fmt.Errorf("listing convoys: %w", err)
	}

	for _, convoy := range convoys {
		if err := ctx.Err(); err != nil {
			return closed, err
		}
		if err := EnsureKnownStatus(convoy.Status); err != nil {
			t.warnf("skipping convoy %s: invalid lifecycle state: %v", convoy.ID, err)
			continue
		}
		tracked, err := t.TrackedIssues(convoy.ID)
		if err != nil {
			t.warnf("skipping convoy %s: %v", convoy.ID, err)
			continue
		}
		ready, err := t.closeIfComplete(convoy.ID, convoy.Title, tracked, dryRun)
		if err != nil {
			t.warnf("couldn't close convoy %s: %v", convoy.ID, err)
			continue
		}
		if ready {
			closed = append(closed, Ref{ID: convoy.ID, Title: convoy.Title})
		}
	}

	return closed, nil
}

// CheckOne closes one convoy if all its tracked issues are resolved.
func (t Town) CheckOne(convoyID string, dryRun bool) error {
	convoy, err := t.store(t.Root).Show(convoyID)
	if err != nil {
		return fmt.Errorf("convoy '%s' not found", convoyID)
	}

	// Verify it's actually a convoy type
	if !IsConvoyIssue(convoy.Type, convoy.Labels) {
		return fmt.Errorf("'%s' is not a convoy (type: %s)", convoyID, convoy.Type)
	}
	if err := EnsureKnownStatus(convoy.Status); err != nil {
		return fmt.Errorf("convoy '%s' has invalid lifecycle state: %w", convoyID, err)
	}

	// Check if convoy is already closed
	if NormalizeStatus(convoy.Status) == StatusClosed {
		t.printf("%s Convoy %s is already closed\n", style.Dim.Render("○"), convoyID)
		return t.PersistAndNotify(convoyID, convoy.Title)
	}

	tracked, err := t.TrackedIssues(convoyID)
	if err != nil {
		return fmt.Errorf("checking convoy %s: %w", convoyID, err)
	}

	_, err = t.closeIfComplete(convoyID, convoy.Title, tracked, dryRun)
	return err
}

// closeIfComplete checks whether all tracked issues in a convoy are resolved
// and closes the convoy if so. Returns (true, nil) if the convoy was closed or
// would be closed (dry-run), (false, nil) if not ready, or (false, err) on failure.
func (t Town) closeIfComplete(convoyID, title string, tracked []TrackedIssue, dryRun bool) (bool, error) {
	// If no tracked issues were resolved, skip auto-close. A 0/0 result means
	// cross-rig tracking resolution failed — not that all issues are done.
	// Treating 0/0 as "complete" caused false 🚚 Convoy landed notifications. (GH#3xxx)
	if len(tracked) == 0 {
		return false, nil
	}

	allClosed := true
	openCount := 0
	unknownCount := 0
	for _, tr := range tracked {
		switch tr.Status {
		case "closed", "tombstone":
			// counted as complete
		case TrackedStatusUnknown:
			// Cross-rig DB unreachable — can't verify completion. Leave convoy
			// open, treat as Info (not a convoy-level failure). (gt-bs6)
			allClosed = false
			unknownCount++
		default:
			allClosed = false
			openCount++
		}
	}

	if !allClosed {
		switch {
		case unknownCount > 0 && openCount > 0:
			t.printf("%s Convoy %s has %d open, %d unknown (cross-rig unreachable) issue(s) remaining\n",
				style.Dim.Render("○"), convoyID, openCount, unknownCount)
		case unknownCount > 0:
			t.printf("%s Convoy %s has %d tracked issue(s) with unknown status (cross-rig unreachable)\n",
				style.Dim.Render("○"), convoyID, unknownCount)
		default:
			t.printf("%s Convoy %s has %d open issue(s) remaining\n", style.Dim.Render("○"), convoyID, openCount)
		}
		return false, nil
	}

	if dryRun {
		t.printf("%s Would auto-close convoy 🚚 %s: %s\n", style.Warning.Render("⚠"), convoyID, title)
		return true, nil
	}

	if err := t.CloseAndExport(convoyID, "All tracked issues completed"); err != nil {
		return false, fmt.Errorf("closing convoy: %w", err)
	}

	t.printf("%s Auto-closed convoy 🚚 %s: %s\n", style.Bold.Render("✓"), convoyID, title)
	t.NotifyCompletion(convoyID, title)
	return true, nil
}

// PersistJSONL writes the current town Beads state to the JSONL file used by
// bd's fallback import path. Convoy close/check suppresses bd's implicit
// auto-export for normal command hygiene, but close state must survive a later
// Dolt rebuild from .beads/issues.jsonl.
func (t Town) PersistJSONL() error {
	beadsDir := beads.ResolveBeadsDir(t.Root)
	if beadsDir == "" {
		return fmt.Errorf("could not resolve town .beads directory")
	}
	return t.store(t.Root).Export(filepath.Join(beadsDir, "issues.jsonl"))
}

// CloseAndExport closes a convoy in the town with reason and re-exports the
// JSONL.
func (t Town) CloseAndExport(convoyID, reason string) error {
	if err := t.store(t.Root).CloseWithReason(reason, convoyID); err != nil {
		return err
	}
	return t.PersistJSONL()
}

// PersistAndNotify exports the JSONL, then sends the completion notices.
func (t Town) PersistAndNotify(convoyID, title string) error {
	if err := t.PersistJSONL(); err != nil {
		return fmt.Errorf("persisting convoy close to JSONL: %w", err)
	}
	t.NotifyCompletion(convoyID, title)
	return nil
}

func notifyFrom(convoyID string) string {
	return "convoy/" + convoyID
}

func mailArgs(addr, subject, body, convoyID string) []string {
	return []string{"mail", "send", addr, "-s", subject, "-m", body, "--from", notifyFrom(convoyID), "--no-notify"}
}

// gt runs a gt child for a notice from the town root. Mail and nudge delivery
// are still gt commands; they move to library calls with the mail and nudge
// extraction.
func (t Town) gt(args ...string) error {
	return t.gtWithEnv(t.Env, args...)
}

// gtWithEnv is gt with the child environment env (nil inherits the process
// environment).
func (t Town) gtWithEnv(env []string, args ...string) error {
	run := t.gtRun
	if run == nil {
		run = runGT
	}
	return run(t.rootDir(), env, args...)
}

// nudgeEnv is the child environment for a nudge, sent as the convoy.
func (t Town) nudgeEnv(convoyID string) []string {
	base := t.Env
	if base == nil {
		base = os.Environ()
	}
	env := beads.StripEnvKey(base, "GT_ROLE")
	return append(env, "GT_ROLE="+notifyFrom(convoyID))
}

// NotifyClosed tells addr that a convoy was closed by hand.
func (t Town) NotifyClosed(addr, convoyID, title, reason string) {
	subject := fmt.Sprintf("🚚 Convoy closed: %s", title)
	body := fmt.Sprintf("Convoy %s has been closed.\n\nReason: %s", convoyID, reason)

	if err := t.gt(mailArgs(addr, subject, body, convoyID)...); err != nil {
		t.warnf("couldn't send notification: %v", err)
	} else {
		t.printf("  Notified: %s\n", addr)
	}
}

// NotifyCompletion sends notifications to owner, any notify addresses, and mayor/.
func (t Town) NotifyCompletion(convoyID, title string) {
	convoy, err := t.store(t.Root).Show(convoyID)
	if err != nil {
		return
	}

	// ZFC: Use typed accessor instead of parsing description text
	fields := beads.ParseConvoyFields(&beads.Issue{Description: convoy.Description})
	if fields == nil {
		fields = &beads.ConvoyFields{}
	}
	if fields.CompletionNotifiedAt != "" {
		return
	}

	// Compute duration since convoy was created.
	var durationStr string
	if created, err := time.Parse(time.RFC3339, convoy.CreatedAt); err == nil {
		durationStr = FormatWorkerAge(time.Since(created).Round(time.Minute))
	}

	// Count tracked issues (best-effort; 0 on error is fine for display).
	trackedIDs, _ := t.DepListRawIDs(t.Root, convoyID, "down", "tracks")
	issueCount := len(trackedIDs)

	// Build enriched body for mayor notification.
	mayorBody := fmt.Sprintf("Convoy %s has completed. All tracked issues are now closed.", convoyID)
	if issueCount > 0 || durationStr != "" {
		mayorBody += "\n"
		if issueCount > 0 {
			mayorBody += fmt.Sprintf("\nIssues: %d", issueCount)
		}
		if durationStr != "" {
			mayorBody += fmt.Sprintf("\nDuration: %s", durationStr)
		}
	}

	// Track notified addresses to avoid duplicate mayor/ notification.
	notifiedAddrs := make(map[string]bool)

	for _, addr := range fields.NotificationAddresses() {
		notifiedAddrs[addr] = true
		args := mailArgs(addr,
			fmt.Sprintf("🚚 Convoy landed: %s", title),
			fmt.Sprintf("Convoy %s has completed.\n\nAll tracked issues are now closed.", convoyID),
			convoyID)
		if err := t.gt(args...); err != nil {
			t.warnf("could not notify %s: %v", addr, err)
		}
	}

	// Send nudge notifications to nudge watchers.
	for _, addr := range fields.NudgeNotificationAddresses() {
		nudgeMsg := fmt.Sprintf("🚚 Convoy landed: %s — Convoy %s has completed. All tracked issues are now closed.", title, convoyID)
		if err := t.gtWithEnv(t.nudgeEnv(convoyID), "nudge", addr, "-m", nudgeMsg); err != nil {
			t.warnf("could not nudge %s: %v", addr, err)
		}
	}

	// Always notify mayor/ for strategic visibility, unless already notified above.
	if !notifiedAddrs["mayor/"] {
		args := mailArgs("mayor/", fmt.Sprintf("Convoy complete: %s", title), mayorBody, convoyID)
		if err := t.gt(args...); err != nil {
			t.warnf("could not notify mayor/ of convoy completion: %v", err)
		}
	}

	// Push notification to active Mayor session if configured.
	t.notifyMayorSession(convoyID, title)

	fields.CompletionNotifiedAt = time.Now().UTC().Format(time.RFC3339)
	newDesc := beads.SetConvoyFields(&beads.Issue{Description: convoy.Description}, fields)
	err = t.store(t.Root).Update(convoyID, beads.UpdateOptions{Description: &newDesc})
	if err == nil {
		err = t.PersistJSONL()
	}
	if err != nil {
		t.warnf("could not record convoy completion notification state for %s: %v", convoyID, err)
		return
	}
}

// notifyMayorSession pushes a convoy completion notification into the active
// Mayor session via nudge, if convoy.notify_on_complete is enabled.
func (t Town) notifyMayorSession(convoyID, title string) {
	settingsPath := config.TownSettingsPath(t.Root)
	settings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return
	}
	if settings.Convoy == nil || !settings.Convoy.NotifyOnComplete {
		return
	}

	nudgeMsg := fmt.Sprintf("🚚 Convoy landed: %s — Convoy %s has completed. All tracked issues are now closed.", title, convoyID)
	if err := t.gtWithEnv(t.nudgeEnv(convoyID), "nudge", "mayor", "-m", nudgeMsg); err != nil {
		t.warnf("could not nudge Mayor session: %v", err)
	}
}
