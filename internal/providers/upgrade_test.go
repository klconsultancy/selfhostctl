package providers

import (
	"slices"
	"strings"
	"testing"

	"github.com/klconsultancy/selfhostctl/internal/support"
)

// installedAs is w installed with the given versions (by key) and fingerprint.
func installedAs(w Wanted, fp string, versions map[string]string) []Installed {
	var out []Installed
	for _, p := range w.Providers {
		if v, ok := versions[p.Key()]; ok {
			p.Version = v
		}
		p.URL = ""
		out = append(out, Installed{Provider: p, Fingerprint: fp})
	}
	return out
}

func stepNames(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.String())
	}
	return out
}

func TestPlan(t *testing.T) {
	w := FromConfig(cfg())
	fp := Fingerprint(w.Variables)
	for _, tt := range []struct {
		name      string
		installed []Installed
		steps     []string
		blocked   string
		adopt     bool
	}{
		{name: "nothing to change", installed: installedAs(w, fp, nil)},
		{
			name:      "one version",
			installed: installedAs(w, fp, map[string]string{"infrastructure-proxmox": "v0.9.1"}),
			steps:     []string{"infrastructure-proxmox v0.9.1 → v0.9.2-kl.1"},
		},
		{
			name:      "the core and a provider, the core first",
			installed: installedAs(w, fp, map[string]string{"cluster-api": "v1.13.2", "bootstrap-k0sproject-k0smotron": "v2.2.0"}),
			steps:     []string{"cluster-api v1.13.2 → " + support.CAPIVersion, "bootstrap-k0sproject-k0smotron v2.2.0 → v2.3.0"},
		},
		{
			name:      "other variables reinstall every provider",
			installed: installedAs(w, "0ld", nil),
			steps: []string{
				"cluster-api " + support.CAPIVersion + " installed again with the config's variables",
				"bootstrap-k0sproject-k0smotron v2.3.0 installed again with the config's variables",
				"control-plane-k0sproject-k0smotron v2.3.0 installed again with the config's variables",
				"infrastructure-proxmox v0.9.2-kl.1 installed again with the config's variables",
			},
		},
		{name: "unstamped only: adopt", installed: installedAs(w, "", nil), adopt: true},
		{
			name: "unstamped next to an upgrade: reinstalled",
			installed: func() []Installed {
				i := installedAs(w, fp, map[string]string{"infrastructure-proxmox": "v0.9.1"})
				i[1].Fingerprint = ""
				return i
			}(),
			steps: []string{"bootstrap-k0sproject-k0smotron v2.3.0 installed again with the config's variables", "infrastructure-proxmox v0.9.1 → v0.9.2-kl.1"},
		},
		{name: "a declared provider missing", installed: installedAs(w, fp, nil)[:3], blocked: "infrastructure-proxmox@v0.9.2-kl.1 is declared but not installed"},
		{
			name:      "an undeclared provider",
			installed: append(installedAs(w, fp, nil), Installed{Provider: Provider{Type: "AddonProvider", Name: "helm", Version: "v0.3.0"}, Fingerprint: fp}),
			blocked:   "addon-helm@v0.3.0 is installed but not declared",
		},
		{
			name:      "a newer core",
			installed: installedAs(w, fp, map[string]string{"cluster-api": "v1.99.1"}),
			blocked:   "use the selfhostctl release for Cluster API v1.99",
		},
		{
			name:      "a core too far behind",
			installed: installedAs(w, fp, map[string]string{"cluster-api": "v1.9.0"}),
			blocked:   "more than 3 minors behind",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			steps, blocked, adopt := Plan(w, tt.installed)
			if got := stepNames(steps); !slices.Equal(got, tt.steps) {
				t.Errorf("steps %q, want %q", got, tt.steps)
			}
			if tt.blocked == "" && len(blocked) > 0 || !strings.Contains(strings.Join(blocked, "; "), tt.blocked) {
				t.Errorf("blocked %q, want %q", blocked, tt.blocked)
			}
			if adopt != tt.adopt {
				t.Errorf("adopt %v", adopt)
			}
		})
	}
}
