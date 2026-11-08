// Package providers installs the declared Cluster API providers on a cluster through
// the clusterctl library, so clusterctl's inventory (which `move` relies on) is
// written as usual, and tells whether a cluster runs exactly what the config declares.
package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/support"
)

// Provider is one provider at one version.
type Provider struct {
	Type    clusterctlv1.ProviderType
	Name    string
	Version string
	// URL of its components, for a provider clusterctl doesn't know or a fork.
	URL string
}

// Key is how clusterctl names a provider in its inventory, e.g. infrastructure-docker.
func (p Provider) Key() string {
	return clusterctlv1.ManifestLabel(p.Name, p.Type)
}

func (p Provider) String() string { return p.Key() + "@" + p.Version }

// Wanted is what the config declares: the providers, the core included, and the
// clusterctl variables their components are rendered with.
type Wanted struct {
	Providers []Provider
	Variables map[string]string
}

// FromConfig derives Wanted from the config and the support matrix.
func FromConfig(c *config.Providers) Wanted {
	w := Wanted{
		Providers: []Provider{{Type: clusterctlv1.CoreProviderType, Name: "cluster-api", Version: support.CAPIVersion}},
		Variables: maps.Clone(c.Variables),
	}
	if w.Variables == nil {
		w.Variables = map[string]string{}
	}
	if c.ClusterTopology && support.ClusterTopologyGateNeeded() {
		w.Variables["CLUSTER_TOPOLOGY"] = "true"
	}
	add := func(t clusterctlv1.ProviderType, list []config.Provider) {
		for _, p := range list {
			w.Providers = append(w.Providers, Provider{Type: t, Name: p.Name, Version: p.Version, URL: p.URL})
		}
	}
	add(clusterctlv1.BootstrapProviderType, c.Bootstrap)
	add(clusterctlv1.ControlPlaneProviderType, c.ControlPlane)
	add(clusterctlv1.InfrastructureProviderType, c.Infrastructure)
	add(clusterctlv1.IPAMProviderType, c.IPAM)
	add(clusterctlv1.AddonProviderType, c.Addon)
	return w
}

// Fingerprint identifies a set of clusterctl variables without revealing their
// values: some may be credentials. It is recorded on every Provider the tool installs.
func Fingerprint(vars map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, vars[k]) // a hash never fails to write
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// FingerprintAnnotation is the annotation on each Provider object that records the
// variables it was installed with.
const FingerprintAnnotation = "selfhostctl.io/variables-fingerprint"

// Installed is a provider as a cluster's clusterctl inventory has it.
type Installed struct {
	Provider
	// Fingerprint of the variables it was installed with; empty when the tool didn't
	// install it.
	Fingerprint string
}

// Match tells how a cluster's providers compare with Wanted.
type Match int

const (
	// None: no provider is installed.
	None Match = iota
	// Exact: every wanted provider, at its version and with the wanted variables,
	// and nothing else.
	Exact
	// Differs: anything else; Compare lists the differences.
	Differs
)

// Compare lists how installed differs from w, sorted.
func Compare(w Wanted, installed []Installed) (Match, []string) {
	if len(installed) == 0 {
		return None, nil
	}
	fp := Fingerprint(w.Variables)
	have := map[string]Installed{}
	for _, i := range installed {
		have[i.Key()] = i
	}
	var diffs []string
	for _, p := range w.Providers {
		i, ok := have[p.Key()]
		switch {
		case !ok:
			diffs = append(diffs, p.String()+" is not installed")
		case i.Version != p.Version:
			diffs = append(diffs, fmt.Sprintf("%s is at %s, the config declares %s", p.Key(), i.Version, p.Version))
		case i.Fingerprint != fp:
			diffs = append(diffs, p.Key()+" was installed with other variables (feature gates) than the config declares")
		}
		delete(have, p.Key())
	}
	for _, i := range have {
		diffs = append(diffs, i.String()+" is installed but not declared")
	}
	slices.Sort(diffs)
	if len(diffs) == 0 {
		return Exact, nil
	}
	return Differs, diffs
}

// Installer reads and installs a cluster's providers.
type Installer interface {
	Installed(ctx context.Context, kubeconfig []byte) ([]Installed, error)
	// Init installs every wanted provider on a cluster that has none.
	Init(ctx context.Context, kubeconfig []byte, w Wanted) error
}

// Summary describes differences for a Blocked reason.
func Summary(diffs []string) string {
	return strings.Join(diffs, "; ")
}
