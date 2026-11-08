package providers

import (
	"slices"
	"strings"
	"testing"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/support"
)

func cfg() *config.Providers {
	return &config.Providers{
		Bootstrap:      []config.Provider{{Name: "k0sproject-k0smotron", Version: "v2.3.0"}},
		ControlPlane:   []config.Provider{{Name: "k0sproject-k0smotron", Version: "v2.3.0"}},
		Infrastructure: []config.Provider{{Name: "proxmox", Version: "v0.9.2-kl.1", URL: "https://github.com/fork/releases/latest/infrastructure-components.yaml"}},
		Variables:      map[string]string{"EXP_MACHINE_POOL": "true"},
	}
}

func TestFromConfig(t *testing.T) {
	c := cfg()
	c.ClusterTopology = true
	w := FromConfig(c)
	var got []string
	for _, p := range w.Providers {
		got = append(got, p.String())
	}
	want := []string{
		"cluster-api@" + support.CAPIVersion,
		"bootstrap-k0sproject-k0smotron@v2.3.0",
		"control-plane-k0sproject-k0smotron@v2.3.0",
		"infrastructure-proxmox@v0.9.2-kl.1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("providers %v, want %v", got, want)
	}
	if w.Providers[3].URL == "" {
		t.Error("the fork's URL got lost")
	}
	if w.Variables["EXP_MACHINE_POOL"] != "true" {
		t.Errorf("variables %v", w.Variables)
	}
	if support.ClusterTopologyGateNeeded() && w.Variables["CLUSTER_TOPOLOGY"] != "true" {
		t.Errorf("clusterTopology on CAPI %s must set CLUSTER_TOPOLOGY=true: %v", support.CAPIVersion, w.Variables)
	}
	if _, set := FromConfig(cfg()).Variables["CLUSTER_TOPOLOGY"]; set {
		t.Error("CLUSTER_TOPOLOGY set without clusterTopology")
	}
	if c.Variables["CLUSTER_TOPOLOGY"] != "" {
		t.Error("FromConfig changed the config's map")
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint(map[string]string{"A": "1", "B": "2"})
	if a != Fingerprint(map[string]string{"B": "2", "A": "1"}) {
		t.Error("the fingerprint depends on map order")
	}
	if a == Fingerprint(map[string]string{"A": "1", "B": "3"}) {
		t.Error("the fingerprint ignores a value")
	}
	if len(a) != 16 {
		t.Errorf("fingerprint %q, want 16 hex characters", a)
	}
}

func installedFor(w Wanted) []Installed {
	fp := Fingerprint(w.Variables)
	out := make([]Installed, len(w.Providers))
	for i, p := range w.Providers {
		p.URL = ""
		out[i] = Installed{Provider: p, Fingerprint: fp}
	}
	return out
}

func TestCompare(t *testing.T) {
	w := FromConfig(cfg())

	if m, _ := Compare(w, nil); m != None {
		t.Errorf("nothing installed: %v, want None", m)
	}
	if m, d := Compare(w, installedFor(w)); m != Exact {
		t.Errorf("all installed: %v %v, want Exact", m, d)
	}

	older := installedFor(w)
	older[1].Version = "v2.2.0"
	if m, d := Compare(w, older); m != Differs || !slices.Contains(d, "bootstrap-k0sproject-k0smotron is at v2.2.0, the config declares v2.3.0") {
		t.Errorf("older version: %v %v", m, d)
	}

	partial := installedFor(w)[:2]
	if m, d := Compare(w, partial); m != Differs || len(d) != 2 {
		t.Errorf("partial install: %v %v, want two missing", m, d)
	}

	gates := installedFor(w)
	gates[0].Fingerprint = Fingerprint(map[string]string{})
	if m, d := Compare(w, gates); m != Differs || !strings.Contains(strings.Join(d, ";"), "other variables") {
		t.Errorf("other variables: %v %v", m, d)
	}

	extra := append(installedFor(w), Installed{Provider: Provider{Type: clusterctlv1.IPAMProviderType, Name: "in-cluster", Version: "v1.1.1"}})
	if m, d := Compare(w, extra); m != Differs || !slices.Contains(d, "ipam-in-cluster@v1.1.1 is installed but not declared") {
		t.Errorf("undeclared provider: %v %v", m, d)
	}
}
