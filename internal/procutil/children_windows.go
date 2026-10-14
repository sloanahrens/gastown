//go:build windows

package procutil

// KillChildren kills nothing: no caller restarts in place on Windows, so there
// is no restart whose children need draining.
func KillChildren() int { return 0 }

// ReapZombieChildren reaps nothing: Windows has no zombie processes, and no
// caller restarts in place there.
func ReapZombieChildren() int { return 0 }
