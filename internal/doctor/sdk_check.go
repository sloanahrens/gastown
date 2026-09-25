package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// MacOSSDKCheck verifies that the SDK a cgo build links against declares only
// architectures the installed linker parses. A Command Line Tools that ships a
// stub advertising an architecture ld rejects fails every cgo link, and the
// linker names neither the SDK nor the remedy — so the break presents as an
// unexplained build failure on a host that otherwise looks healthy (gt-1a0t).
type MacOSSDKCheck struct {
	BaseCheck
}

// NewMacOSSDKCheck creates a new macOS SDK check.
func NewMacOSSDKCheck() *MacOSSDKCheck {
	return &MacOSSDKCheck{
		BaseCheck: BaseCheck{
			CheckName:        "macos-sdk",
			CheckDescription: "Verify the macOS SDK a cgo build links against is well-formed",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// resolveSDK reports the SDK a build will link against and where it came from.
// SDKROOT wins because the daemon passes it through to every polecat and
// refinery build (config.DaemonEnvPath); xcrun is the fallback for a shell that
// sets nothing.
var resolveSDK = func() (path, source string, err error) {
	if root := strings.TrimSpace(os.Getenv("SDKROOT")); root != "" {
		return root, "SDKROOT", nil
	}
	out, err := exec.Command("xcrun", "--show-sdk-path").Output() //nolint:gosec // G204: fixed args, no user input
	if err != nil {
		return "", "", err
	}
	path = strings.TrimSpace(string(out))
	if path == "" {
		return "", "", fmt.Errorf("xcrun --show-sdk-path reported no SDK")
	}
	return path, "xcrun", nil
}

// stubScanRoots are the SDK directories a cgo link resolves stubs from. usr/lib
// holds libSystem, libresolv and libc++, which every cgo link needs; the
// frameworks hold CoreFoundation and Security (gt-1a0t).
var stubScanRoots = []string{
	filepath.Join("usr", "lib"),
	filepath.Join("System", "Library", "Frameworks"),
}

// stubHeadBytes bounds the read per stub: the top-level targets key tbd
// requires sits within the first handful of lines, so the head is enough to
// judge a stub and a full-SDK scan stays cheap.
const stubHeadBytes = 4096

// stubOffense is a stub whose declared architectures include one ld rejects.
type stubOffense struct {
	path  string   // relative to the SDK root, for a readable report
	archs []string // the offending target tokens
}

// sdkScan is what a scan observed: the offending stubs, and how many stubs it
// was able to judge. The count distinguishes "no bad stub" from "nothing read",
// which the check must not report as the same clean pass.
type sdkScan struct {
	offenses  []stubOffense
	inspected int
}

// scanMalformedStubs judges every stub under the SDK's link roots. A stub that
// cannot be read declares nothing this check can judge, so it is passed over
// rather than counted as clean.
var scanMalformedStubs = func(sdkPath string) (sdkScan, error) {
	var scan sdkScan
	for _, root := range stubScanRoots {
		walkRoot := filepath.Join(sdkPath, root)
		if _, err := os.Stat(walkRoot); err != nil {
			continue // not every SDK ships every root
		}
		err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".tbd") {
				return nil
			}
			head, err := readHead(path, stubHeadBytes)
			if err != nil {
				return nil
			}
			scan.inspected++
			if archs := malformedTargets(stubTopLevelTargets(head)); len(archs) > 0 {
				rel, relErr := filepath.Rel(sdkPath, path)
				if relErr != nil {
					rel = path
				}
				scan.offenses = append(scan.offenses, stubOffense{path: rel, archs: archs})
			}
			return nil
		})
		if err != nil {
			return scan, err
		}
	}
	return scan, nil
}

// readHead reads at most n bytes of path.
func readHead(path string, n int) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is walked from the SDK root
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, n)
	read, err := f.Read(buf)
	if err != nil && read == 0 {
		return "", err
	}
	return string(buf[:read]), nil
}

// stubTopLevelTargets returns the architecture tokens of a stub's top-level
// targets list. The list wraps across lines, and a corrupt stub puts its
// offending token on the continuation line, so the parse follows the list to
// its closing bracket.
func stubTopLevelTargets(stub string) []string {
	lines := strings.Split(stub, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "targets:") {
			continue
		}
		list := strings.TrimPrefix(trimmed, "targets:")
		for j := i + 1; j < len(lines) && !strings.Contains(list, "]"); j++ {
			list += " " + strings.TrimSpace(lines[j])
		}
		open := strings.Index(list, "[")
		closing := strings.LastIndex(list, "]")
		if open < 0 || closing < open {
			return nil
		}
		var targets []string
		for _, tok := range strings.Split(list[open+1:closing], ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				targets = append(targets, tok)
			}
		}
		return targets
	}
	return nil
}

// malformedTargets returns the target tokens whose architecture ld rejects.
// Every architecture the linker accepts is a bare name (arm64, arm64e, x86_64,
// x86_64h); a dotted architecture such as arm64e.x1 is a vendor variant ld
// reads as "unknown architecture".
func malformedTargets(targets []string) []string {
	var bad []string
	for _, target := range targets {
		arch, _, _ := strings.Cut(target, "-")
		if strings.Contains(arch, ".") {
			bad = append(bad, target)
		}
	}
	return bad
}

// maxReportedStubs bounds the offender list so one SDK-wide packaging bug does
// not bury the remedy under hundreds of paths.
const maxReportedStubs = 5

// sdkFixHint names the two remedies: a well-formed SDK already on the host,
// selected through the file the daemon reads, or a reinstall when none is.
const sdkFixHint = "Point SDKROOT at a well-formed SDK in settings/daemon.env (see Reference, Daemon Environment), or reinstall Command Line Tools"

// Run reports whether the SDK a cgo build links against is well-formed.
func (c *MacOSSDKCheck) Run(_ *CheckContext) *CheckResult {
	if runtime.GOOS != "darwin" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "SDK check not applicable (non-macOS)",
		}
	}

	sdkPath, source, err := resolveSDK()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not resolve the SDK a cgo build links against",
			Details: []string{err.Error()},
		}
	}

	// A stale SDKROOT outlives the SDK it names — an operator who renames or
	// removes the offending SDK leaves every build pointed at a deleted path.
	if _, err := os.Stat(sdkPath); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("SDK %s (from %s) is missing", sdkPath, source),
			Details: []string{err.Error()},
			FixHint: sdkFixHint,
		}
	}

	scan, err := scanMalformedStubs(sdkPath)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("SDK %s (from %s) could not be read", sdkPath, source),
			Details: []string{err.Error()},
			FixHint: sdkFixHint,
		}
	}

	if scan.inspected == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: fmt.Sprintf("unknown: found no linker stubs under %s", sdkPath),
			Details: []string{fmt.Sprintf("Resolved from %s.", source)},
		}
	}

	if len(scan.offenses) > 0 {
		details := []string{
			fmt.Sprintf("Resolved from %s.", source),
			"While these stubs are on the link path, every cgo link fails with 'tapi error: malformed file' and 'unknown architecture'.",
		}
		for i, off := range scan.offenses {
			if i == maxReportedStubs {
				details = append(details, fmt.Sprintf("... and %d more stubs.", len(scan.offenses)-maxReportedStubs))
				break
			}
			details = append(details, fmt.Sprintf("%s: %s", off.path, strings.Join(off.archs, ", ")))
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("SDK %s has %d stub(s) declaring an architecture the linker cannot parse", sdkPath, len(scan.offenses)),
			Details: details,
			FixHint: sdkFixHint,
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("SDK %s declares only architectures ld parses", sdkPath),
		Details: []string{
			fmt.Sprintf("Resolved from %s.", source),
			fmt.Sprintf("Judged %d stubs under %s.", scan.inspected, strings.Join(stubScanRoots, " and ")),
		},
	}
}
