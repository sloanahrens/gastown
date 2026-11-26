package specsplit

import (
	"fmt"
	"strings"
)

// GroupByBoxes splits a spec whose Constraints bullets name one package or none
// into a code group and a test-only group: the first holds every bullet and the
// first acceptance box, the second holds the remaining boxes and no bullets,
// and both name the one package the bullets name, or "." when they name none.
// It refuses, naming the cause, a spec with fewer than two boxes, a box with no
// Run command or whose Run names no go test command, bullets naming more than
// one package (such a spec is for Group), and a bullet naming the root package.
//
// The split answers the beads the package split cannot: a single-package bead
// whose further boxes test code the first box's piece writes (gt-dvs3va).
func GroupByBoxes(spec Spec) ([]FileGroup, error) {
	dirs, err := newDirIndex(spec.Dirs)
	if err != nil {
		return nil, err
	}
	bs := boxes(spec.Acceptance)
	if len(bs) < 2 {
		// Also keeps the first box below in range.
		return nil, fmt.Errorf("a box-group split needs at least two acceptance boxes, and the spec has %d", len(bs))
	}
	pkg, err := bulletPackageOf(dirs, spec.Constraints)
	if err != nil {
		return nil, err
	}
	groups := []FileGroup{
		// A fresh slice each: a caller that grows one group's boxes must not
		// overwrite the other's.
		{Package: pkg, Constraints: bullets(spec.Constraints), Boxes: []Box{bs[0]}},
		{Package: pkg, Boxes: append([]Box(nil), bs[1:]...)},
	}
	// checkGroups is the authority on a box with no Run command, a box whose Run
	// names no go test command and a box held twice; the groups it passes are
	// the ones Plan's pieces can carry.
	if err := checkGroups(spec, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// bulletPackageOf returns the one package directory the bullets name, taking
// each bullet's first package token as Group's bulletPackage does, and "." when
// they name none. A bullet naming the root package is refused, as it is there.
func bulletPackageOf(dirs *dirIndex, constraints string) (string, error) {
	var packages []string
	for _, bullet := range bullets(constraints) {
		pkg, place := firstPackageToken(dirs, bullet)
		switch place {
		case tokenIsRoot:
			return "", fmt.Errorf("constraints bullet %q names the root package ., which this planner does not place", bullet)
		case tokenIsPackage:
			if !contains(packages, pkg) {
				packages = append(packages, pkg)
			}
		}
	}
	switch len(packages) {
	case 0:
		return ".", nil
	case 1:
		return packages[0], nil
	}
	return "", fmt.Errorf("constraints bullets name %d packages (%s), so this spec is for Group",
		len(packages), strings.Join(packages, ", "))
}

// firstPackageToken returns what the first token of a bullet that names a
// package names: dirIndex.resolve is the one authority on that, so a bullet
// resolves here exactly as Group's bulletPackage resolves it.
func firstPackageToken(dirs *dirIndex, bullet string) (pkg string, place placement) {
	for _, token := range strings.FieldsFunc(bullet, splitToken) {
		if p, place := dirs.resolve(token); place != tokenIsProse {
			return p, place
		}
	}
	return "", tokenIsProse
}
