package binaryinstall

import (
	"fmt"
	"io"
	"path/filepath"
)

// runUsage is the usage line of the install-binary command, the script's with
// the command's own name.
const runUsage = "usage: install-binary <built-binary> <install-dir> [name]"

// Run is the install-binary command: it replaces destDir/name with the built
// binary src and returns the process exit status (gt-tpg0p.68). It is the
// script's sequence: the source is checked, the directory made, the copy
// written beside the target, the copy signed, the immutable flag cleared, and
// the copy renamed over the target with the flag restored.
//
//	install-binary <built-binary> <install-dir> [name]
//
// Replace owns the copy, flag and rename steps, so the signing rides on its
// copy: the copy is signed once, before the flag comes off and before the
// rename, and no second copy of a binary over a hundred megabytes is made.
//
// inst and sign are the seams. Their zero values run against the real
// filesystem, the real lock and the real codesign. Every notice prints to
// stderr under the command's name, and a usage error, a missing source or a
// failed install exits 1.
func Run(args []string, stderr io.Writer, inst Installer, sign Signer) int {
	if len(args) < 2 || args[0] == "" || args[1] == "" {
		fmt.Fprintln(stderr, runUsage)
		return 1
	}
	src, destDir, name := args[0], args[1], ""
	if len(args) > 2 {
		name = args[2]
	}
	if name == "" {
		name = filepath.Base(src)
	}
	if !inst.Files.isFile(src) {
		fmt.Fprintf(stderr, "install-binary: source binary not found: %s\n", src)
		return 1
	}
	inst.Warnf = commandNotice(stderr)
	sign.Notef = commandNotice(stderr)
	// The signer restores a failed codesign's copy through the Files seam it is
	// given, so it takes the plain seam, not the signing copy below: a restore
	// that signed again would loop.
	sign.Installer = inst
	plainCopy := inst.Files.copy
	inst.Files.Copy = func(dst, from string) error {
		if err := plainCopy(dst, from); err != nil {
			return err
		}
		return sign.Sign(dst, from, name)
	}
	if err := inst.Replace(src, destDir, name); err != nil {
		fmt.Fprintf(stderr, "install-binary: %v\n", err)
		return 1
	}
	return 0
}

// commandNotice writes a notice under the command's name, as the script's
// echo lines were written under install-binary.sh:.
func commandNotice(stderr io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(stderr, "install-binary: "+format+"\n", args...)
	}
}
