// Package land lands one piece of work on its target branch (ADR 0004, D2).
//
// The author side (`gt done`) rebases, gates locally, pushes its branch and
// marks the work bead ready to land. A landing worker then calls
// Lander.Land, which merges that branch into a throwaway worktree of the
// target, gates the merged tree while om reviews the same range, pushes with
// a lease, reads the tip back, and records the landing on the work bead and in
// the rig's append-only landings file. A rejection is written to the work bead
// and hands it back for rework.
//
// This package must not import internal/refinery: the refinery is deleted at
// cutover (gt-v4ssj.6), and the helpers both need live here.
package land
