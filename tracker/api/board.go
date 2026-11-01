package api

import "slices"

// The rules the ready and blocked boards answer by. The Postgres ready query
// builds its SQL from them, and the bd shim, whose store seam carries no
// blocked query, applies the same rules to the issues it listed. One
// definition, so the two cannot disagree about what is held or what is ready.

// BlockingLinkTypes are the dependency types that hold their issue back.
var BlockingLinkTypes = []string{"blocks", "conditional-blocks"}

// ResolvedStatuses are the statuses a link's target may hold without holding
// the issue back.
var ResolvedStatuses = []string{"closed", "pinned"}

// ReadyExcludedTypes are the issue types the whole-database ready board leaves
// out, as bd's does: the infrastructure types, and the kinds of bead that are
// not work to pick up.
var ReadyExcludedTypes = append(slices.Clone(InfraTypes), "merge-request", "gate", "molecule", "rig")
