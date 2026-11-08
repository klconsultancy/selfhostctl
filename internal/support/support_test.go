package support

import (
	"os"
	"strings"
	"testing"
)

func TestMinor(t *testing.T) {
	for v, want := range map[string]int{"v1.14.4": 14, "v1.15.0": 15, "v2.0.0": 0, "x": 0} {
		if got := minor(v); got != want {
			t.Errorf("minor(%q) = %d, want %d", v, got, want)
		}
	}
}

// The core the tool installs is the cluster-api library it links, and the replace of
// its api module points at the same release.
func TestCAPIVersionMatchesGoMod(t *testing.T) {
	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := string(data)
	for _, want := range []string{
		"\tsigs.k8s.io/cluster-api " + CAPIVersion + "\n",
		"sigs.k8s.io/cluster-api/api => sigs.k8s.io/cluster-api/api " + CAPIVersion + "\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("go.mod lacks %q: CAPIVersion and go.mod disagree", strings.TrimSpace(want))
		}
	}
}
