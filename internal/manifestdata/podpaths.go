package manifestdata

// The daemon pod's paths on the Mac host. The pod town root moves at the rename
// (gt-ipp4uy), so its one spelling is here: TestNoLiteralPodTownRootOutsideTheConstant
// fails any other non-test Go file that spells it.

const (
	// PodTownRoot is the town root inside the daemon pod: the path the Deployment
	// sets as GT_TOWN_ROOT and mounts the town-state claim at.
	PodTownRoot = "/Users/sloan/gt"
	// PodLandingWorkPath is the landing work claim's mount in the pod.
	PodLandingWorkPath = "/Users/sloan/gt-landing.noindex"
	// PodDoltBackupPath is where the compose file mounts the host's Dolt backups,
	// and the pod mounts the same path (GT_DOLT_BACKUP_ROOT names it).
	PodDoltBackupPath = "/Users/sloan/gt-backups/dolt"
	// PodClaudeBeadsRoot is the host's ~/.claude/.beads, which the compose daemon
	// binds read-write. The pod mounts nothing there.
	PodClaudeBeadsRoot = "/Users/sloan/.claude/.beads"
)
