package dispatch

// MergeRejectionNoteMarker is the canonical vocabulary written into source-bead
// notes when a branch-caused merge rejection is recorded. The polecat work
// formula (mol-polecat-work) greps bead notes for this exact marker during its
// resume path, so a redispatched polecat can find the rejection details and
// reuse the surviving branch. Keep the marker and the formula in sync (gt-tc0).
//
// It lives in this leaf, re-exported by refinery (which writes it), because the
// convoy feeders read it too: a bead carrying it is the deacon's to redispatch,
// and convoy cannot import refinery (gt-ghyfx).
const MergeRejectionNoteMarker = "MERGE REJECTION"
