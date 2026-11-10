package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/breakglass"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/flux"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

const recoverManifests = `
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata: {name: mgmt, namespace: mgmt}
spec:
  controlPlaneRef: {apiGroup: controlplane.cluster.x-k8s.io, kind: K0sControlPlane, name: mgmt}
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: K0sControlPlane
metadata: {name: mgmt, namespace: mgmt}
spec:
  version: v1.36.5+k0s.0
  k0sConfigSpec:
    args: [--enable-worker, --disable-components=konnectivity-server]
    k0s:
      spec:
        api:
          extraArgs: {encryption-provider-config: /etc/k0s/encryption-config.yaml}
        storage: {type: kine, kine: {dataSource: postgres://in-the-manifest}}
        network: {podCIDR: 10.244.0.0/16, serviceCIDR: 10.96.0.0/12}
`

const recoveryYAML = `
recovery:
  profile: k0smotron-kine
  bundleFromEnv: BUNDLE
  encryptionKeyFromEnv: KEY
  kine: {datasourceFromEnv: DATASOURCE}
`

var recoveryEnv = map[string]string{
	"BUNDLE": `{"apiVersion":"selfhostctl/v1alpha1","kind":"RecoveryBundle","cluster":"mgmt/mgmt",
		"endpoint":{"host":"172.18.0.10","port":6443},
		"ca":{"cert":"ca-cert","key":"ca-key"},"serviceAccount":{"cert":"sa-pub","key":"sa-key"},
		"frontProxyCA":{"cert":"fp-cert","key":"fp-key"}}`,
	"KEY":        "a2V5",
	"DATASOURCE": "postgres://from-the-environment",
	"FLUX_KEY":   "identity",
}

// down is a world whose Management cluster's machines are gone: its kubeconfig
// doesn't answer, the Bootstrap cluster runs empty, and the datastore holds what the
// Break-glass controller will serve.
func down(t *testing.T) *world {
	t.Helper()
	kcp := obj(breakglass.K0sControlPlaneGVK, "mgmt", map[string]any{"spec": map[string]any{"version": "v1.36.5+k0s.0"}})
	child := obj(clusterGVK, "child", map[string]any{"spec": map[string]any{"paused": false}})
	child.SetNamespace("children")
	own := obj(clusterGVK, "kept", map[string]any{"spec": map[string]any{"paused": true}})
	own.SetNamespace("children")
	hook := func(kind, name string, labelled bool) ctrlclient.Object {
		meta := metav1.ObjectMeta{Name: name}
		if labelled {
			meta.Labels = map[string]string{providerLabel: "cluster-api"}
		}
		if kind == "validating" {
			return &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: meta}
		}
		return &admissionv1.MutatingWebhookConfiguration{ObjectMeta: meta}
	}
	root := obj(flux.KustomizationGVK, "flux-system", map[string]any{"spec": map[string]any{}})
	root.SetNamespace("flux-system")
	now := time.Now()
	return &world{
		docker:     running(t),
		bootstrap:  newFake(),
		kubeconfig: kubeconfigFile(t),
		breakGlass: newFake(theCluster(false), kcp, child, own, root,
			// The Break-glass controller's own leases, just renewed; the dead machine's,
			// silent for ten minutes.
			apiserverLease("apiserver-bg", "selfhostctl-mgmt-break-glass", now),
			nodeLease("k0s-ctrl-selfhostctl-mgmt-break-glass", now),
			apiserverLease("apiserver-old", "mgmt-cp-1", now.Add(-10*time.Minute)),
			nodeLease("mgmt-cp-1", now.Add(-10*time.Minute)),
			hook("validating", "capi-validating", true), hook("mutating", "capi-mutating", true), hook("validating", "someone-elses", false),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mgmt-old-1"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mgmt-old-2"}}),
		env: recoveryEnv,
	}
}

func apiserverLease(name, host string, renewed time.Time) *coordinationv1.Lease {
	t := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: name, Labels: map[string]string{
			apiserverIdentity: "kube-apiserver", "kubernetes.io/hostname": host}},
		Spec: coordinationv1.LeaseSpec{RenewTime: &t},
	}
}

func nodeLease(name string, renewed time.Time) *coordinationv1.Lease {
	t := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: name}, Spec: coordinationv1.LeaseSpec{RenewTime: &t}}
}

func TestRecoverLeases(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(t *testing.T, c ctrlclient.Client)
		state phase.State
		why   string
	}{
		{"every other lease silent", func(*testing.T, ctrlclient.Client) {}, phase.Done, "no other API server"},
		{"another API server renews its lease", func(t *testing.T, c ctrlclient.Client) {
			if err := c.Create(t.Context(), apiserverLease("apiserver-live", "mgmt-cp-2", time.Now().Add(-20*time.Second))); err != nil {
				t.Fatal(err)
			}
		}, phase.Blocked, "API server mgmt-cp-2 (20s ago)"},
		{"a kubelet renews its lease", func(t *testing.T, c ctrlclient.Client) {
			if err := c.Create(t.Context(), nodeLease("worker-1", time.Now().Add(-5*time.Second))); err != nil {
				t.Fatal(err)
			}
		}, phase.Blocked, "Lease kube-node-lease/worker-1"},
		{"the Break-glass controller's own isn't there yet", func(t *testing.T, c ctrlclient.Client) {
			if err := c.Delete(t.Context(), apiserverLease("apiserver-bg", "", time.Time{})); err != nil {
				t.Fatal(err)
			}
		}, phase.Next, "its own lease"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := down(t)
			w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{running: true}
			tt.edit(t, w.breakGlass)
			st := states(t, w.recoverPhases(t))["leases"]
			if st.State != tt.state || !strings.Contains(st.Reason, tt.why) {
				t.Errorf("leases: %v %q, want %v %q", st.State, st.Reason, tt.state, tt.why)
			}
		})
	}
}

// A live cluster stops the Recovery before it changes anything in the datastore.
func TestRecoverStopsAtALiveCluster(t *testing.T) {
	w := down(t)
	if err := w.breakGlass.Create(t.Context(), apiserverLease("apiserver-live", "mgmt-cp-2", time.Now())); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := phase.Run(t.Context(), w.recoverPhases(t, fluxYAML), "", &out)
	if !phase.IsStop(err) || !strings.Contains(err.Error(), "leases: blocked: the datastore shows a live cluster") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	vwc := &admissionv1.ValidatingWebhookConfigurationList{}
	_ = w.breakGlass.List(t.Context(), vwc)
	nodes := &corev1.NodeList{}
	_ = w.breakGlass.List(t.Context(), nodes)
	if len(vwc.Items) != 2 || len(nodes.Items) != 2 || len(w.moves) != 0 {
		t.Errorf("changed the datastore: %d webhooks, %d Nodes, moves %v", len(vwc.Items), len(nodes.Items), w.moves)
	}
}

// movingMover moves the Cluster object between the fake clusters.
type movingMover struct{ w *world }

func (m movingMover) Move(ctx context.Context, from, to []byte, ns string, dryRun bool) error {
	m.w.moves = append(m.w.moves, map[bool]string{true: "dry-run", false: "move"}[dryRun])
	if dryRun {
		return nil
	}
	src, err := m.w.clients(from)
	if err != nil {
		return err
	}
	dst, err := m.w.clients(to)
	if err != nil {
		return err
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := src.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: "mgmt"}, cl); err != nil {
		return err
	}
	moved := cl.DeepCopy()
	moved.SetResourceVersion("")
	_ = unstructured.SetNestedField(moved.Object, false, "spec", "paused")
	// What a move that stopped halfway already copied, it updates.
	if err := dst.Delete(ctx, moved); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := dst.Create(ctx, moved); err != nil {
		return err
	}
	return src.Delete(ctx, cl)
}

func (w *world) recoverPhases(t *testing.T, extra ...string) []phase.Phase {
	t.Helper()
	dir := t.TempDir()
	manifests := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(manifests, []byte(recoverManifests), 0o600); err != nil {
		t.Fatal(err)
	}
	path := w.kubeconfig
	if path == "" {
		path = filepath.Join(dir, "none.kubeconfig")
	}
	yaml := strings.Replace(testYAML, "/nonexistent/selfhostctl-test/mgmt.kubeconfig", path, 1)
	yaml = strings.Replace(yaml, "manifests: cluster.yaml", "manifests: "+manifests, 1)
	cfg, err := config.Parse(filepath.Join(dir, "t.yaml"), []byte(yaml+recoveryYAML+strings.Join(extra, "")))
	if err != nil {
		t.Fatal(err)
	}
	if w.installed == nil {
		stamped := installedFor(providers.FromConfig(&cfg.Providers))
		w.installed = map[string][]providers.Installed{bootstrapKC: stamped, breakGlassKC: stamped}
	}
	return RecoverPhases(cfg, Deps{
		Docker:            w.docker,
		NodesReady:        func(context.Context, []byte) (bool, error) { return true, nil },
		Providers:         fakeSides{w},
		Mover:             movingMover{w},
		Clients:           w.clients,
		Lookup:            func(k string) (string, bool) { v, ok := w.env[k]; return v, ok },
		Now:               time.Now,
		BreakGlassTimeout: time.Second,
	})
}

const fluxYAML = `
gitops:
  flux:
    credentialSecret: {name: flux-git, namespace: flux-system, fromEnv: FLUX_KEY, knownHosts: t.yaml}
`

func TestRecover(t *testing.T) {
	w := down(t)
	ctx := t.Context()
	var out bytes.Buffer
	if err := phase.Run(ctx, w.recoverPhases(t, fluxYAML), "", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	// The Break-glass controller ran at the manifest's version, with its files, and is
	// gone.
	if len(w.docker.created) != 1 {
		t.Fatalf("created %d containers", len(w.docker.created))
	}
	c := w.docker.created[0]
	if c.Name != "selfhostctl-mgmt-break-glass" || c.Config.Image != "docker.io/k0sproject/k0s:v1.36.5-k0s.0" || c.HostConfig.NetworkMode != "kind" {
		t.Errorf("break-glass container %s, %s, network %s", c.Name, c.Config.Image, c.HostConfig.NetworkMode)
	}
	if _, ok := w.docker.containers["selfhostctl-mgmt-break-glass"]; ok {
		t.Error("the Break-glass controller is still there")
	}

	// The webhooks: the providers' saved in the Bootstrap cluster, removed from the
	// datastore; another's left alone.
	cm := &corev1.ConfigMap{}
	if err := w.bootstrap.Get(ctx, ctrlclient.ObjectKey{Namespace: WebhooksNamespace, Name: WebhooksConfigMap}, cm); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Kind  string `json:"kind"`
		Items []struct {
			Kind     string            `json:"kind"`
			Metadata map[string]any    `json:"metadata"`
			Labels   map[string]string `json:"labels"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(cm.Data[WebhooksKey]), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range list.Items {
		names = append(names, i.Kind+"/"+i.Metadata["name"].(string))
		if _, ok := i.Metadata["resourceVersion"]; ok {
			t.Errorf("%s saved with its resourceVersion", i.Metadata["name"])
		}
	}
	slices.Sort(names)
	if list.Kind != "List" || !slices.Equal(names, []string{"MutatingWebhookConfiguration/capi-mutating", "ValidatingWebhookConfiguration/capi-validating"}) {
		t.Errorf("saved %s %v", list.Kind, names)
	}
	vwc := &admissionv1.ValidatingWebhookConfigurationList{}
	_ = w.breakGlass.List(ctx, vwc)
	if len(vwc.Items) != 1 || vwc.Items[0].Name != "someone-elses" {
		t.Errorf("left on the datastore: %v", vwc.Items)
	}

	// Flux suspended, the child paused and marked, the operator's own left alone.
	k := &unstructured.Unstructured{}
	k.SetGroupVersionKind(flux.KustomizationGVK)
	_ = w.breakGlass.Get(ctx, ctrlclient.ObjectKey{Namespace: "flux-system", Name: "flux-system"}, k)
	if s, _, _ := unstructured.NestedBool(k.Object, "spec", "suspend"); !s {
		t.Error("Flux isn't suspended")
	}
	for name, want := range map[string]string{"child": "true", "kept": ""} {
		cl := &unstructured.Unstructured{}
		cl.SetGroupVersionKind(clusterGVK)
		_ = w.breakGlass.Get(ctx, ctrlclient.ObjectKey{Namespace: "children", Name: name}, cl)
		if p, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); !p || cl.GetAnnotations()[PausedAnnotation] != want {
			t.Errorf("Cluster %s: paused %v, marker %q", name, p, cl.GetAnnotations()[PausedAnnotation])
		}
	}
	nodes := &corev1.NodeList{}
	_ = w.breakGlass.List(ctx, nodes)
	if len(nodes.Items) != 0 {
		t.Errorf("%d old Nodes left", len(nodes.Items))
	}

	// Moved after a dry run, unpaused in the Bootstrap cluster.
	if !slices.Equal(w.moves, []string{"dry-run", "move"}) {
		t.Errorf("moves %v", w.moves)
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := w.bootstrap.Get(ctx, ctrlclient.ObjectKey{Namespace: "mgmt", Name: "mgmt"}, cl); err != nil {
		t.Fatal(err)
	}

	// Again: everything is done, nothing runs.
	out.Reset()
	if err := phase.Run(ctx, w.recoverPhases(t, fluxYAML), "", &out); err != nil {
		t.Fatalf("second run: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "running") || len(w.moves) != 2 || len(w.docker.created) != 1 {
		t.Errorf("second run did something:\n%s", out.String())
	}
	for _, want := range []string{"outage: done (a Recovery is in progress)", "break-glass: done (moved)", "webhooks: done (moved)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("second run lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRecoverWritesTheBreakGlassFiles(t *testing.T) {
	w := down(t)
	ps := w.recoverPhases(t)
	var bg phase.Phase
	for _, p := range ps {
		if p.Name() == "break-glass" {
			bg = p
		}
	}
	if err := bg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	files := w.docker.containers["selfhostctl-mgmt-break-glass"].files
	for _, f := range []string{"/etc/k0s/k0s.yaml", "/etc/k0s/encryption-config.yaml", "/var/lib/k0s/pki/ca.key", "/var/lib/k0s/pki/sa.pub"} {
		if len(files[f]) == 0 {
			t.Errorf("no %s", f)
		}
	}
	if !strings.Contains(string(files["/etc/k0s/k0s.yaml"]), "postgres://from-the-environment") || !strings.Contains(string(files["/etc/k0s/k0s.yaml"]), "externalAddress: 172.18.0.10") {
		t.Errorf("k0s.yaml:\n%s", files["/etc/k0s/k0s.yaml"])
	}
	// kube-apiserver, not root, reads the encryption config; the keys stay root's.
	modes := w.docker.containers["selfhostctl-mgmt-break-glass"].modes
	for f, want := range map[string]int64{"/etc/k0s/encryption-config.yaml": 0o644, "/var/lib/k0s/pki/ca.key": 0o600, "/etc/k0s/k0s.yaml": 0o600} {
		if modes[f] != want {
			t.Errorf("%s: mode %o, want %o", f, modes[f], want)
		}
	}
}

// A stopped Break-glass controller is made again, from the current config.
func TestRecoverRemakesAStoppedBreakGlass(t *testing.T) {
	w := down(t)
	w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{image: "docker.io/k0sproject/k0s:v1.36.5-k0s.0"}
	var bg phase.Phase
	for _, p := range w.recoverPhases(t) {
		if p.Name() == "break-glass" {
			bg = p
		}
	}
	if err := bg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := w.docker.containers["selfhostctl-mgmt-break-glass"]
	if len(w.docker.created) != 1 || !c.running || len(c.files) == 0 {
		t.Errorf("created %d, running %v, %d files", len(w.docker.created), c.running, len(c.files))
	}
}

func TestRecoverBreakGlassAtTheLiveVersion(t *testing.T) {
	w := down(t)
	ctx := t.Context()
	kcp := &unstructured.Unstructured{}
	kcp.SetGroupVersionKind(breakglass.K0sControlPlaneGVK)
	_ = w.breakGlass.Get(ctx, ctrlclient.ObjectKey{Namespace: "mgmt", Name: "mgmt"}, kcp)
	_ = unstructured.SetNestedField(kcp.Object, "v1.37.0+k0s.0", "spec", "version")
	if err := w.breakGlass.Update(ctx, kcp); err != nil {
		t.Fatal(err)
	}
	var bg phase.Phase
	for _, p := range w.recoverPhases(t) {
		if p.Name() == "break-glass" {
			bg = p
		}
	}
	if err := bg.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var images []string
	for _, c := range w.docker.created {
		images = append(images, c.Config.Image)
	}
	if !slices.Equal(images, []string{"docker.io/k0sproject/k0s:v1.36.5-k0s.0", "docker.io/k0sproject/k0s:v1.37.0-k0s.0"}) {
		t.Errorf("created %v", images)
	}
	if st, _ := bg.Status(ctx); st.State != phase.Done {
		t.Errorf("after the recreate: %v %s", st.State, st.Reason)
	}

	// A container at the old version, found by a later run: Next, with why.
	w.docker.containers["selfhostctl-mgmt-break-glass"].image = "docker.io/k0sproject/k0s:v1.36.5-k0s.0"
	for _, p := range w.recoverPhases(t) {
		if p.Name() == "break-glass" {
			bg = p
		}
	}
	if st, _ := bg.Status(ctx); st.State != phase.Next || !strings.Contains(st.Reason, "live K0sControlPlane is at v1.37.0+k0s.0") {
		t.Errorf("old container: %v %s", st.State, st.Reason)
	}
}

func TestRecoverOutage(t *testing.T) {
	tests := []struct {
		name  string
		world func(t *testing.T) *world
		state phase.State
		why   string
	}{
		{"the Management cluster answers", func(t *testing.T) *world {
			return &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), management: newFake(theCluster(false))}
		}, phase.Blocked, "nothing to recover"},
		{"it answers, with a Break-glass controller left over", func(t *testing.T) *world {
			d := newFakeDocker()
			d.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{}
			return &world{docker: d, kubeconfig: kubeconfigFile(t), management: newFake(theCluster(false))}
		}, phase.Blocked, "docker rm -f -v selfhostctl-mgmt-break-glass"},
		{"it refuses the kubeconfig", func(t *testing.T) *world {
			return &world{docker: newFakeDocker(), kubeconfig: kubeconfigFile(t), refuse: true}
		}, phase.Blocked, "kubeconfig renew"},
		{"no kubeconfig", func(t *testing.T) *world { return &world{docker: newFakeDocker()} }, phase.Blocked, "no kubeconfig"},
		{"before the pivot", func(t *testing.T) *world {
			return &world{docker: running(t), bootstrap: newFake(theCluster(false)), kubeconfig: kubeconfigFile(t)}
		}, phase.Blocked, "`bootstrap`'s to finish"},
		{"it doesn't answer", down, phase.Done, "doesn't answer"},
		{"a Recovery in progress", func(t *testing.T) *world {
			w := down(t)
			w.bootstrap = newFake(theCluster(false), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: WebhooksNamespace, Name: WebhooksConfigMap}})
			return w
		}, phase.Done, "in progress"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := states(t, tt.world(t).recoverPhases(t))["outage"]
			if st.State != tt.state || !strings.Contains(st.Reason, tt.why) {
				t.Errorf("outage: %v %q, want %v %q", st.State, st.Reason, tt.state, tt.why)
			}
		})
	}
}

func TestRecoverHost(t *testing.T) {
	tests := []struct {
		name    string
		docker  func(d *fakeDocker)
		state   phase.State
		reasons []string
	}{
		{"enough", func(*fakeDocker) {}, phase.Done, nil},
		{"too little memory", func(d *fakeDocker) { d.memory = 2 << 30 }, phase.Blocked, []string{"Docker has 2.0 GiB", "at least 4 GiB"}},
		{"low inotify limits", func(d *fakeDocker) { d.inotify = "65536\n128\n" }, phase.Blocked,
			[]string{"sysctl -w fs.inotify.max_user_watches=524288 fs.inotify.max_user_instances=512"}},
		{"one low limit", func(d *fakeDocker) { d.inotify = "1048576\n128\n" }, phase.Blocked,
			[]string{"sysctl -w fs.inotify.max_user_instances=512"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := down(t)
			tt.docker(w.docker)
			st := states(t, w.recoverPhases(t))["host"]
			if st.State != tt.state {
				t.Errorf("host: %v %s", st.State, st.Reason)
			}
			for _, r := range tt.reasons {
				if !strings.Contains(st.Reason, r) {
					t.Errorf("reason %q lacks %q", st.Reason, r)
				}
			}
		})
	}
	// The limits are read inside the Bootstrap cluster's container.
	w := down(t)
	states(t, w.recoverPhases(t))
	if len(w.docker.execs) == 0 || !strings.HasPrefix(w.docker.execs[0], "selfhostctl-mgmt-bootstrap: sh -c") {
		t.Errorf("execs %v", w.docker.execs)
	}
}

func TestRecoverResumesASplitMove(t *testing.T) {
	w := down(t)
	ctx := t.Context()
	// A move that stopped halfway: the Cluster paused on both sides.
	w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{image: "docker.io/k0sproject/k0s:v1.36.5-k0s.0", running: true}
	w.bootstrap = newFake(theCluster(true), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: WebhooksNamespace, Name: WebhooksConfigMap}})
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	_ = w.breakGlass.Get(ctx, ctrlclient.ObjectKey{Namespace: "mgmt", Name: "mgmt"}, cl)
	_ = unstructured.SetNestedField(cl.Object, true, "spec", "paused")
	if err := w.breakGlass.Update(ctx, cl); err != nil {
		t.Fatal(err)
	}

	var move phase.Phase
	for _, p := range w.recoverPhases(t) {
		if p.Name() == "move-to-bootstrap" {
			move = p
		}
	}
	st, err := move.Status(ctx)
	if err != nil || st.State != phase.Next || !strings.Contains(st.Reason, "stopped halfway") {
		t.Fatalf("split: %v %s %v", st.State, st.Reason, err)
	}
	if err := move.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.moves, []string{"move"}) {
		t.Errorf("moves %v: a resumed move has no dry run", w.moves)
	}
	if err := w.breakGlass.Get(ctx, ctrlclient.ObjectKeyFromObject(cl), cl); !apierrors.IsNotFound(err) {
		t.Errorf("the Break-glass controller still holds the Cluster: %v", err)
	}
}

func TestBootstrapIsBlockedDuringARecovery(t *testing.T) {
	w := down(t)
	w.bootstrap = newFake(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: WebhooksNamespace, Name: WebhooksConfigMap}})
	for name, st := range states(t, w.phases(t, false)) {
		if st.State != phase.Blocked || !strings.Contains(st.Reason, "selfhostctl recover") {
			t.Errorf("%s: %v %s", name, st.State, st.Reason)
		}
	}
}

func TestRecoverySecrets(t *testing.T) {
	cfg := testConfig(t, recoveryYAML)
	_, err := RecoverySecrets(cfg, func(k string) (string, bool) {
		if k == "KEY" {
			return "", true
		}
		v, ok := recoveryEnv[k]
		return v, ok
	})
	if err == nil || !strings.Contains(err.Error(), "empty or unset: KEY") {
		t.Errorf("err %v", err)
	}
	s, err := RecoverySecrets(cfg, func(k string) (string, bool) { v, ok := recoveryEnv[k]; return v, ok })
	if err != nil || s.Bundle.Endpoint.Host != "172.18.0.10" || s.Datasource != "postgres://from-the-environment" {
		t.Errorf("%+v %v", s, err)
	}
}

func TestWarnings(t *testing.T) {
	w := down(t)
	ctx := t.Context()
	w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{running: true}
	ps := w.recoverPhases(t, fluxYAML)
	for _, name := range []string{"suspend-gitops", "pause-children"} {
		for _, p := range ps {
			if p.Name() == name {
				if err := p.Run(ctx); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	cfg, _ := config.Parse(filepath.Join(t.TempDir(), "t.yaml"), []byte(strings.Replace(testYAML, "/nonexistent/selfhostctl-test/mgmt.kubeconfig", w.kubeconfig, 1)+fluxYAML))
	got, err := Warnings(ctx, cfg, Deps{Docker: w.docker, Clients: w.clients})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Kustomization flux-system/flux-system is suspended", "Cluster children/child is paused", "Cluster children/kept is paused"}
	if !slices.Equal(got, want) {
		t.Errorf("warnings %v\nwant %v", got, want)
	}
}
