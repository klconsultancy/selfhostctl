package providers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/version"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
)

// Upgrader brings a cluster's installed providers to Wanted.
type Upgrader interface {
	// Upgrade installs each step's provider at its version with w's variables, then
	// stamps every provider with w's fingerprint.
	Upgrade(ctx context.Context, kubeconfig []byte, w Wanted, steps []Step) error
}

// Step is one provider `upgrade` changes: to another version, or at the same one
// installed again with the config's variables.
type Step struct {
	// Provider is the target.
	Provider
	// From is the installed version.
	From string
}

func (s Step) String() string {
	if s.From == s.Version {
		return s.Key() + " " + s.Version + " installed again with the config's variables"
	}
	return fmt.Sprintf("%s %s → %s", s.Key(), s.From, s.Version)
}

// MaxCoreMinors is how many minors clusterctl lets the core skip in one upgrade.
const MaxCoreMinors = 3

// Plan is how `upgrade` brings installed to w: its steps, or why it can't (blocked).
// adopt is true when the only difference is providers without a fingerprint at the
// config's versions: `providers adopt` stamps them, no reinstall needed.
//
// Every provider whose version or fingerprint differs is a step. The fingerprint
// covers all variables, so a changed variable reinstalls every provider. A declared
// provider that isn't installed, or an installed one that isn't declared, blocks:
// upgrade installs and removes nothing. A core newer than w's, or more than
// MaxCoreMinors behind it, blocks.
func Plan(w Wanted, installed []Installed) (steps []Step, blocked []string, adopt bool) {
	fp := Fingerprint(w.Variables)
	have := map[string]Installed{}
	for _, i := range installed {
		have[i.Key()] = i
	}
	var unstamped []Step
	for _, p := range w.Providers {
		i, ok := have[p.Key()]
		delete(have, p.Key())
		switch {
		case !ok:
			blocked = append(blocked, p.String()+" is declared but not installed: upgrade installs no new provider")
		case p.Type == clusterctlv1.CoreProviderType && coreBlocked(i.Version, p.Version) != "":
			blocked = append(blocked, coreBlocked(i.Version, p.Version))
		case i.Version != p.Version:
			steps = append(steps, Step{Provider: p, From: i.Version})
		case i.Fingerprint == "":
			unstamped = append(unstamped, Step{Provider: p, From: i.Version})
		case i.Fingerprint != fp:
			steps = append(steps, Step{Provider: p, From: i.Version})
		}
	}
	for _, i := range have {
		blocked = append(blocked, i.String()+" is installed but not declared: upgrade removes no provider")
	}
	slices.Sort(blocked)
	if len(blocked) > 0 {
		return nil, blocked, false
	}
	if len(steps) == 0 {
		return nil, nil, len(unstamped) > 0
	}
	// Reinstalled with the config's variables, an unstamped provider may be stamped.
	steps = append(steps, unstamped...)
	slices.SortFunc(steps, func(a, b Step) int {
		if a.Type.Order() != b.Type.Order() {
			return a.Type.Order() - b.Type.Order()
		}
		return strings.Compare(a.Key(), b.Key())
	})
	return steps, nil, false
}

// coreBlocked is why the core can't go from one version to the other, or "".
func coreBlocked(from, to string) string {
	f, err1 := version.ParseSemantic(from)
	t, err2 := version.ParseSemantic(to)
	if err1 != nil || err2 != nil {
		return fmt.Sprintf("core-cluster-api is at %q, which isn't a version this release can compare with %s", from, to)
	}
	switch {
	case f.GreaterThan(t):
		return fmt.Sprintf("core-cluster-api is at %s, newer than %s, which this release manages: use the selfhostctl release for Cluster API v%d.%d (README, support matrix)",
			from, to, f.Major(), f.Minor())
	case f.Major() == t.Major() && t.Minor() > f.Minor()+MaxCoreMinors:
		return fmt.Sprintf("core-cluster-api is at %s, more than %d minors behind %s: upgrade through a selfhostctl release in between (README, support matrix)",
			from, MaxCoreMinors, to)
	}
	return ""
}
