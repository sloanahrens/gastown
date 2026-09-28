// Package cli provides CLI configuration utilities.
package cli

import (
	"os"
	"sync"
)

// nameCache resolves the CLI name once from getenv and caches it.
type nameCache struct {
	getenv func(string) string
	once   sync.Once
	name   string
}

func (c *nameCache) get() string {
	c.once.Do(func() {
		c.name = c.getenv("GT_COMMAND")
		if c.name == "" {
			c.name = "gt"
		}
	})
	return c.name
}

var processName = &nameCache{getenv: os.Getenv}

// Name returns the Gas Town CLI command name.
// Defaults to "gt", but can be overridden with GT_COMMAND env var.
// This allows coexistence with other tools that use "gt" (e.g., Graphite).
func Name() string {
	return processName.get()
}
