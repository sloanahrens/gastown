//go:build windows

package cmd

import (
	"errors"
	"os"
)

func fileInode(os.FileInfo) uint64 { return 0 }

func reexecSelf(string) error { return errors.New("re-exec is not supported on windows") }
