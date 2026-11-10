package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	kcp := obj(breakglass.K0sControlPlaneGVK, "mgmt", map[string]any{
		"spec":   map[string]any{"version": "v1.36.5+k0s.0", "replicas": int64(1)},
		"status": map[string]any{"readyReplicas": int64(1)},
	})
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
	lease := func(name string) *coordinationv1.Lease {
		return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: name}}
	}
	controlNode := func(name string) *unstructured.Unstructured {
		u := obj(controlNodeGVK, name, map[string]any{})
		u.SetNamespace("")
		return u
	}
	cluster := theCluster(false, "ControlPlaneAvailable", "True", "Available", "True", "RemoteConnectionProbe", "True")
	_ = unstructured.SetNestedStringMap(cluster.Object, map[string]string{
		"apiGroup": "controlplane.cluster.x-k8s.io", "kind": "K0sControlPlane", "name": "mgmt"}, "spec", "controlPlaneRef")
	w := &world{
		docker:     running(t),
		kubeconfig: kubeconfigFile(t),
		breakGlass: newFake(cluster, kcp, child, own, root,
			oldMachine("mgmt-old-1"), oldMachine("mgmt-old-2"),
			hook("validating", "capi-validating", true), hook("mutating", "capi-mutating", true), hook("validating", "someone-elses", false),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mgmt-old-1"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mgmt-old-2"}},
			// What the old machines and the Break-glass controller leave behind; and
			// another's lease.
			lease("k0s-ctrl-mgmt-old-1"), lease("mgmt-old-1"), lease("k0s-ctrl-selfhostctl-mgmt-break-glass"), lease("someone-elses"),
			controlNode("mgmt-old-1"), controlNode("selfhostctl-mgmt-break-glass"),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "children", Name: "child-kubeconfig"}, Data: map[string][]byte{"value": []byte(childKC)}}),
		child: newFake(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "child-1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}),
		env:   recoveryEnv,
	}
	w.bootstrap = kcpSim{Client: newFake(), w: w}
	return w
}

// childKC is the Child cluster's kubeconfig, as its Secret holds it.
const childKC = "child\n"

func oldMachine(name string) *unstructured.Unstructured {
	m := obj(machineListGVK.GroupVersion().WithKind("Machine"), name, map[string]any{})
	m.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "mgmt"})
	m.SetAnnotations(map[string]string{preTerminateHookPref + "/kcp-cleanup": ""})
	return m
}

// kcpSim is the Bootstrap cluster with k0smotron's control plane in it: an old
// Machine deleted, it creates a new one, which comes up on the datastore. The
// Management cluster then answers, on the Break-glass controller's datastore.
type kcpSim struct {
	ctrlclient.Client
	w *world
}

func (k kcpSim) Delete(ctx context.Context, o ctrlclient.Object, opts ...ctrlclient.DeleteOption) error {
	u, old := o.(*unstructured.Unstructured)
	old = old && u.GetKind() == "Machine" && strings.HasPrefix(u.GetName(), "mgmt-old-")
	live := &unstructured.Unstructured{}
	if old {
		live.SetGroupVersionKind(u.GroupVersionKind())
		_ = k.Get(ctx, ctrlclient.ObjectKeyFromObject(u), live)
	}
	if err := k.Client.Delete(ctx, o, opts...); err != nil {
		return err
	}
	if old {
		// What it was deleted with: the annotations that let CAPI delete it.
		k.w.deleted = append(k.w.deleted, u.GetName()+" "+fmt.Sprint(live.GetAnnotations()))
		m := obj(machineListGVK.GroupVersion().WithKind("Machine"), strings.Replace(u.GetName(), "old", "new", 1),
			map[string]any{"status": map[string]any{"conditions": conditions("NodeHealthy", "True")}})
		m.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "mgmt"})
		if err := k.Create(ctx, m); err != nil {
			return err
		}
		k.w.management = k.w.breakGlass
	}
	return nil
}

func TestRecoverDatastore(t *testing.T) {
	tests := []struct {
		name       string
		writes     int64
		breakGlass bool
		state      phase.State
		why        string
	}{
		{"nothing writes to it", 0, false, phase.Done, "nothing wrote to it"},
		{"something writes to it", 3, false, phase.Blocked, "the datastore shows a live cluster: 3 writes"},
		// The Break-glass controller writes to it itself: it isn't watched again.
		{"the Break-glass controller runs on it", 3, true, phase.Done, "already ran on it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := down(t)
			w.writes = tt.writes
			if tt.breakGlass {
				w.docker.containers["selfhostctl-mgmt-break-glass"] = &fakeContainer{running: true}
			}
			var ds phase.Phase
			for _, p := range w.recoverPhases(t) {
				if p.Name() == "datastore" {
					ds = p
				}
			}
			if st, err := ds.Status(t.Context()); err != nil || st.State != phase.Next && !tt.breakGlass {
				t.Fatalf("before watching: %v %v", st, err)
			}
			if err := ds.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			st, err := ds.Status(t.Context())
			if err != nil || st.State != tt.state || !strings.Contains(st.Reason, tt.why) {
				t.Errorf("datastore: %v %q (%v), want %v %q", st.State, st.Reason, err, tt.state, tt.why)
			}
		})
	}
}

// A live cluster stops the Recovery before anything starts on its datastore.
func TestRecoverStopsAtALiveCluster(t *testing.T) {
	w := down(t)
	w.writes = 1
	var out bytes.Buffer
	err := phase.Run(t.Context(), w.recoverPhases(t, fluxYAML), "", &out)
	if !phase.IsStop(err) || !strings.Contains(err.Error(), "datastore: blocked: the datastore shows a live cluster") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	if len(w.docker.created) != 0 || w.docker.containers["selfhostctl-mgmt-break-glass"] != nil {
		t.Errorf("started something: created %v", w.docker.created)
	}
	if w.dataSource != "postgres://from-the-environment" {
		t.Errorf("watched %q", w.dataSource)
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
	objs := []*unstructured.Unstructured{cl}
	for _, gvk := range []schema.GroupVersionKind{breakglass.K0sControlPlaneGVK, machineListGVK.GroupVersion().WithKind("Machine")} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		if err := src.List(ctx, list, ctrlclient.InNamespace(ns)); err != nil {
			return err
		}
		for i := range list.Items {
			objs = append(objs, &list.Items[i])
		}
	}
	for _, o := range objs {
		moved := o.DeepCopy()
		moved.SetResourceVersion("")
		if o == cl {
			_ = unstructured.SetNestedField(moved.Object, false, "spec", "paused")
		}
		// What a move that stopped halfway already copied, it updates.
		if err := dst.Delete(ctx, moved); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := dst.Create(ctx, moved); err != nil {
			return err
		}
	}
	for _, o := range objs {
		if err := src.Delete(ctx, o); err != nil {
			return err
		}
	}
	return nil
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
		w.installed = map[string][]providers.Installed{bootstrapKC: stamped, breakGlassKC: stamped, managementKC_: stamped}
	}
	return RecoverPhases(cfg, Deps{
		Docker:              w.docker,
		NodesReady:          func(context.Context, []byte) (bool, error) { return true, nil },
		Providers:           fakeSides{w},
		Mover:               movingMover{w},
		Clients:             w.clients,
		Lookup:              func(k string) (string, bool) { v, ok := w.env[k]; return v, ok },
		Now:                 w.now,
		BreakGlassTimeout:   time.Second,
		ClusterReadyTimeout: 10 * time.Millisecond,
		MachinesTimeout:     10 * time.Millisecond,
		ChildrenTimeout:     10 * time.Millisecond,
		Revision: func(_ context.Context, ds string) (int64, error) {
			w.dataSource = ds
			w.revision += w.writes
			return w.revision, nil
		},
		DatastoreQuiet: time.Millisecond,
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
	// The Management cluster is Available from now on: the stable window starts.
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := w.breakGlass.Get(ctx, ctrlclient.ObjectKey{Namespace: "mgmt", Name: "mgmt"}, cl); err != nil {
		t.Fatal(err)
	}
	conds := conditions("ControlPlaneAvailable", "True", "Available", "True", "RemoteConnectionProbe", "True")
	for _, c := range conds {
		c.(map[string]any)["lastTransitionTime"] = time.Now().Format(time.RFC3339)
	}
	_ = unstructured.SetNestedSlice(cl.Object, conds, "status", "conditions")
	if err := w.breakGlass.Update(ctx, cl); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := phase.Run(ctx, w.recoverPhases(t, fluxYAML), "", &out)
	var checkpoint *phase.CheckpointError
	if !errors.As(err, &checkpoint) || checkpoint.Phase != "delete-bootstrap-cluster" {
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
	if names, _, _ := recordedMachines(ctx, w.bootstrap); !slices.Equal(names, []string{"mgmt-old-1", "mgmt-old-2"}) {
		t.Errorf("recorded the old Machines %v", names)
	}

	// The old Machines were deleted without drain, volume wait or k0smotron's hook;
	// the new ones came up on the datastore, which is the Management cluster again.
	if len(w.deleted) != 2 {
		t.Fatalf("deleted %v", w.deleted)
	}
	for _, d := range w.deleted {
		if !strings.Contains(d, excludeDraining+":true") || !strings.Contains(d, excludeVolumeDetach+":true") || strings.Contains(d, "kcp-cleanup") {
			t.Errorf("deleted %s", d)
		}
	}
	mgmt := w.breakGlass
	if !slices.Equal(w.moves, []string{"dry-run", "move", "dry-run", "move"}) {
		t.Errorf("moves %v", w.moves)
	}
	machines, _ := clusterMachines(ctx, mgmt, &config.ManagementCluster{Cluster: config.Cluster{Namespace: "mgmt", Name: "mgmt"}})
	if !slices.Equal(machines, []string{"mgmt-new-1", "mgmt-new-2"}) {
		t.Errorf("the Management cluster's Machines: %v", machines)
	}

	// The finish: the webhooks back, the leftovers gone, Flux resumed, the child
	// unpaused; the operator's own left alone.
	vwc := &admissionv1.ValidatingWebhookConfigurationList{}
	_ = mgmt.List(ctx, vwc)
	mwc := &admissionv1.MutatingWebhookConfigurationList{}
	_ = mgmt.List(ctx, mwc)
	if len(vwc.Items) != 2 || len(mwc.Items) != 1 {
		t.Errorf("webhooks: %d validating, %d mutating", len(vwc.Items), len(mwc.Items))
	}
	leases := &coordinationv1.LeaseList{}
	_ = mgmt.List(ctx, leases)
	if len(leases.Items) != 1 || leases.Items[0].Name != "someone-elses" {
		t.Errorf("leases left: %v", leases.Items)
	}
	cns := &unstructured.UnstructuredList{}
	cns.SetGroupVersionKind(controlNodeGVK)
	_ = mgmt.List(ctx, cns)
	if len(cns.Items) != 0 {
		t.Errorf("%d ControlNodes left", len(cns.Items))
	}
	k := &unstructured.Unstructured{}
	k.SetGroupVersionKind(flux.KustomizationGVK)
	_ = mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: "flux-system", Name: "flux-system"}, k)
	if s, _, _ := unstructured.NestedBool(k.Object, "spec", "suspend"); s || k.GetAnnotations()[flux.SuspendedAnnotation] != "" {
		t.Error("Flux isn't resumed")
	}
	for name, want := range map[string]bool{"child": false, "kept": true} {
		cl := &unstructured.Unstructured{}
		cl.SetGroupVersionKind(clusterGVK)
		_ = mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: "children", Name: name}, cl)
		if p, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); p != want || cl.GetAnnotations()[PausedAnnotation] != "" {
			t.Errorf("Cluster %s: paused %v, marker %q", name, p, cl.GetAnnotations()[PausedAnnotation])
		}
	}
	// The Bootstrap cluster stays for the stable window; once it passed, the next run
	// deletes it and does nothing else.
	if _, ok := w.docker.containers["selfhostctl-mgmt-bootstrap"]; !ok {
		t.Fatal("the Bootstrap cluster went before the stable window passed")
	}
	w.clock = time.Now().Add(24 * time.Hour)
	out.Reset()
	if err := phase.Run(ctx, w.recoverPhases(t, fluxYAML), "", &out); err != nil {
		t.Fatalf("second run: %v\n%s", err, out.String())
	}
	if strings.Count(out.String(), "running") != 1 || !strings.Contains(out.String(), "delete-bootstrap-cluster: running") {
		t.Errorf("second run:\n%s", out.String())
	}
	if _, ok := w.docker.containers["selfhostctl-mgmt-bootstrap"]; ok {
		t.Error("the Bootstrap cluster is still there")
	}

	// Then there is nothing to recover.
	err = phase.Run(ctx, w.recoverPhases(t, fluxYAML), "", &out)
	if !phase.IsStop(err) || !strings.Contains(err.Error(), "outage: blocked: the Management cluster answers: nothing to recover") {
		t.Errorf("third run: %v", err)
	}
}

// The finish order is fixed: the webhooks before anything reconciles again, Flux
// before the children, the Bootstrap cluster last.
func TestRecoverPhaseOrder(t *testing.T) {
	var names []string
	for _, p := range down(t).recoverPhases(t) {
		names = append(names, p.Name())
	}
	want := []string{"outage", "datastore", "bootstrap-cluster", "host", "providers", "before-apply",
		"break-glass", "webhooks", "suspend-gitops", "pause-children", "delete-nodes", "move-to-bootstrap", "delete-break-glass",
		"infrastructure", "replace-machines", "ready", "move-to-management",
		"restore-webhooks", "leftovers", "resume-gitops", "unpause-children", "delete-bootstrap-cluster"}
	if !slices.Equal(names, want) {
		t.Errorf("phases %v", names)
	}
}

// A child stays paused until its API answers with its Nodes Ready; a later run
// unpauses it.
func TestRecoverUnpausesAChildOnceItAnswers(t *testing.T) {
	w := down(t)
	child := w.child
	w.child = nil
	ctx := t.Context()
	var out bytes.Buffer
	err := phase.Run(ctx, w.recoverPhases(t, fluxYAML), "", &out)
	if !phase.IsStop(err) || !strings.Contains(err.Error(), "unpause-children: blocked") || !strings.Contains(err.Error(), "Cluster children/child: its API doesn't answer") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	paused := func() (bool, string) {
		cl := &unstructured.Unstructured{}
		cl.SetGroupVersionKind(clusterGVK)
		_ = w.breakGlass.Get(ctx, ctrlclient.ObjectKey{Namespace: "children", Name: "child"}, cl)
		p, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused")
		return p, cl.GetAnnotations()[PausedAnnotation]
	}
	if p, marker := paused(); !p || marker != "true" {
		t.Errorf("the child: paused %v, marker %q", p, marker)
	}

	w.child = child
	out.Reset()
	_ = phase.Run(ctx, w.recoverPhases(t, fluxYAML), "", &out)
	if p, marker := paused(); p || marker != "" {
		t.Errorf("the child, answering: paused %v, marker %q\n%s", p, marker, out.String())
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
	for f, want := range map[string]int64{"/etc/k0s/encryption-config.yaml": 0o644, "/var/lib/k0s/pki/ca.key": 0o600, "/etc/k0s/k0s.yaml": 0o600,
		"/var/lib/k0s/pki/ca.crt": 0o644, "/var/lib/k0s/pki/sa.pub": 0o644, "/var/lib/k0s/pki/front-proxy-ca.crt": 0o644, "/var/lib/k0s/pki/sa.key": 0o600} {
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
