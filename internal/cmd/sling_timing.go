package cmd

import "github.com/steveyegge/gastown/internal/sling"

// slingSteps is the step timer for the sling command in this process; runSling
// sets it and every seam on the dispatch path calls slingSteps.Step(...). Other
// entry points leave it nil, which is a no-op.
//
// The daemon runs the dispatch engine in process and has no per-run command to
// set this, so it hands the engine its own sling.Timer through Options.Steps
// instead, which keeps the timing lines tagged with the convoy being fed.
var slingSteps *sling.Timer
