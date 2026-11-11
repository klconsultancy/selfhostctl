package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

// fakeUpgrader brings the Management cluster's providers to the steps' versions.
type fakeUpgrader struct {
	w     *world
	calls [][]providers.Step
}

func (f *fakeUpgrader) Upgrade(_ context.Context, kc []byte, w providers.Wanted, steps []providers.Step) error {
	f.calls = append(f.calls, steps)
	fp := providers.Fingerprint(w.Variables)
	list := f.w.installed[string(kc)]
	for i := range list {
		for _, s := range steps {
			if list[i].Key() == s.Key() {
				list[i].Version = s.Version
			}
		}
		list[i].Fingerprint = fp
	}
	return nil
}

func (w *world) upgradePhases(t *testing.T, up providers.Upgrader) []phase.Phase {
	t.Helper()
	path := w.kubeconfig
	if path == "" {
		path = filepath.Join(t.TempDir(), "none.kubeconfig")
	}
	return UpgradePhases(pivotConfig(t, path, w.extraYAML), Deps{
		Docker:    w.docker,
		Providers: fakeSides{w},
		Upgrader:  up,
		Clients:   w.clients,
		Probe:     func(context.Context, string) bool { return w.probeAnswer },
		Now:       time.Now,
	})
}

func pivotedWorld(t *testing.T) *world {
	return &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: newFake(theCluster(false), kubeconfigSecret(managementKC_))}
}

func TestUpgrade(t *testing.T) {
	w := pivotedWorld(t)
	wanted := providers.FromConfig(&pivotConfig(t, w.kubeconfig).Providers)
	old := installedFor(wanted)
	old[1].Version = "v0.0.1"
	w.installed = map[string][]providers.Installed{managementKC_: old}
	up := &fakeUpgrader{w: w}

	var out bytes.Buffer
	if err := phase.Run(t.Context(), w.upgradePhases(t, up), "", &out); err != nil {
		t.Fatal(err)
	}
	if len(up.calls) != 1 || len(up.calls[0]) != 1 || up.calls[0][0].From != "v0.0.1" {
		t.Fatalf("upgrades %+v", up.calls)
	}
	key := old[1].Key()
	for _, want := range []string{
		"management: done (the Management cluster manages itself)",
		"providers: running (" + key + " v0.0.1 → " + wanted.Providers[1].Version + "; cert-manager to the version clusterctl ships)",
		"providers: done (",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// A second upgrade changes nothing.
	out.Reset()
	if err := phase.Run(t.Context(), w.upgradePhases(t, up), "", &out); err != nil {
		t.Fatal(err)
	}
	if len(up.calls) != 1 || strings.Contains(out.String(), "running") {
		t.Errorf("a second upgrade ran: %s", out.String())
	}
}

func TestUpgradeBlocks(t *testing.T) {
	for _, tt := range []struct {
		name   string
		world  func(t *testing.T) *world
		phase  string
		reason string
	}{
		{"before the pivot", func(t *testing.T) *world { return &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t)} }, "management", "finish `selfhostctl bootstrap` first"},
		{"a provider missing", func(t *testing.T) *world {
			w := pivotedWorld(t)
			wanted := providers.FromConfig(&pivotConfig(t, w.kubeconfig).Providers)
			w.installed = map[string][]providers.Installed{managementKC_: installedFor(wanted)[1:]}
			return w
		}, "providers", "is declared but not installed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.world(t)
			err := phase.Run(t.Context(), w.upgradePhases(t, &fakeUpgrader{w: w}), "", &bytes.Buffer{})
			var b *phase.BlockedError
			if !errors.As(err, &b) || b.Phase != tt.phase || !strings.Contains(b.Reason, tt.reason) {
				t.Errorf("got %v", err)
			}
		})
	}
}

// Providers the tool didn't install, at the config's versions: adopt them.
func TestUpgradeAsksToAdopt(t *testing.T) {
	w := pivotedWorld(t)
	wanted := providers.FromConfig(&pivotConfig(t, w.kubeconfig).Providers)
	list := installedFor(wanted)
	for i := range list {
		list[i].Fingerprint = ""
	}
	w.installed = map[string][]providers.Installed{managementKC_: list}
	err := phase.Run(t.Context(), w.upgradePhases(t, &fakeUpgrader{w: w}), "", &bytes.Buffer{})
	var c *phase.CheckpointError
	if !errors.As(err, &c) || !strings.Contains(c.Instruction, "providers adopt") {
		t.Errorf("got %v", err)
	}
}

// A config bumped before the outage: the Management cluster's new machines run the
// datastore's old providers, so move-to-management blocks and asks for upgrade,
// which runs at that step; recover then goes on.
func TestUpgradeDuringARecovery(t *testing.T) {
	w := down(t)
	ctx := t.Context()
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := w.breakGlass.Get(ctx, ctrlclient.ObjectKey{Namespace: "mgmt", Name: "mgmt"}, cl); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(cl.Object, conditions("ControlPlaneAvailable", "True", "Available", "True", "RemoteConnectionProbe", "True"), "status", "conditions")
	if err := w.breakGlass.Update(ctx, cl); err != nil {
		t.Fatal(err)
	}
	wanted := providers.FromConfig(&pivotConfig(t, w.kubeconfig).Providers)
	old := installedFor(wanted)
	old[1].Version = "v0.0.1"
	w.installed = map[string][]providers.Installed{bootstrapKC: installedFor(wanted), breakGlassKC: old, managementKC_: old}
	// The Break-glass controller runs on the datastore's providers; the move gate there
	// compares it with the Bootstrap cluster's: make the first move pass.
	w.installed[breakGlassKC] = installedFor(wanted)

	up := &fakeUpgrader{w: w}
	err := phase.Run(ctx, w.upgradePhases(t, up), "", &bytes.Buffer{})
	var b *phase.BlockedError
	if !errors.As(err, &b) || b.Phase != "management" {
		t.Fatalf("upgrade before the outage was handled: %v", err)
	}

	var out bytes.Buffer
	err = phase.Run(ctx, w.recoverPhases(t), "", &out)
	if !errors.As(err, &b) || b.Phase != "move-to-management" || !strings.Contains(b.Reason, "Run `selfhostctl upgrade") {
		t.Fatalf("recover: %v\n%s", err, out.String())
	}

	if err := phase.Run(ctx, w.upgradePhases(t, up), "", &out); err != nil {
		t.Fatalf("upgrade at move-to-management: %v", err)
	}
	if len(up.calls) != 1 {
		t.Fatalf("upgrades %+v", up.calls)
	}

	out.Reset()
	if err := phase.Run(ctx, w.recoverPhases(t), "", &out); err != nil {
		t.Fatalf("recover after the upgrade: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "move-to-management: done") {
		t.Errorf("recover after the upgrade:\n%s", out.String())
	}
}
