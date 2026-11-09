package providers

import (
	"slices"
	"strings"
	"testing"
)

func TestBetween(t *testing.T) {
	w := FromConfig(cfg())
	same := installedFor(w)

	if d, u := Between(same, installedFor(w)); len(d) != 0 || len(u) != 0 {
		t.Errorf("identical sides: %v %v", d, u)
	}

	older := installedFor(w)
	older[1].Version = "v2.2.0"
	if d, _ := Between(same, older); len(d) != 1 || !strings.Contains(d[0], "at v2.3.0 on the source, v2.2.0 on the target") {
		t.Errorf("a version mismatch: %v", d)
	}

	gates := installedFor(w)
	gates[0].Fingerprint = Fingerprint(map[string]string{"CLUSTER_TOPOLOGY": "true"})
	if d, _ := Between(same, gates); len(d) != 1 || !strings.Contains(d[0], "other variables") {
		t.Errorf("a feature-gate mismatch: %v", d)
	}

	missing := installedFor(w)[:3]
	if d, _ := Between(same, missing); len(d) != 1 || !strings.Contains(d[0], "on the source only") {
		t.Errorf("a provider missing on the target: %v", d)
	}

	unstamped := installedFor(w)
	for i := range unstamped {
		unstamped[i].Fingerprint = ""
	}
	d, u := Between(same, unstamped)
	if len(d) != 0 || len(u) != len(w.Providers) {
		t.Errorf("an unstamped target: diffs %v, unstamped %v", d, u)
	}
}

func TestUnstampedAndVersionsMatch(t *testing.T) {
	w := FromConfig(cfg())
	plain := installedFor(w)
	for i := range plain {
		plain[i].Fingerprint = ""
	}
	if !Unstamped(w, plain) {
		t.Error("matching versions without fingerprints must count as unstamped")
	}
	if Unstamped(w, installedFor(w)) {
		t.Error("stamped providers are not unstamped")
	}
	plain[1].Version = "v2.2.0"
	if Unstamped(w, plain) || !slices.ContainsFunc(VersionsMatch(w, plain), func(s string) bool { return strings.Contains(s, "v2.2.0") }) {
		t.Error("another version is a version mismatch, not unstamped")
	}
}
