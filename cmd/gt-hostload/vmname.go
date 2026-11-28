package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

// lsofVM names the VM pid runs through (gt-efoycy): lsof -p pid, then VMName.
func lsofVM(pid int, read func(timeout time.Duration, name string, args ...string) (string, error)) (string, error) {
	text, err := read(10*time.Second, "lsof", "-p", strconv.Itoa(pid))
	if err != nil {
		return "", fmt.Errorf("lsof -p %d: %w", pid, err)
	}
	return hostload.VMName(text), nil
}
