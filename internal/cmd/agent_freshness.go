package cmd

// heartbeatLabelKey is the label an agent bead stamps its liveness in, as a Unix
// epoch. It is the only state label carrying a timestamp, which makes it the only
// one a reader can age out.
const heartbeatLabelKey = "heartbeat"
