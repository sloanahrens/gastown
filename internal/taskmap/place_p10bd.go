package taskmap

// Placements for batch P10, half B, part 4 (gt-sdadsq): four tap commands. Three
// are hook handlers that Claude Code calls (tap guard script-files, tap
// nudge-drain and tap polecat-stop-check), so they go in internal, the same as
// the other tap handlers (place_p10bb.go).
//
// Group judgement: tap list is the one exception to the hook rule. It lists the
// handlers rather than running as a hook, but it sits in internal beside the tap
// parent, which is also internal, because it describes gt's own hook plumbing.
func init() {
	Register(
		Entry{Path: "tap guard script-files", Group: "internal", Use: "Use this when an agent tries to create a new shell, Perl or Ruby script inside its own worktree, so the write is refused and the work is redirected to Go with a unit test."},
		Entry{Path: "tap list", Group: "internal", Use: "Use this to see which tap handlers exist, such as the guards and audits that Claude Code hooks call, before adding one or working out which handler refused a tool call."},
		Entry{Path: "tap nudge-drain", Group: "internal", Use: "Use this when a nudge queued for a working polecat, such as an overrun split prompt, must reach the model after its tool call instead of waiting for a prompt that never comes and expiring."},
		Entry{Path: "tap polecat-stop-check", Group: "internal", Use: "Use this when a polecat's turn ends with committed work that was never submitted and nothing shows its checks are still running, so the Stop hook submits the work with gt done instead of leaving it idle."},
	)
}
