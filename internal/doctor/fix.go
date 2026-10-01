package doctor

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/ui"
)

// Refusals from FixOne (gt-638go.3, deep review G4-09).
var (
	// ErrUnknownCheck means no registered check has the requested name.
	ErrUnknownCheck = errors.New("no check by that name")
	// ErrUnknownResult means the check could not determine a result — a
	// StatusSkipped check. A repair on top of an unverified assumption is what
	// gt-fcxe9.1 forbids, so the fixer refuses.
	ErrUnknownResult = errors.New("check result is UNKNOWN; a fixer may not act on it")
	// ErrNotFixable means the check reports only and has no repair.
	ErrNotFixable = errors.New("check is report-only")
)

// DestructiveFixer is implemented by checks whose repair destroys state or
// ends a running process — it kills a session, removes a repository, purges
// rows — so the repair needs authorization when an agent runs it (gt-638go.3,
// deep review G4-01, G4-02, G4-09).
type DestructiveFixer interface {
	// DestructiveFix reports whether this check's repair is destructive.
	DestructiveFix() bool
}

// IsDestructiveFix reports whether a check's repair is destructive.
func IsDestructiveFix(check Check) bool {
	d, ok := check.(DestructiveFixer)
	return ok && d.DestructiveFix()
}

// Find returns the registered check named name.
func (d *Doctor) Find(name string) (Check, bool) {
	for _, c := range d.checks {
		if c.Name() == name {
			return c, true
		}
	}
	return nil, false
}

// FixOne runs the single named check and repairs it.
//
// It is the fixer half of the doctor split: `gt doctor` only reports, and a
// repair names exactly one check. A repair runs only on a confirmed problem:
//
//   - an unknown name refuses with ErrUnknownCheck,
//   - a check that could not determine a result (StatusSkipped) is UNKNOWN and
//     refuses with ErrUnknownResult — a fixer acting on it repairs an
//     unverified assumption (gt-fcxe9.1),
//   - a report-only check refuses with ErrNotFixable.
//
// authorize, when non-nil, is consulted before a destructive fix runs and may
// refuse it; it is not called for a non-destructive check. w, when non-nil,
// receives the same streaming lines a full run prints.
func (d *Doctor) FixOne(ctx *CheckContext, name string, w io.Writer, authorize func(Check) error) (*CheckResult, error) {
	check, ok := d.Find(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCheck, name)
	}

	if w != nil {
		fmt.Fprintf(w, "  %s  %s...", ui.RenderMuted("○"), check.Name())
	}
	start := time.Now()
	result := finishResult(check, check.Run(ctx))

	switch {
	case result.Status == StatusOK:
		result.Elapsed = time.Since(start)
		renderFixResult(w, result, false)
		return result, nil
	case result.Status == StatusSkipped:
		result.Elapsed = time.Since(start)
		renderFixResult(w, result, false)
		return result, fmt.Errorf("%w: %s", ErrUnknownResult, check.Name())
	case !check.CanFix():
		result.Elapsed = time.Since(start)
		renderFixResult(w, result, false)
		return result, fmt.Errorf("%w: %s", ErrNotFixable, check.Name())
	}

	if IsDestructiveFix(check) && authorize != nil {
		if err := authorize(check); err != nil {
			result.Elapsed = time.Since(start)
			renderFixResult(w, result, false)
			return result, err
		}
	}

	if err := safeFixCheck(check, ctx); err != nil {
		result.Elapsed = time.Since(start)
		if errors.Is(err, ErrSkippedNoStart) {
			// --no-start suppressed the startup this repair needed: report it
			// as a detail so it does not read as a crashed fixer.
			result.Details = append(result.Details, "Skipped: --no-start suppresses startup")
		} else {
			result.Details = append(result.Details, "Fix failed: "+err.Error())
		}
		renderFixResult(w, result, false)
		return result, err
	}

	// Re-run: a fixer returns nil after a no-op as readily as after a repair,
	// so only the check's own verdict says the problem is gone.
	after := finishResult(check, check.Run(ctx))
	after.Elapsed = time.Since(start)
	if after.Status == StatusOK {
		after.Message += " (fixed)"
		after.Fixed = true
	}
	renderFixResult(w, after, after.Fixed)
	return after, nil
}

// finishResult fills the fields a check may leave to the framework.
func finishResult(check Check, result *CheckResult) *CheckResult {
	if result == nil {
		return &CheckResult{Name: check.Name(), Status: StatusSkipped, Message: "check returned no result"}
	}
	if result.Name == "" {
		result.Name = check.Name()
	}
	if cg, ok := check.(categoryGetter); ok && result.Category == "" {
		result.Category = cg.Category()
	}
	return result
}

// statusIcon maps a status to its glyph. StatusSkipped is UNKNOWN, and it must
// never render as a pass (gt-whvu).
func statusIcon(status CheckStatus) string {
	switch status {
	case StatusOK:
		return ui.RenderPassIcon()
	case StatusWarning:
		return ui.RenderWarnIcon()
	case StatusError:
		return ui.RenderFailIcon()
	case StatusSkipped:
		return ui.RenderSkipIcon()
	default:
		return ui.RenderSkipIcon()
	}
}

// renderFixResult overwrites the streaming "checking" line with the result.
func renderFixResult(w io.Writer, result *CheckResult, fixed bool) {
	if w == nil {
		return
	}
	icon := statusIcon(result.Status)
	if fixed {
		icon = ui.RenderFixIcon()
	}
	fmt.Fprintf(w, "\r  %s  %s", icon, result.Name)
	if result.Message != "" {
		fmt.Fprintf(w, "%s", ui.RenderMuted(" "+result.Message))
	}
	fmt.Fprintln(w)
}
