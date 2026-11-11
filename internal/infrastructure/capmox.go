package infrastructure

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// Researched against CAPMOX v0.9.2-kl.1 (docs/design.md#recovery).
var (
	clusterGVK        = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	machineGVK        = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	proxmoxClusterGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha2", Kind: "ProxmoxCluster"}
	proxmoxMachineGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha2", Kind: "ProxmoxMachine"}
)

// capmox is the adapter for Cluster API Provider Proxmox. It asks Proxmox itself, with
// the ProxmoxCluster's own credentials.
type capmox struct {
	timeout time.Duration
	poll    time.Duration
}

// oldVM is an old Machine's ProxmoxMachine that was given a VM.
type oldVM struct {
	obj  *unstructured.Unstructured
	vmid int64
	// node is status.proxmoxNode: the node CAPMOX asks to delete the VM.
	node string
}

// capmoxView is what the adapter reads from the cluster.
type capmoxView struct {
	api      *proxmoxAPI
	apiURL   string
	secret   string
	cluster  *unstructured.Unstructured
	zoned    bool
	machines []oldVM
}

func blocked(format string, a ...any) *phase.Status {
	return &phase.Status{State: phase.Blocked, Reason: fmt.Sprintf(format, a...)}
}

func get(ctx context.Context, c ctrlclient.Client, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u, c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: name}, u)
}

// look reads the Cluster's ProxmoxCluster, its credentials and the old Machines'
// ProxmoxMachines with a VM.
func (capmox) look(ctx context.Context, m Machines) (*capmoxView, *phase.Status, error) {
	cluster, err := get(ctx, m.Client, clusterGVK, m.Namespace, m.Cluster)
	if err != nil {
		return nil, nil, err
	}
	kind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind")
	name, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "name")
	if kind != proxmoxClusterGVK.Kind {
		return nil, blocked("adapter capmox: the Cluster's infrastructure is a %q, not a ProxmoxCluster", kind), nil
	}
	pc, err := get(ctx, m.Client, proxmoxClusterGVK, m.Namespace, name)
	if err != nil {
		return nil, nil, err
	}
	zones, _, _ := unstructured.NestedSlice(pc.Object, "spec", "zoneConfig")
	ref, _, _ := unstructured.NestedStringMap(pc.Object, "spec", "credentialsRef")
	if ref["name"] == "" {
		return nil, blocked("ProxmoxCluster %s has no spec.credentialsRef: recover asks Proxmox with the cluster's own credentials, so give it a Secret", name), nil
	}
	ns := ref["namespace"]
	if ns == "" {
		ns = m.Namespace
	}
	secret := &corev1.Secret{}
	if err := m.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: ref["name"]}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, blocked("the Proxmox credentials Secret %s/%s isn't there", ns, ref["name"]), nil
		}
		return nil, nil, err
	}
	api, err := newProxmoxAPI(secret.Data)
	if err != nil {
		return nil, blocked("the Proxmox credentials Secret %s/%s: %v", ns, ref["name"], err), nil
	}
	v := &capmoxView{api: api, apiURL: string(secret.Data["url"]), secret: ns + "/" + ref["name"], cluster: cluster, zoned: len(zones) > 0}
	for _, n := range m.Names {
		vm, err := oldMachineVM(ctx, m.Client, m.Namespace, n)
		if err != nil {
			return nil, nil, err
		}
		if vm != nil {
			v.machines = append(v.machines, *vm)
		}
	}
	return v, nil, nil
}

// oldMachineVM is the Machine's ProxmoxMachine, or nil when it has none or no VM.
func oldMachineVM(ctx context.Context, c ctrlclient.Client, ns, name string) (*oldVM, error) {
	machine, err := get(ctx, c, machineGVK, ns, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	kind, _, _ := unstructured.NestedString(machine.Object, "spec", "infrastructureRef", "kind")
	ref, _, _ := unstructured.NestedString(machine.Object, "spec", "infrastructureRef", "name")
	if kind != proxmoxMachineGVK.Kind {
		return nil, nil
	}
	pm, err := get(ctx, c, proxmoxMachineGVK, ns, ref)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	vmid, ok := int64Field(pm, "spec", "virtualMachineID")
	if !ok {
		return nil, nil
	}
	node, _, _ := unstructured.NestedString(pm.Object, "status", "proxmoxNode")
	return &oldVM{obj: pm, vmid: vmid, node: node}, nil
}

func int64Field(u *unstructured.Unstructured, fields ...string) (int64, bool) {
	v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, fields...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// alive is the Blocked status for old VMs that Proxmox still has, or nil.
func (v *capmoxView) alive(ctx context.Context) (*phase.Status, error) {
	vms, err := v.api.vms(ctx)
	if err != nil {
		return v.unreachable(err), nil
	}
	byID := map[int64]proxmoxVM{}
	for _, vm := range vms {
		byID[vm.VMID] = vm
	}
	var still, taken []string
	for _, m := range v.machines {
		vm, ok := byID[m.vmid]
		switch {
		case !ok:
		case vm.Name == m.obj.GetName():
			still = append(still, fmt.Sprintf("VM %d (%s) on %s, %s", vm.VMID, vm.Name, vm.Node, vm.Status))
		default:
			taken = append(taken, fmt.Sprintf("VMID %d of ProxmoxMachine %s is now VM %q on %s", vm.VMID, m.obj.GetName(), vm.Name, vm.Node))
		}
	}
	var reasons []string
	if len(still) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"the old machines' VMs still exist: %s. Start them and the cluster may come back by itself; to recover, destroy them in Proxmox first",
			strings.Join(still, "; ")))
	}
	if len(taken) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%s: CAPMOX would take it for the old machine, and deleting that would destroy it. Resolve it by hand first",
			strings.Join(taken, "; ")))
	}
	if len(reasons) == 0 {
		return nil, nil
	}
	return &phase.Status{State: phase.Blocked, Reason: strings.Join(reasons, ". ")}, nil
}

// unreachable is the Blocked status for a failed Proxmox API call.
func (v *capmoxView) unreachable(err error) *phase.Status {
	if errors.As(err, new(*tls.CertificateVerificationError)) {
		return blocked("the Proxmox certificate at %s doesn't verify (%v): add its CA as root_ca to Secret %s, or set insecure: \"true\" there knowingly; the token it holds can destroy VMs", v.apiURL, err, v.secret)
	}
	return blocked("the Proxmox API at %s doesn't answer: %v", v.apiURL, err)
}

// stale are the old VMs whose node CAPMOX can't ask: none, or one not online.
func (v *capmoxView) stale(online []string) []oldVM {
	var out []oldVM
	for _, m := range v.machines {
		if !slices.Contains(online, m.node) {
			out = append(out, m)
		}
	}
	return out
}

// failureDomainsShown: the Cluster lists failure domains, which CAPMOX builds from
// the zones once the ProxmoxCluster is reconciled. Without them, a new Machine lands
// in the default zone.
func (v *capmoxView) failureDomainsShown() bool {
	if !v.zoned {
		return true
	}
	fd, ok, _ := unstructured.NestedFieldNoCopy(v.cluster.Object, "status", "failureDomains")
	if !ok {
		return false
	}
	switch f := fd.(type) {
	case []any:
		return len(f) > 0
	case map[string]any:
		return len(f) > 0
	}
	return false
}

// Gone blocks while Proxmox still has an old machine's VM, running or not: CAPMOX
// would find it and start it again once the objects move.
func (a capmox) Gone(ctx context.Context, m Machines) (phase.Status, error) {
	v, b, err := a.look(ctx, m)
	if err != nil || b != nil {
		return deref(b), err
	}
	if len(v.machines) == 0 {
		return phase.Status{State: phase.Done, Reason: "no old ProxmoxMachine has a VM"}, nil
	}
	if b, err := v.alive(ctx); err != nil || b != nil {
		return deref(b), err
	}
	return phase.Status{State: phase.Done, Reason: fmt.Sprintf("the old machines' VMs are gone from Proxmox (%d)", len(v.machines))}, nil
}

func (a capmox) Status(ctx context.Context, m Machines) (phase.Status, error) {
	v, b, err := a.look(ctx, m)
	if err != nil || b != nil {
		return deref(b), err
	}
	if len(v.machines) == 0 {
		return phase.Status{State: phase.Done, Reason: "no old ProxmoxMachine has a VM"}, nil
	}
	if b, err := v.alive(ctx); err != nil || b != nil {
		return deref(b), err
	}
	online, err := v.api.onlineNodes(ctx)
	if err != nil {
		return *v.unreachable(err), nil
	}
	stale := v.stale(online)
	if len(stale) > 0 && len(online) == 0 {
		return *blocked("no Proxmox node is online: CAPMOX asks one to delete an old machine's VM"), nil
	}
	var todo []string
	if len(stale) > 0 {
		todo = append(todo, fmt.Sprintf("points %d old ProxmoxMachines at node %s, which can tell their VMs are gone", len(stale), online[0]))
	}
	if !v.failureDomainsShown() {
		todo = append(todo, "waits for the Cluster's failure domains")
	}
	if len(todo) == 0 {
		return phase.Status{State: phase.Done, Reason: "the old VMs are gone, and an online node can tell"}, nil
	}
	return phase.Status{State: phase.Next, Reason: strings.Join(todo, ", then ")}, nil
}

func (a capmox) Run(ctx context.Context, m Machines) error {
	v, b, err := a.look(ctx, m)
	if err != nil {
		return err
	}
	if b != nil {
		return errors.New(b.Reason)
	}
	if b, err := v.alive(ctx); err != nil || b != nil {
		if b != nil {
			return errors.New(b.Reason)
		}
		return err
	}
	online, err := v.api.onlineNodes(ctx)
	if err != nil {
		return errors.New(v.unreachable(err).Reason)
	}
	if stale := v.stale(online); len(stale) > 0 {
		if len(online) == 0 {
			return errors.New("no Proxmox node is online: CAPMOX asks one to delete an old machine's VM")
		}
		for _, s := range stale {
			patch, err := json.Marshal(map[string]any{"status": map[string]any{"proxmoxNode": online[0]}})
			if err != nil {
				return err
			}
			if err := m.Client.Status().Patch(ctx, s.obj, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
				return fmt.Errorf("ProxmoxMachine %s: %w", s.obj.GetName(), err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	for !v.failureDomainsShown() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the Cluster shows no failure domains after %s: CAPMOX builds them from the ProxmoxCluster's zones once its IPAM is ready", a.timeout)
		case <-time.After(a.poll):
		}
		if v.cluster, err = get(ctx, m.Client, clusterGVK, m.Namespace, m.Cluster); err != nil {
			return err
		}
	}
	return nil
}

func deref(s *phase.Status) phase.Status {
	if s == nil {
		return phase.Status{}
	}
	return *s
}
