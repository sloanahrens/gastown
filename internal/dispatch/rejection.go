package dispatch

// MergeRejectionNoteMarker is the canonical vocabulary written into source-bead
// notes when a branch-caused merge rejection is recorded. The polecat work
// formula (mol-polecat-work) greps bead notes for this exact marker during its
// resume path, so a redispatched polecat can find the rejection details and
// reuse the surviving branch. Keep the marker and the formula in sync (gt-tc0).
//
// It lives in this leaf so the writer and every reader that cannot import each
// other freely reach the same marker (gt-ghyfx).
const MergeRejectionNoteMarker = "MERGE REJECTION"
