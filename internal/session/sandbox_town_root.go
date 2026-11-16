package session

import "path"

// SandboxTownRootPath is the town root inside a sandbox: the skeleton every
// sandbox seat's clone lives under. A mutating git call whose repository
// resolves to it is refused for a sandbox seat as it is for a host seat.
func SandboxTownRootPath() string { return sandboxTownRoot }

// SandboxTaskFilePath is the path the runtime archive stages a seat's task at
// inside a sandbox: TASK.md under the sandbox root (runtimeTaskFile). A
// file-mode seat reads its assignment from there (gt-w1mzol).
func SandboxTaskFilePath() string { return path.Join(sandboxHome, runtimeTaskFile) }
