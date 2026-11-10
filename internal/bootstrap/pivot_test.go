package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/flux"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
	"github.com/klconsultancy/selfhostctl/internal/recovery"
)

// The Bootstrap cluster's kubeconfig as the fake Docker returns it, and the
// Management cluster's as the kubeconfig file holds it.
const (
	bootstrapKC   = "server: https://127.0.0.1:32001\n"
	managementKC_ = "management\n"
	breakGlassKC  = "server: https://127.0.0.1:32002\n"
)

func newFake(objs ...ctrlclient.Object) ctrlclient.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{clusterGVK.GroupVersion(), k0sControlPlaneGVK.GroupVersion()})
	for _, gvk := range []schema.GroupVersionKind{clusterGVK, k0sControlPlaneGVK, {Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"},
		flux.KustomizationGVK, flux.HelmReleaseGVK, flux.FluxInstanceGVK, gitRepositoryGVK} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).Build()
}

// world is one test's clusters.
type world struct {
	docker      *fakeDocker
	bootstrap   ctrlclient.Client // nil: holds nothing
	management  ctrlclient.Client // nil: the kubeconfig doesn't reach it
	breakGlass  ctrlclient.Client // nil: its API doesn't answer
	kubeconfig  string            // the kubeconfig file's path; "" for none
	installed   map[string][]providers.Installed
	moves       []string
	probeAnswer bool
	refuse      bool              // the Management cluster answers Unauthorized
	extraYAML   string            // appended to the config
	env         map[string]string // the operator's environment
	// The datastore: writes is how much each read of its revision finds it grew;
	// dataSource is what was read.
	writes, revision int64
	dataSource       string
}

func (w *world) clients(kc []byte) (ctrlclient.Client, error) {
	switch string(kc) {
	case bootstrapKC:
		if w.bootstrap == nil {
			return newFake(), nil
		}
		return w.bootstrap, nil
	case managementKC_:
		if w.refuse {
			return nil, apierrors.NewUnauthorized("Unauthorized")
		}
		if w.management == nil {
			return nil, errors.New("connection refused")
		}
		return w.management, nil
	case breakGlassKC:
		if w.breakGlass == nil {
			return nil, errors.New("connection refused")
		}
		return w.breakGlass, nil
	}
	return nil, errors.New("unknown cluster")
}

// fakeSides is an Installer whose providers depend on the cluster.
type fakeSides struct{ w *world }

func (f fakeSides) Installed(_ context.Context, kc []byte) ([]providers.Installed, error) {
	return f.w.installed[string(kc)], nil
}
func (f fakeSides) Init(context.Context, []byte, providers.Wanted) error { return nil }

type fakeMover struct{ w *world }

func (f fakeMover) Move(_ context.Context, from, to []byte, ns string, dryRun bool) error {
	f.w.moves = append(f.w.moves, map[bool]string{true: "dry-run", false: "move"}[dryRun])
	return nil
}

func theCluster(paused bool, conds ...string) *unstructured.Unstructured {
	u := obj(clusterGVK, "mgmt", map[string]any{
		"spec":   map[string]any{"paused": paused},
		"status": map[string]any{"conditions": conditions(conds...)},
	})
	return u
}

func pivotConfig(t *testing.T, kubeconfig string, extra ...string) *config.ManagementCluster {
	t.Helper()
	cfg, err := config.Parse("t.yaml", []byte(strings.Replace(testYAML, "/nonexistent/selfhostctl-test/mgmt.kubeconfig", kubeconfig, 1)+strings.Join(extra, "")))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (w *world) phases(t *testing.T, first bool) []phase.Phase {
	t.Helper()
	path := w.kubeconfig
	if path == "" {
		path = filepath.Join(t.TempDir(), "none.kubeconfig")
	}
	wanted := providers.FromConfig(&pivotConfig(t, path).Providers)
	stamped := installedFor(wanted)
	if w.installed == nil {
		w.installed = map[string][]providers.Installed{bootstrapKC: stamped, managementKC_: installedFor(wanted)}
	}
	return Phases(pivotConfig(t, path, w.extraYAML), Deps{
		Docker:        w.docker,
		NodesReady:    func(context.Context, []byte) (bool, error) { return true, nil },
		Providers:     fakeSides{w},
		Mover:         fakeMover{w},
		Clients:       w.clients,
		Lookup:        func(k string) (string, bool) { v, ok := w.env[k]; return v, ok },
		First:         first,
		Probe:         func(context.Context, string) bool { return w.probeAnswer },
		Now:           time.Now,
		GitOpsTimeout: 2 * time.Second,
		GitOpsPoll:    5 * time.Millisecond,
	})
}

func installedFor(w providers.Wanted) []providers.Installed {
	fp := providers.Fingerprint(w.Variables)
	out := make([]providers.Installed, len(w.Providers))
	for i, p := range w.Providers {
		p.URL = ""
		out[i] = providers.Installed{Provider: p, Fingerprint: fp}
	}
	return out
}

func kubeconfigFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mgmt.kubeconfig")
	if err := os.WriteFile(p, []byte(managementKC_), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func states(t *testing.T, ps []phase.Phase) map[string]phase.Status {
	t.Helper()
	out := map[string]phase.Status{}
	for _, p := range ps {
		st, err := p.Status(t.Context())
		if err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
		out[p.Name()] = st
	}
	return out
}

func TestLocate(t *testing.T) {
	tests := []struct {
		name  string
		world func(t *testing.T) *world
		want  Location
	}{
		{"nothing, no kubeconfig", func(t *testing.T) *world { return &world{docker: newFakeDocker()} }, LocationUnknown},
		{"a kubeconfig that doesn't answer", func(t *testing.T) *world { return &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t)} }, LocationUnknown},
		{"a reachable cluster without the Cluster", func(t *testing.T) *world {
			return &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: newFake()}
		}, LocationNone},
		{"our own Bootstrap cluster, empty", func(t *testing.T) *world { return &world{docker: running(t)} }, LocationNone},
		// After the pivot, the Bootstrap cluster stays for the stable window; a
		// Management cluster that doesn't answer then must not read as none.
		{"an empty Bootstrap cluster and a kubeconfig that doesn't answer", func(t *testing.T) *world {
			return &world{docker: running(t), kubeconfig: kubeconfigFile(t)}
		}, LocationUnknown},
		{"in the Bootstrap cluster", func(t *testing.T) *world { return &world{docker: running(t), bootstrap: newFake(theCluster(false))} }, LocationBootstrap},
		{"on both sides", func(t *testing.T) *world {
			return &world{docker: running(t), bootstrap: newFake(theCluster(true)), kubeconfig: kubeconfigFile(t), management: newFake(theCluster(true))}
		}, LocationSplit},
		{"pivoted", func(t *testing.T) *world {
			return &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: newFake(theCluster(false))}
		}, LocationManagement},
		{"on the Break-glass controller", func(t *testing.T) *world {
			w := &world{docker: running(t), kubeconfig: kubeconfigFile(t), breakGlass: newFake(theCluster(false))}
			w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{running: true}
			return w
		}, LocationBreakGlass},
		{"on the Break-glass controller and in the Bootstrap cluster", func(t *testing.T) *world {
			w := &world{docker: running(t), kubeconfig: kubeconfigFile(t), breakGlass: newFake(theCluster(true)), bootstrap: newFake(theCluster(true))}
			w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{running: true}
			return w
		}, LocationSplit},
		{"a Break-glass controller whose API doesn't answer yet", func(t *testing.T) *world {
			w := &world{docker: running(t), kubeconfig: kubeconfigFile(t)}
			w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{running: true}
			return w
		}, LocationUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.world(t)
			path := w.kubeconfig
			if path == "" {
				path = filepath.Join(t.TempDir(), "none.kubeconfig")
			}
			s, err := Locate(t.Context(), pivotConfig(t, path), Deps{Docker: w.docker, Clients: w.clients})
			if err != nil || s.Location != tt.want {
				t.Errorf("Locate: %v (%s) %v, want %v", s.Location, s.Reason, err, tt.want)
			}
		})
	}
}

func TestFailClosed(t *testing.T) {
	w := &world{docker: newFakeDocker()}
	st := states(t, w.phases(t, false))["bootstrap-cluster"]
	if st.State != phase.Blocked || !strings.Contains(st.Reason, "--first") {
		t.Errorf("no handle, no --first: %+v, want blocked naming --first", st)
	}
	if st := states(t, w.phases(t, true))["bootstrap-cluster"]; st.State != phase.Next {
		t.Errorf("--first: %+v, want next", st)
	}
	// A stale kubeconfig, whose server can be probed.
	stale := filepath.Join(t.TempDir(), "stale.kubeconfig")
	if err := os.WriteFile(stale, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: m\n  cluster:\n    server: https://10.0.0.1:6443\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.kubeconfig = stale
	w.probeAnswer = true
	if st := states(t, w.phases(t, true))["bootstrap-cluster"]; st.State != phase.Blocked || !strings.Contains(st.Reason, "doesn't answer") {
		t.Errorf("--first with a kubeconfig that doesn't answer: %+v, want blocked", st)
	}

	// Without a kubeconfig, a known endpoint from the manifests that answers refuses
	// --first.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), []byte("apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2\nkind: ProxmoxCluster\nmetadata: {name: mgmt, namespace: mgmt}\nspec:\n  controlPlaneEndpoint: {host: 10.0.0.1, port: 6443}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(cfgFile, []byte(strings.Replace(testYAML, "/nonexistent/selfhostctl-test/mgmt.kubeconfig", filepath.Join(dir, "none.kubeconfig"), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	ps := Phases(cfg, Deps{Docker: newFakeDocker(), Clients: w.clients, First: true, Probe: func(context.Context, string) bool { return true }})
	if st := statusOf(t, ps, "bootstrap-cluster"); st.State != phase.Blocked || !strings.Contains(st.Reason, "--first is refused") || !strings.Contains(st.Reason, "10.0.0.1:6443") {
		t.Errorf("--first while the manifests' endpoint answers: %+v, want refused", st)
	}
}

// After the pivot, nothing before it runs again and no Bootstrap cluster appears.
// kubeconfigSecret is Cluster API's <cluster>-kubeconfig Secret holding value.
func kubeconfigSecret(value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "mgmt", Name: "mgmt-kubeconfig"}, Data: map[string][]byte{"value": []byte(value)}}
}

func TestPivotedRunsNothing(t *testing.T) {
	w := &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: newFake(theCluster(false), kubeconfigSecret(managementKC_))}
	got := states(t, w.phases(t, false))
	for _, name := range []string{"bootstrap-cluster", "providers", "before-apply", "apply", "ready", "recovery-bundle"} {
		if got[name].State != phase.Done || got[name].Reason != "pivoted" {
			t.Errorf("%s: %+v, want done (pivoted)", name, got[name])
		}
	}
	for _, name := range []string{"kubeconfig", "management-providers", "move", "delete-bootstrap-cluster"} {
		if got[name].State != phase.Done {
			t.Errorf("%s: %+v, want done", name, got[name])
		}
	}
	if len(w.docker.created) != 0 {
		t.Error("a Bootstrap cluster was created")
	}
}

// statusOf judges one phase.
func statusOf(t *testing.T, ps []phase.Phase, name string) phase.Status {
	t.Helper()
	for _, p := range ps {
		if p.Name() == name {
			st, err := p.Status(t.Context())
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return st
		}
	}
	t.Fatalf("no phase %s", name)
	return phase.Status{}
}

func TestMoveGates(t *testing.T) {
	newWorld := func(t *testing.T, objs ...ctrlclient.Object) *world {
		return &world{docker: running(t), bootstrap: newFake(append([]ctrlclient.Object{theCluster(false)}, objs...)...), kubeconfig: kubeconfigFile(t), management: newFake()}
	}

	w := newWorld(t)
	if st := statusOf(t, w.phases(t, false), "move"); st.State != phase.Next {
		t.Fatalf("matching sides: %+v, want next", st)
	}
	if err := phase.Run(t.Context(), w.phases(t, false), "move", ioDiscard{}); err == nil {
		t.Log("ran")
	}
	if strings.Join(w.moves, ",") != "dry-run,move" {
		t.Errorf("moves %v, want a dry run first", w.moves)
	}

	w = newWorld(t)
	_ = w.phases(t, false) // fill installed
	w.installed[managementKC_][1].Version = "v2.2.0"
	if st := statusOf(t, w.phases(t, false), "move"); st.State != phase.Blocked || !strings.Contains(st.Reason, "providers differ") {
		t.Errorf("a version mismatch: %+v", st)
	}

	w = newWorld(t)
	_ = w.phases(t, false)
	for i := range w.installed[managementKC_] {
		w.installed[managementKC_][i].Fingerprint = ""
	}
	if st := statusOf(t, w.phases(t, false), "move"); st.State != phase.Manual || !strings.Contains(st.Reason, "providers adopt") {
		t.Errorf("an unstamped target: %+v", st)
	}

	other := theCluster(false)
	other.SetName("smoke")
	w = newWorld(t, other)
	if st := statusOf(t, w.phases(t, false), "move"); st.State != phase.Blocked || !strings.Contains(st.Reason, "other Clusters ([smoke])") {
		t.Errorf("another Cluster in the namespace: %+v", st)
	}
}

func TestDeleteBootstrapClusterWaitsForTheStableWindow(t *testing.T) {
	recent := metav1.NewTime(time.Now().Add(-2 * time.Minute)).Format(time.RFC3339)
	cl := theCluster(false)
	_ = unstructured.SetNestedSlice(cl.Object, []any{
		map[string]any{"type": "Available", "status": "True", "lastTransitionTime": recent},
		map[string]any{"type": "RemoteConnectionProbe", "status": "True", "lastTransitionTime": recent},
	}, "status", "conditions")
	machine := readyCluster("True", "True", 1)[2]
	w := &world{docker: running(t), kubeconfig: kubeconfigFile(t), management: newFake(cl, machine)}

	st := states(t, w.phases(t, false))["delete-bootstrap-cluster"]
	if st.State != phase.Manual || !strings.Contains(st.Reason, "of 10m0s") {
		t.Fatalf("2 minutes into the window: %+v, want manual", st)
	}
	ps := w.phases(t, false)
	for i, p := range ps {
		if p.Name() == "delete-bootstrap-cluster" {
			ps[i] = tracked{&deleteBootstrapPhase{cfg: pivotConfig(t, w.kubeconfig), loc: p.(tracked).loc, cluster: BootstrapCluster(pivotConfig(t, w.kubeconfig), w.docker), clients: w.clients, now: func() time.Time { return time.Now().Add(time.Hour) }}, p.(tracked).loc}
		}
	}
	if st := states(t, ps)["delete-bootstrap-cluster"]; st.State != phase.Next {
		t.Errorf("past the window: %+v, want next", st)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

// After the pivot, a certificate Cluster API renewed in the Management cluster's Secret
// reaches the kubeconfig file.
func TestKubeconfigFollowsTheManagementCluster(t *testing.T) {
	w := &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: newFake(theCluster(false), kubeconfigSecret("renewed\n"))}
	if st := statusOf(t, w.phases(t, false), "kubeconfig"); st.State != phase.Next {
		t.Fatalf("a renewed Secret: %+v, want next", st)
	}
	if err := phase.Run(t.Context(), w.phases(t, false), "kubeconfig", ioDiscard{}); err == nil {
		// The run continues past kubeconfig, which doesn't matter here.
		t.Log("ran")
	}
	if got, _ := os.ReadFile(w.kubeconfig); string(got) != "renewed\n" {
		t.Errorf("kubeconfig file %q, want the renewed one", got)
	}
}

// An expired or refused kubeconfig says how to renew it, not to remove it, and blocks
// even --first.
func TestKubeconfigThatCantBeUsed(t *testing.T) {
	crt, key := keyPair(t)
	b := &recovery.Bundle{CA: recovery.KeyPair{Cert: crt, Key: key}, Endpoint: recovery.Endpoint{Host: "10.0.0.1", Port: 6443}}
	expired, err := b.AdminKubeconfig("mgmt", time.Hour, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mgmt.kubeconfig")
	if err := os.WriteFile(path, expired, 0o600); err != nil {
		t.Fatal(err)
	}
	w := &world{docker: newFakeDocker(), kubeconfig: path}
	for _, first := range []bool{false, true} {
		st := statusOf(t, w.phases(t, first), "bootstrap-cluster")
		if st.State != phase.Blocked || !strings.Contains(st.Reason, "certificate expired") || !strings.Contains(st.Reason, "kubeconfig renew") || strings.Contains(st.Reason, "remove") {
			t.Errorf("an expired kubeconfig (--first %v): %+v", first, st)
		}
	}

	// A kubeconfig whose cluster refuses it.
	w = &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), refuse: true}
	if st := statusOf(t, w.phases(t, false), "bootstrap-cluster"); st.State != phase.Blocked || !strings.Contains(st.Reason, "refuses its credentials") {
		t.Errorf("a refused kubeconfig: %+v", st)
	}
}
