package dispatch

// SlingHookedMarker begins the line sling.Run prints once it has hooked the
// bead. The daemon's spec ticker counts it in the output of a tick killed
// before its report, so sling.Run prints the line from this constant and the
// ticker counts against it: a reworded line cannot drop out of the count
// unnoticed (gt-zeqqg).
const SlingHookedMarker = "Work attached to "
