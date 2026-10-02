package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/workspace"
)

// renderRoleTemplate renders the static role template for ctx. It returns ""
// (no error) when templates are unavailable or the role is unknown, in which
// case callers fall back to the hardcoded context.
func renderRoleTemplate(ctx RoleContext) (string, error) {
	tmpl, err := templates.New()
	if err != nil {
		return "", nil
	}

	// Map role to template name
	var roleName string
	switch ctx.Role {
	case RoleMayor:
		roleName = constants.RoleMayor
	case RolePolecat:
		roleName = constants.RolePolecat
	case RoleCrew:
		roleName = constants.RoleCrew
	default:
		// Unknown role - caller uses fallback
		return "", nil
	}

	// Build template data
	// Get town name for session names
	townName, _ := workspace.GetTownName(ctx.TownRoot)

	defaultBranch, isForkRig, upstreamURL := roleRigContext(ctx)

	data := templates.RoleData{
		Role:          roleName,
		RigName:       ctx.Rig,
		TownRoot:      ctx.TownRoot,
		TownName:      townName,
		WorkDir:       ctx.WorkDir,
		DefaultBranch: defaultBranch,
		IsForkRig:     isForkRig,
		UpstreamURL:   upstreamURL,
		Polecat:       ctx.Polecat,
		MayorSession:  session.MayorSessionName(),
	}

	output, err := tmpl.RenderRole(roleName, data)
	if err != nil {
		return "", fmt.Errorf("rendering template: %w", err)
	}
	return output, nil
}

func roleRigContext(ctx RoleContext) (defaultBranch string, isForkRig bool, upstreamURL string) {
	defaultBranch = "main"
	if ctx.Rig == "" || ctx.TownRoot == "" {
		return defaultBranch, false, ""
	}
	rigPath := filepath.Join(ctx.TownRoot, ctx.Rig)
	rigCfg, err := rig.LoadRigConfig(rigPath)
	if err != nil || rigCfg == nil {
		return defaultBranch, false, ""
	}
	if rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	if strings.TrimSpace(rigCfg.UpstreamURL) != "" {
		return defaultBranch, true, util.RedactURL(rigCfg.UpstreamURL)
	}
	return defaultBranch, false, ""
}

// outputRoleDirectives loads and emits operator-provided role directives.
// These come from the directive file layout (town-level and/or rig-level)
// and override formula defaults where they conflict.
//
// w and explainEnabled are injected so tests can capture output without
// mutating os.Stdout or the primeExplain global (avoiding data races
// under t.Parallel).
func outputRoleDirectives(ctx RoleContext, w io.Writer, explainEnabled bool) {
	outputRoleDirectivesCapped(ctx, w, explainEnabled, primeHookMode)
}

// outputRoleDirectivesCapped is outputRoleDirectives with the hook-mode cap
// on a long directive applied when capped is set.
func outputRoleDirectivesCapped(ctx RoleContext, w io.Writer, explainEnabled, capped bool) {
	role := string(ctx.Role)
	townRoot := ctx.TownRoot
	rigName := ctx.Rig

	townPath := filepath.Join(townRoot, "directives", role+".md")
	rigPath := ""
	if rigName != "" {
		rigPath = filepath.Join(townRoot, rigName, "directives", role+".md")
	}

	explainf := func(format string, args ...any) {
		if explainEnabled {
			fmt.Fprintf(w, "\n[EXPLAIN] "+format+"\n", args...)
		}
	}

	// A misnamed file is dead for every role, so this runs before the
	// content check below rather than inside it: the roles most likely to act
	// on the warning (mayor, polecat) are exactly the ones that have a
	// directive of their own and would never reach the early return.
	outputUnusedDirectiveWarning(w, townRoot, rigName)

	content := config.LoadRoleDirective(role, townRoot, rigName)
	if content == "" {
		explainf("Role directives: no directive files found (checked %s", townPath)
		if rigPath != "" {
			explainf("Role directives: also checked %s", rigPath)
		}
		return
	}

	// Determine source label for the header
	hasTown := false
	hasRig := false
	if data, err := os.ReadFile(townPath); err == nil { //nolint:gosec // G304: path is from trusted config
		if s := strings.TrimSpace(string(data)); s != "" {
			hasTown = true
		}
	}
	if rigPath != "" {
		if data, err := os.ReadFile(rigPath); err == nil { //nolint:gosec // G304: path is from trusted config
			if s := strings.TrimSpace(string(data)); s != "" {
				hasRig = true
			}
		}
	}

	explainf("Role directives: town=%v rig=%v (town=%s, rig=%s)", hasTown, hasRig, townPath, rigPath)

	fmt.Fprintln(w)
	if hasTown && hasRig {
		fmt.Fprintln(w, "## Town & Rig Directives (operator policy — overrides formula where they conflict)")
	} else if hasRig {
		fmt.Fprintln(w, "## Rig Directives (operator policy — overrides formula where they conflict)")
	} else {
		fmt.Fprintln(w, "## Town Directives (operator policy — overrides formula where they conflict)")
	}
	fmt.Fprintln(w)
	// The cap protects the hook budget only; a plain `gt prime` shows it all.
	if capped && len(content) > primeDirectiveMaxChars {
		cut := strings.LastIndexByte(content[:primeDirectiveMaxChars], '\n')
		if cut <= 0 {
			cut = primeDirectiveMaxChars
		}
		fmt.Fprintln(w, content[:cut])
		fmt.Fprintf(w, "\n_[prime] directive truncated at %d of %d chars; read the file(s) above in full: %s", cut, len(content), townPath)
		if rigPath != "" {
			fmt.Fprintf(w, ", %s", rigPath)
		}
		fmt.Fprintln(w, "_")
		return
	}
	fmt.Fprintln(w, content)
}

// outputUnusedDirectiveWarning lists the directive files that no role loads.
// A scan failure prints nothing: prime is not the place to report a broken
// directives directory, and gt doctor already carries that result.
func outputUnusedDirectiveWarning(w io.Writer, townRoot, rigName string) {
	files, err := config.ScanDirectiveFiles(townRoot, rigName)
	if err != nil {
		return
	}

	var unused []config.DirectiveFile
	for _, f := range files {
		if f.Unused() {
			unused = append(unused, f)
		}
	}
	if len(unused) == 0 {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, style.Warning.Render("## Unused Directive Files (no agent role loads these)"))
	fmt.Fprintln(w)
	for _, f := range unused {
		fmt.Fprintf(w, "- `%s` — no role named %q\n", f.Path, f.Role)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Rename each file to a role name, or move shared policy into `%s.md`, which every role loads (see `%s directive list`).\n",
		config.SharedDirectiveName, cli.Name())
}

func outputPrimeContextFallback(w io.Writer, ctx RoleContext) {
	switch ctx.Role {
	case RoleMayor:
		outputMayorContext(w, ctx)
	case RolePolecat:
		outputPolecatContext(w, ctx)
	case RoleCrew:
		outputCrewContext(w, ctx)
	default:
		outputUnknownContext(w, ctx)
	}
}

func outputMayorContext(w io.Writer, ctx RoleContext) {
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("# Mayor Context"))
	fmt.Fprintln(w, "You are the **Mayor** - the global coordinator of Gas Town.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Responsibilities")
	fmt.Fprintln(w, "- Coordinate work across all rigs")
	fmt.Fprintln(w, "- Delegate to Refineries, not directly to polecats")
	fmt.Fprintln(w, "- Monitor overall system health")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Key Commands")
	fmt.Fprintln(w, "- `"+cli.Name()+" mail inbox` - Check your messages")
	fmt.Fprintln(w, "- `"+cli.Name()+" mail read <id>` - Read a specific message")
	fmt.Fprintln(w, "- `"+cli.Name()+" status` - Show overall town status")
	fmt.Fprintln(w, "- `"+cli.Name()+" rig list` - List all rigs")
	fmt.Fprintln(w, "- `bd ready` - Issues ready to work")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Hookable Mail")
	fmt.Fprintln(w, "Mail can be hooked for ad-hoc instructions: `"+cli.Name()+" hook attach <mail-id>`")
	fmt.Fprintln(w, "If mail is on your hook, read and execute its instructions (GUPP applies).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Lifecycle Nudges (SLOT_OPEN)")
	fmt.Fprintln(w, "When you receive a SLOT_OPEN nudge from the Witness, a polecat has completed")
	fmt.Fprintln(w, "work and its slot is available. **Always verify via CLI before deciding action:**")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "1. Run `"+cli.Name()+" polecat list` to get ground truth on polecat state")
	fmt.Fprintln(w, "2. Do NOT trust your in-context belief about polecat state — it may be stale")
	fmt.Fprintln(w, "3. If slots are open and beads are queued: `"+cli.Name()+" sling <bead> <rig>`")
	fmt.Fprintln(w, "4. Witness lifecycle events are authoritative — never second-guess them")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Startup")
	fmt.Fprintln(w, "Check for handoff messages with 🤝 HANDOFF in subject - continue predecessor's work.")
	fmt.Fprintln(w)
	outputCommandQuickReference(os.Stdout, ctx)
	fmt.Fprintf(w, "Town root: %s\n", style.Dim.Render(ctx.TownRoot))
}

func outputPolecatContext(w io.Writer, ctx RoleContext) {
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("# Polecat Context"))
	fmt.Fprintf(w, "You are polecat **%s** in rig: %s\n\n",
		style.Bold.Render(ctx.Polecat), style.Bold.Render(ctx.Rig))
	fmt.Fprintln(w, "## Startup Protocol")
	fmt.Fprintln(w, "1. Run `"+cli.Name()+" prime` - loads context and checks mail automatically")
	fmt.Fprintln(w, "2. Check inbox - if mail shown, read with `"+cli.Name()+" mail read <id>`")
	fmt.Fprintln(w, "3. Look for '📋 Work Assignment' messages for your task")
	fmt.Fprintln(w, "4. If no mail, check `bd list --status=in_progress` for existing work")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Key Commands")
	fmt.Fprintln(w, "- `"+cli.Name()+" mail inbox` - Check your inbox for work assignments")
	fmt.Fprintln(w, "- `bd show <issue>` - View your assigned issue")
	if _, isForkRig, _ := roleRigContext(ctx); isForkRig {
		fmt.Fprintln(w, "- Fork rig: push to origin and use PR/no-merge workflow; do not submit upstream changes to MQ")
	} else {
		fmt.Fprintln(w, "- `"+cli.Name()+" done` - Signal work ready for merge")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Hookable Mail")
	fmt.Fprintln(w, "Mail can be hooked for ad-hoc instructions: `"+cli.Name()+" hook attach <mail-id>`")
	fmt.Fprintln(w, "If mail is on your hook, read and execute its instructions (GUPP applies).")
	fmt.Fprintln(w)
	outputCommandQuickReference(os.Stdout, ctx)
	fmt.Fprintf(w, "Polecat: %s | Rig: %s\n",
		style.Dim.Render(ctx.Polecat), style.Dim.Render(ctx.Rig))
}

func outputCrewContext(w io.Writer, ctx RoleContext) {
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("# Crew Worker Context"))
	fmt.Fprintf(w, "You are crew worker **%s** in rig: %s\n\n",
		style.Bold.Render(ctx.Polecat), style.Bold.Render(ctx.Rig))
	fmt.Fprintln(w, "## About Crew Workers")
	fmt.Fprintln(w, "- Persistent workspace (not auto-garbage-collected)")
	fmt.Fprintln(w, "- User-managed (not Witness-monitored)")
	fmt.Fprintln(w, "- Long-lived identity across sessions")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "**Identity**: You are the AI agent. The human sending you messages is the")
	fmt.Fprintln(w, "**Overseer** — the only non-agent role in Gas Town. Do not confuse your identity with theirs.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Key Commands")
	fmt.Fprintln(w, "- `"+cli.Name()+" mail inbox` - Check your inbox")
	fmt.Fprintln(w, "- `bd ready` - Available issues")
	fmt.Fprintln(w, "- `bd show <issue>` - View issue details")
	fmt.Fprintln(w, "- `"+cli.Name()+" done --bead <issue>` - Submit your pushed branch; landing closes the issue")
	if _, isForkRig, _ := roleRigContext(ctx); isForkRig {
		fmt.Fprintln(w, "- Fork rig: branch from upstream, push to origin, create PR against upstream")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Hookable Mail")
	fmt.Fprintln(w, "Mail can be hooked for ad-hoc instructions: `"+cli.Name()+" hook attach <mail-id>`")
	fmt.Fprintln(w, "If mail is on your hook, read and execute its instructions (GUPP applies).")
	fmt.Fprintln(w)
	outputCommandQuickReference(os.Stdout, ctx)
	fmt.Fprintf(w, "Crew: %s | Rig: %s\n",
		style.Dim.Render(ctx.Polecat), style.Dim.Render(ctx.Rig))
}

func outputUnknownContext(w io.Writer, ctx RoleContext) {
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("# Gas Town Context"))
	fmt.Fprintln(w, "Could not determine specific role from current directory.")
	fmt.Fprintln(w)
	if ctx.Rig != "" {
		fmt.Fprintf(w, "You appear to be in rig: %s\n\n", style.Bold.Render(ctx.Rig))
	}
	fmt.Fprintln(w, "Navigate to a specific agent directory:")
	fmt.Fprintln(w, "- `<rig>/polecats/<name>/` - Polecat role")
	fmt.Fprintln(w, "- `mayor/` or `<rig>/mayor/` - Mayor role")
	fmt.Fprintln(w, "- Town root is neutral (set GT_ROLE or cd into a role directory)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Town root: %s\n", style.Dim.Render(ctx.TownRoot))
}

// outputCommandQuickReference outputs a compact role-aware cheatsheet of commonly
// confused commands. This helps agents avoid guessing wrong commands.
func outputCommandQuickReference(w io.Writer, ctx RoleContext) {
	c := cli.Name()
	fmt.Fprintln(w, "## ⚡ Command Quick-Reference")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "**Commonly confused — use the right command:**")
	fmt.Fprintln(w)

	switch ctx.Role {
	case RoleMayor:
		fmt.Fprintln(w, "| Want to... | Correct command | Common mistake |")
		fmt.Fprintln(w, "|------------|----------------|----------------|")
		fmt.Fprintln(w, "| Close/complete a bead | `gt bead close <id>` | ~~bd complete~~ (not a command), ~~bd update --status done~~ (invalid status) |")
		fmt.Fprintf(w, "| Dispatch work to polecat | `%s sling <bead> <rig>` | ~~gt polecat spawn~~ (not a command) |\n", c)
		fmt.Fprintf(w, "| Message another agent | `%s nudge <target> \"msg\"` | ~~tmux send-keys~~ (unreliable) |\n", c)
		fmt.Fprintf(w, "| Kill stuck polecat | `%s polecat nuke <rig>/<name> --force` | ~~gt polecat kill~~ (not a command) |\n", c)
		fmt.Fprintf(w, "| Pause rig (daemon won't restart) | `%s rig park <rig>` | ~~gt rig stop~~ (daemon will restart it) |\n", c)
		fmt.Fprintf(w, "| Permanently disable rig | `%s rig dock <rig>` | ~~gt rig park~~ (temporary only) |\n", c)
		fmt.Fprintln(w, "| Create issues | `gt bead create \"title\"` | ~~gt issue create~~ (not a command) |")

	case RoleCrew:
		fmt.Fprintln(w, "| Want to... | Correct command | Common mistake |")
		fmt.Fprintln(w, "|------------|----------------|----------------|")
		fmt.Fprintln(w, "| Close/complete a bead | `gt bead close <id>` | ~~bd complete~~ (not a command), ~~bd update --status done~~ (invalid status) |")
		fmt.Fprintf(w, "| Message another agent | `%s nudge <target> \"msg\"` | ~~tmux send-keys~~ (unreliable) |\n", c)
		fmt.Fprintf(w, "| Dispatch work to polecat | `%s sling <bead> <rig>` | ~~gt polecat spawn~~ (not a command) |\n", c)
		fmt.Fprintf(w, "| Stop my session | `%s crew stop %s` | ~~gt rig stop~~ (stops rig agents, not crew) |\n", c, ctx.Polecat)
		fmt.Fprintf(w, "| Pause rig (daemon won't restart) | `%s rig park <rig>` | ~~gt rig stop~~ (daemon will restart it) |\n", c)
		fmt.Fprintf(w, "| Permanently disable rig | `%s rig dock <rig>` | ~~gt rig park~~ (temporary only) |\n", c)

	case RolePolecat:
		fmt.Fprintln(w, "| Want to... | Correct command | Common mistake |")
		fmt.Fprintln(w, "|------------|----------------|----------------|")
		fmt.Fprintf(w, "| Signal work complete | `%s done` | ~~gt bead close <root-issue>~~ (Refinery closes it) |\n", c)
		fmt.Fprintln(w, "| Close a sub-issue | `gt bead close <id>` | ~~bd complete~~ (not a command), ~~bd update --status done~~ (invalid status) |")
		fmt.Fprintf(w, "| Message another agent | `%s nudge <target> \"msg\"` | ~~tmux send-keys~~ (unreliable) |\n", c)
		fmt.Fprintf(w, "| Check workflow steps | `%s prime` (shows inline checklist) | ~~bd ready~~ (excludes molecule steps) |\n", c)
		fmt.Fprintln(w, "| Create issues | `gt bead create \"title\"` | ~~gt issue create~~ (not a command) |")
		fmt.Fprintf(w, "| Escalate blocker | `%s escalate \"desc\" -s HIGH` | ~~waiting for human~~ (never wait) |\n", c)

	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "**Rig lifecycle commands (park vs dock vs stop):**")
	fmt.Fprintln(w, "- `park/unpark` — Temporary pause. Daemon skips parked rigs.")
	fmt.Fprintln(w, "- `dock/undock` — Persistent disable. Survives daemon restarts.")
	fmt.Fprintln(w, "- `stop/shutdown` — Immediate stop of the rig's polecat sessions.")
	fmt.Fprintln(w)
}

// outputHandoffContent reads and displays the pinned handoff bead for the role.
func outputHandoffContent(w io.Writer, ctx RoleContext) {
	if ctx.Role == RoleUnknown {
		return
	}

	// Get role key for handoff bead lookup
	roleKey := string(ctx.Role)

	bd := beads.New(ctx.TownRoot)
	issue, err := bd.FindHandoffBead(roleKey)
	if err != nil {
		// Silently skip if beads lookup fails (might not be a beads repo)
		return
	}
	if issue == nil || issue.Description == "" {
		// No handoff content
		return
	}

	// Display handoff content
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("## 🤝 Handoff from Previous Session"))
	fmt.Fprintln(w, issue.Description)
	fmt.Fprintln(w)
	fmt.Fprintln(w, style.Dim.Render("(Clear with: gt rig reset --handoff)"))
}

// outputStartupDirective outputs role-specific instructions for the agent.
// This tells agents like Mayor to announce themselves on startup.
func outputStartupDirective(w io.Writer, ctx RoleContext) {
	switch ctx.Role {
	case RoleMayor:
		fmt.Fprintln(w)
		fmt.Fprintln(w, "---")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "**STARTUP PROTOCOL**: You are the Mayor. Please:")
		fmt.Fprintln(w, "1. Run `"+cli.Name()+" prime` (loads full context, mail, and pending work)")
		fmt.Fprintln(w, "2. Announce: \"Mayor, checking in.\"")
		fmt.Fprintln(w, "3. Check mail: `"+cli.Name()+" mail inbox` - look for 🤝 HANDOFF messages")
		fmt.Fprintln(w, "4. Check for attached work: `"+cli.Name()+" hook`")
		fmt.Fprintln(w, "   - If mol attached → **RUN IT** (no human input needed)")
		fmt.Fprintln(w, "   - If no mol → await user instruction")
	case RolePolecat:
		fmt.Fprintln(w)
		fmt.Fprintln(w, "---")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "**STARTUP PROTOCOL**: You are a polecat with NO WORK on your hook.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "1. Run `"+cli.Name()+" prime` (loads full context, mail, and pending work)")
		fmt.Fprintln(w, "2. Check if any mail was injected above in this output")
		fmt.Fprintln(w, "3. If you have mail with work instructions → execute that work")
		fmt.Fprintln(w, "4. If NO mail → run `"+cli.Name()+" done` IMMEDIATELY")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Polecat sessions are ephemeral. No work on hook + no mail = terminate.")
		fmt.Fprintln(w, "DO NOT wait. DO NOT escalate. DO NOT send idle alerts.")
		fmt.Fprintln(w, "Just run `"+cli.Name()+" done` and exit.")
	case RoleCrew:
		fmt.Fprintln(w)
		fmt.Fprintln(w, "---")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "**STARTUP PROTOCOL**: You are a crew worker. Please:")
		fmt.Fprintln(w, "1. Run `"+cli.Name()+" prime` (loads full context, mail, and pending work)")
		fmt.Fprintf(w, "2. Announce: \"%s Crew %s, checking in.\"\n", ctx.Rig, ctx.Polecat)
		fmt.Fprintln(w, "3. Check mail: `"+cli.Name()+" mail inbox`")
		fmt.Fprintln(w, "4. If there's a 🤝 HANDOFF message, read it and continue the work")
		fmt.Fprintln(w, "5. Check for attached work: `"+cli.Name()+" hook`")
		fmt.Fprintln(w, "   - If attachment found → **RUN IT** (no human input needed)")
		fmt.Fprintln(w, "   - If no attachment → **STOP and wait for input**. Do NOT run")
		fmt.Fprintln(w, "     any more commands. Do NOT poll mail. Do NOT check status.")
		fmt.Fprintln(w, "     Sit idle at your prompt — a nudge or user message will arrive.")
	}
}

// outputAttachmentStatus checks for attached work molecule and outputs status.
// This is key for the autonomous overnight work pattern.
// The Propulsion Principle: "If you find something on your hook, YOU RUN IT."
func outputAttachmentStatus(w io.Writer, ctx RoleContext) {
	// Skip only unknown roles - all valid roles can have pinned work
	if ctx.Role == RoleUnknown {
		return
	}

	// Check for pinned beads with attachments
	b := beads.New(ctx.WorkDir)

	// Build assignee string based on role (same as getAgentIdentity)
	assignee := getAgentIdentity(ctx)
	if assignee == "" {
		return
	}

	// Find pinned beads for this agent
	pinnedBeads, err := b.List(beads.ListOptions{
		Status:   beads.StatusPinned,
		Assignee: assignee,
		Priority: -1,
	})
	if err != nil || len(pinnedBeads) == 0 {
		// No pinned beads - interactive mode
		return
	}

	// Check first pinned bead for attachment
	attachment := beads.ParseAttachmentFields(pinnedBeads[0])
	if !hasWorkflowAttachment(attachment) {
		// No attachment - interactive mode
		return
	}

	// Has attached work - output prominently with current step
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("## 🎯 ATTACHED WORK DETECTED"))
	fmt.Fprintf(w, "Pinned bead: %s\n", pinnedBeads[0].ID)
	if attachment.AttachedFormula != "" {
		fmt.Fprintf(w, "Attached formula: %s\n", attachment.AttachedFormula)
	}
	if attachment.AttachedMolecule != "" {
		fmt.Fprintf(w, "Attached molecule: %s\n", attachment.AttachedMolecule)
	}
	if attachment.AttachedAt != "" {
		fmt.Fprintf(w, "Attached at: %s\n", attachment.AttachedAt)
	}
	if len(attachment.AttachedVars) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s\n", style.Bold.Render("🧩 VARS (instantiated formula inputs):"))
		for _, variable := range attachment.AttachedVars {
			fmt.Fprintf(w, "  --var %s\n", variable)
		}
	}
	if attachment.AttachedArgs != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s\n", style.Bold.Render("📋 ARGS (use these to guide execution):"))
		fmt.Fprintf(w, "  %s\n", attachment.AttachedArgs)
	}
	fmt.Fprintln(w)

	ctx.showChecklist(w, attachment)
}

// outputContinuationDirective displays a brief continuation prompt for post-compact/resume.
// Unlike outputAutonomousDirective, this does NOT ask the agent to re-announce or
// re-run startup protocol — it just reminds the agent what's on the hook. (GH#1965)
func outputContinuationDirective(w io.Writer, hookedBead *beads.Issue, hasMolecule bool) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("## ▶ CONTINUE HOOKED WORK"))
	fmt.Fprintln(w, "Your context was compacted/resumed. **Continue working on your hooked bead.**")
	fmt.Fprintln(w, "Do NOT re-announce, re-initialize, or re-read the bead from scratch.")
	fmt.Fprintln(w, "Pick up where you left off.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Hooked: %s — %s\n", style.Bold.Render(hookedBead.ID), hookedBead.Title)
	if hasMolecule {
		fmt.Fprintln(w, "  (Has attached molecule — check `"+cli.Name()+" mol current` for next step)")
	}
	fmt.Fprintln(w)
}

// outputHandoffWarning outputs the post-handoff warning message.
func outputHandoffWarning(w io.Writer, prevSession string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, style.Bold.Render("╔══════════════════════════════════════════════════════════════════╗"))
	fmt.Fprintln(w, style.Bold.Render("║  ✅ HANDOFF COMPLETE - You are the NEW session                   ║"))
	fmt.Fprintln(w, style.Bold.Render("╚══════════════════════════════════════════════════════════════════╝"))
	fmt.Fprintln(w)
	if prevSession != "" {
		fmt.Fprintf(w, "Your predecessor (%s) handed off to you.\n", prevSession)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, style.Bold.Render("⚠️  DO NOT run /handoff - that was your predecessor's action."))
	fmt.Fprintln(w, "   The /handoff you see in context is NOT a request for you.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Instead: Check your hook (`"+cli.Name()+" mol status`) and mail (`"+cli.Name()+" mail inbox`).")
	fmt.Fprintln(w)
}

// outputState outputs only the session state (for --state flag).
// If jsonOutput is true, outputs JSON format instead of key:value.
func outputState(w io.Writer, ctx RoleContext, jsonOutput bool) {
	state := detectSessionState(ctx)

	if jsonOutput {
		data, err := json.Marshal(state)
		if err != nil {
			// Fall back to plain text on error
			fmt.Fprintf(w, "state: %s\n", state.State)
			fmt.Fprintf(w, "role: %s\n", state.Role)
			return
		}
		fmt.Fprintln(w, string(data))
		return
	}

	fmt.Fprintf(w, "state: %s\n", state.State)
	fmt.Fprintf(w, "role: %s\n", state.Role)

	switch state.State {
	case "post-handoff":
		if state.PrevSession != "" {
			fmt.Fprintf(w, "prev_session: %s\n", state.PrevSession)
		}
	case "crash-recovery":
		if state.CheckpointAge != "" {
			fmt.Fprintf(w, "checkpoint_age: %s\n", state.CheckpointAge)
		}
	case "autonomous":
		if state.HookedBead != "" {
			fmt.Fprintf(w, "hooked_bead: %s\n", state.HookedBead)
		}
	}
}

// outputCheckpointContext reads and displays any previous session checkpoint.
// This enables crash recovery by showing what the previous session was working on.
func outputCheckpointContext(w io.Writer, ctx RoleContext) {
	// Only applies to polecats and crew workers
	if ctx.Role != RolePolecat && ctx.Role != RoleCrew {
		return
	}

	// Read checkpoint
	cp, err := checkpoint.Read(ctx.WorkDir)
	if err != nil {
		// Silently ignore read errors
		return
	}
	if cp == nil {
		// No checkpoint exists
		return
	}

	// Check if checkpoint is stale (older than 24 hours)
	if cp.IsStale(24 * time.Hour) {
		// Remove stale checkpoint
		_ = checkpoint.Remove(ctx.WorkDir)
		return
	}

	// Display checkpoint context
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("## 📌 Previous Session Checkpoint"))
	fmt.Fprintf(w, "A previous session left a checkpoint %s ago.\n\n", cp.Age().Round(time.Minute))

	if cp.StepTitle != "" {
		fmt.Fprintf(w, "  **Working on:** %s\n", cp.StepTitle)
	}
	if cp.MoleculeID != "" {
		fmt.Fprintf(w, "  **Molecule:** %s\n", cp.MoleculeID)
	}
	if cp.CurrentStep != "" {
		fmt.Fprintf(w, "  **Step:** %s\n", cp.CurrentStep)
	}
	if cp.HookedBead != "" {
		fmt.Fprintf(w, "  **Hooked bead:** %s\n", cp.HookedBead)
	}
	if cp.Branch != "" {
		fmt.Fprintf(w, "  **Branch:** %s\n", cp.Branch)
	}
	if len(cp.ModifiedFiles) > 0 {
		fmt.Fprintf(w, "  **Modified files:** %d\n", len(cp.ModifiedFiles))
		// Show first few files
		maxShow := 5
		if len(cp.ModifiedFiles) < maxShow {
			maxShow = len(cp.ModifiedFiles)
		}
		for i := 0; i < maxShow; i++ {
			fmt.Fprintf(w, "    - %s\n", cp.ModifiedFiles[i])
		}
		if len(cp.ModifiedFiles) > maxShow {
			fmt.Fprintf(w, "    ... and %d more\n", len(cp.ModifiedFiles)-maxShow)
		}
	}
	if cp.Notes != "" {
		fmt.Fprintf(w, "  **Notes:** %s\n", cp.Notes)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Use this context to resume work. The checkpoint will be updated as you progress.")
	fmt.Fprintln(w)
}

// explain outputs an explanatory message if --explain mode is enabled.
func explain(condition bool, reason string) {
	explainTo(os.Stdout, primeExplain, condition, reason)
}

// explainTo writes reason to w as an [EXPLAIN] line when enabled and
// condition both hold.
func explainTo(w io.Writer, enabled, condition bool, reason string) {
	if enabled && condition {
		fmt.Fprintf(w, "\n[EXPLAIN] %s\n", reason)
	}
}
