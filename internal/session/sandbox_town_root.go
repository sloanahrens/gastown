package session

// SandboxTownRootPath is the town root inside a sandbox: the skeleton every
// sandbox seat's clone lives under. A mutating git call whose repository
// resolves to it is refused for a sandbox seat as it is for a host seat.
func SandboxTownRootPath() string { return sandboxTownRoot }
