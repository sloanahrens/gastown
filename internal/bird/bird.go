// Package bird names the sessions that run claude: a pool of bird names, one
// locked state file recording which names are bound to beads and which
// processes hold them, and Pick, which hands out a name no one else holds.
//
// The state file is the single source of truth for a name in use. It is read
// and written only under an flock on a sibling lock file, and written only by
// rename, so a reader sees the old state or the new one and concurrent writers
// never lose each other's entry. A name Pick hands out is reserved until Bind
// claims it or its TTL runs out, so two launches never start under one name.
// Liveness is the pid plus the kernel's start token for it (see
// internal/procid): a pid the kernel has since handed to a stranger is not the
// agent that recorded it, and a process table that will not answer is an error
// rather than a death, so nothing is pruned on a guess.
package bird
