// Package formula owns gastown's shipped formula files, not their format.
//
// bd is the one formula engine (D6, gt-fd2cu): it parses, resolves and cooks
// every formula, and gastown reads what `bd cook` returns. This package holds
// what gastown itself owns:
//
//   - the formulas embedded in the gt binary (formulas/*.formula.toml) and
//     their sync to <town>/.beads/formulas, checked by content hash
//     (ProvisionFormulas, PlanFormulaSync, SyncFormulas, UpdateFormulas)
//   - the town's one overlay dir, <town>/formula-overlays, and its step
//     override files (OverlayDir, LoadFormulaOverlay)
//
// Gastown's own fields ride in keys bd's strict decode accepts: a step's
// metadata table (a workflow step's target and interactive flag), which
// gt formula run reads from the cooked tree.
package formula
