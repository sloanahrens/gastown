// Package landworker is the daemon's landing worker (ADR 0004, gt-v4ssj.2):
// one Worker per rig, one landing at a time within a rig, rigs in parallel.
//
// Each pass lists the rig's work beads labeled gt:ready-to-land, oldest
// first, and lands them one by one through land.Lander. The worker owns
// everything around a landing that Land does not: which head to land (the
// branch tip on origin at land time, never a pinned pre-rebase head), the
// repair of a landing whose record was left incomplete, the rework comment on
// a rejection, the polecat's intent record, backoff after infrastructure
// failures, and the annotations a human needs when only a human can proceed.
package landworker
