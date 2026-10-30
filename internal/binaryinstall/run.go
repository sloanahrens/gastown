package binaryinstall

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

// runUsage is the usage line of the install-binary command, the script's with
// the command's own name.
const runUsage = "usage: install-binary <built-binary> <install-dir> [name]"

// exitSigningFailed is the status of an install whose signing failed once the
// signing material was found. Nothing was installed. 1 is every other failure,
// and 2 is not used.
const exitSigningFailed = 3

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
// rename. A failed codesign restores the copy from the source, which is a second
// copy of the binary, so the temp never holds half-written bytes.
//
// inst and sign are the seams. Their zero values run against the real
// filesystem, the real lock and the real codesign. Every notice prints to
// stderr under the command's name. Exit status: 0 for an install, 1 for a usage
// error, a bad name, a missing source or a failed install, and 3 for a signing
// that failed once its material was found, where nothing is installed.
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
	if !bareName(name) {
		fmt.Fprintf(stderr, "install-binary: name %q is not a bare file name\n", name)
		return 1
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
		if errors.Is(err, ErrSigningFailed) {
			return exitSigningFailed
		}
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
