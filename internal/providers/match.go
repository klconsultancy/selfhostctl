package providers

import (
	"fmt"
	"slices"
)

// Between lists how two clusters' providers differ: the gate before every move. Every
// provider must be on both, at the same version, installed with the same variables
// (feature gates), so the topology controller defaults nothing differently after the
// move. A provider without a fingerprint on either side is listed in unstamped, not
// as a difference: only the operator can say what it was installed with.
func Between(source, target []Installed) (diffs, unstamped []string) {
	in := func(list []Installed) map[string]Installed {
		m := map[string]Installed{}
		for _, i := range list {
			m[i.Key()] = i
		}
		return m
	}
	src, dst := in(source), in(target)
	for key, s := range src {
		t, ok := dst[key]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s is on the source only", s))
		case s.Version != t.Version:
			diffs = append(diffs, fmt.Sprintf("%s is at %s on the source, %s on the target", key, s.Version, t.Version))
		case s.Fingerprint == "" || t.Fingerprint == "":
			unstamped = append(unstamped, key)
		case s.Fingerprint != t.Fingerprint:
			diffs = append(diffs, key+" was installed with other variables (feature gates) on each side")
		}
	}
	for key, t := range dst {
		if _, ok := src[key]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s is on the target only", t))
		}
	}
	slices.Sort(diffs)
	slices.Sort(unstamped)
	return diffs, unstamped
}

// VersionsMatch lists how installed differs from w in versions alone, ignoring the
// variables: what `providers adopt` checks before it stamps.
func VersionsMatch(w Wanted, installed []Installed) []string {
	stamped := make([]Installed, len(installed))
	fp := Fingerprint(w.Variables)
	for i, p := range installed {
		p.Fingerprint = fp
		stamped[i] = p
	}
	_, diffs := Compare(w, stamped)
	return diffs
}

// Unstamped reports whether installed matches w except that some providers carry no
// fingerprint: providers the tool didn't install, to be adopted.
func Unstamped(w Wanted, installed []Installed) bool {
	if len(VersionsMatch(w, installed)) > 0 {
		return false
	}
	for _, i := range installed {
		if i.Fingerprint == "" {
			return true
		}
	}
	return false
}
