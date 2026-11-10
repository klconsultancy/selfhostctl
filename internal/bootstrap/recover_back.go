package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/gitops"
	"github.com/klconsultancy/selfhostctl/internal/infrastructure"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// The second half of `recover` (docs/design.md#recovery): new machines on the
// surviving datastore, the move back, and the finish.

// MachinesKey is the recovery ConfigMap's key with the old Machines' names, recorded
// before the first is deleted.
const MachinesKey = "machines.json"

var controlNodeGVK = schema.GroupVersionKind{Group: "autopilot.k0sproject.io", Version: "v1beta2", Kind: "ControlNode"}

// Annotations that let CAPI delete a Machine whose node is gone: no drain, no wait
// for volumes. Its lifecycle hooks (k0smotron's pre-terminate kcp-cleanup among
// them) would ask the dead cluster, so they are removed.
const (
	excludeDraining      = "machine.cluster.x-k8s.io/exclude-node-draining"
	excludeVolumeDetach  = "machine.cluster.x-k8s.io/exclude-wait-for-node-volume-detach"
	preDrainHookPrefix   = "pre-drain.delete.hook.machine.cluster.x-k8s.io"
	preTerminateHookPref = "pre-terminate.delete.hook.machine.cluster.x-k8s.io"
)

// poll is how often the waits of the second half look.
const poll = 2 * time.Second

// movingBack: the objects are in the Management cluster, or on their way there.
func movingBack(s Situation) bool {
	return s.Location == LocationManagement || (s.Location == LocationSplit && !s.BreakGlassExists)
}

// beforeMoveBack is a phase on the Bootstrap cluster: done once the objects move back.
type beforeMoveBack struct {
	phase.Phase
	loc *locator
}

func (p beforeMoveBack) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if movingBack(s) {
		return phase.Status{State: phase.Done, Reason: "moved back"}, nil
	}
	return p.Phase.Status(ctx)
}

// bootstrapClient is a client on the Bootstrap cluster, which holds the objects.
func bootstrapClient(ctx context.Context, loc *locator, clients Clients) (ctrlclient.Client, *phase.Status, error) {
	s, err := loc.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	if s.Location != LocationBootstrap {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the objects aren't in the Bootstrap cluster yet (phase move-to-bootstrap)"}, nil
	}
	kc, blocked := bootstrapKubeconfig(s)
	if blocked != nil {
		return nil, blocked, nil
	}
	c, err := clients(kc)
	return c, nil, err
}

// --- the old Machines ---

func recoveryConfigMap(ctx context.Context, c ctrlclient.Client) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: WebhooksNamespace, Name: WebhooksConfigMap}, cm)
	return cm, err
}

// recordedMachines are the old Machines' names; ok is false before they are recorded.
func recordedMachines(ctx context.Context, boot ctrlclient.Client) (names []string, ok bool, err error) {
	cm, err := recoveryConfigMap(ctx, boot)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	data, ok := cm.Data[MachinesKey]
	if !ok {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(data), &names); err != nil {
		return nil, false, fmt.Errorf("%s/%s %s: %w", WebhooksNamespace, WebhooksConfigMap, MachinesKey, err)
	}
	return names, true, nil
}

// clusterMachines are the names of the Cluster's Machines now.
func clusterMachines(ctx context.Context, c ctrlclient.Client, cfg *config.ManagementCluster) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineListGVK)
	if err := c.List(ctx, list, ctrlclient.InNamespace(cfg.Cluster.Namespace), ctrlclient.MatchingLabels{"cluster.x-k8s.io/cluster-name": cfg.ClusterName()}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, m := range list.Items {
		names = append(names, m.GetName())
	}
	slices.Sort(names)
	return names, nil
}

// oldMachines are the recorded names, or, before they are recorded, the Cluster's
// Machines now: all of them are old until recover deletes the first.
func oldMachines(ctx context.Context, boot ctrlclient.Client, cfg *config.ManagementCluster) ([]string, error) {
	names, ok, err := recordedMachines(ctx, boot)
	if err != nil || ok {
		return names, err
	}
	return clusterMachines(ctx, boot, cfg)
}

// recordMachines records the old Machines once and returns them.
func recordMachines(ctx context.Context, boot ctrlclient.Client, cfg *config.ManagementCluster) ([]string, error) {
	names, ok, err := recordedMachines(ctx, boot)
	if err != nil || ok {
		return names, err
	}
	if names, err = clusterMachines(ctx, boot, cfg); err != nil {
		return nil, err
	}
	data, err := json.Marshal(names)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{}
	cm.SetNamespace(WebhooksNamespace)
	cm.SetName(WebhooksConfigMap)
	patch, err := json.Marshal(map[string]any{"data": map[string]string{MachinesKey: string(data)}})
	if err != nil {
		return nil, err
	}
	if err := boot.Patch(ctx, cm, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
		return nil, fmt.Errorf("recording the old Machines: %w", err)
	}
	return names, nil
}

// existingMachines are the named Machines still there.
func existingMachines(ctx context.Context, c ctrlclient.Client, ns string, names []string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, n := range names {
		m := &unstructured.Unstructured{}
		m.SetGroupVersionKind(machineListGVK.GroupVersion().WithKind("Machine"))
		if err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: n}, m); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// --- infrastructure ---

// infrastructurePhase is the Infrastructure adapter's turn, before the old Machines go.
type infrastructurePhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	clients Clients
	adapter infrastructure.Adapter
}

func (p *infrastructurePhase) Name() string { return "infrastructure" }

func (p *infrastructurePhase) machines(ctx context.Context) (infrastructure.Machines, *phase.Status, error) {
	c, blocked, err := bootstrapClient(ctx, p.loc, p.clients)
	if err != nil || blocked != nil {
		return infrastructure.Machines{}, blocked, err
	}
	names, err := oldMachines(ctx, c, p.cfg)
	if err != nil {
		return infrastructure.Machines{}, nil, err
	}
	left, err := existingMachines(ctx, c, p.cfg.Cluster.Namespace, names)
	if err != nil {
		return infrastructure.Machines{}, nil, err
	}
	m := infrastructure.Machines{Client: c, Namespace: p.cfg.Cluster.Namespace}
	for _, l := range left {
		m.Names = append(m.Names, l.GetName())
	}
	return m, nil, nil
}

func (p *infrastructurePhase) Status(ctx context.Context) (phase.Status, error) {
	m, blocked, err := p.machines(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	if len(m.Names) == 0 {
		return phase.Status{State: phase.Done, Reason: "no old Machines left"}, nil
	}
	return p.adapter.Status(ctx, m)
}

func (p *infrastructurePhase) Run(ctx context.Context) error {
	m, blocked, err := p.machines(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	return p.adapter.Run(ctx, m)
}

// --- replace-machines ---

// replaceMachinesPhase deletes the old Machines, so the control plane creates new ones
// that come up on the datastore.
type replaceMachinesPhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	clients Clients
	timeout time.Duration
}

func (p *replaceMachinesPhase) Name() string { return "replace-machines" }

func (p *replaceMachinesPhase) Status(ctx context.Context) (phase.Status, error) {
	c, blocked, err := bootstrapClient(ctx, p.loc, p.clients)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	names, ok, err := recordedMachines(ctx, c)
	if err != nil {
		return phase.Status{}, err
	}
	if !ok {
		return phase.Status{State: phase.Next, Reason: "records the old Machines, then deletes them"}, nil
	}
	left, err := existingMachines(ctx, c, p.cfg.Cluster.Namespace, names)
	if err != nil {
		return phase.Status{}, err
	}
	if len(left) > 0 {
		return phase.Status{State: phase.Next, Reason: fmt.Sprintf("%d of the %d old Machines left", len(left), len(names))}, nil
	}
	return phase.Status{State: phase.Done, Reason: fmt.Sprintf("the %d old Machines are gone", len(names))}, nil
}

func (p *replaceMachinesPhase) Run(ctx context.Context) error {
	c, blocked, err := bootstrapClient(ctx, p.loc, p.clients)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	names, err := recordMachines(ctx, c, p.cfg)
	if err != nil {
		return err
	}
	ns := p.cfg.Cluster.Namespace
	left, err := existingMachines(ctx, c, ns, names)
	if err != nil {
		return err
	}
	for _, m := range left {
		if err := deleteDeadMachine(ctx, c, m); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(p.timeout)
	for {
		left, err := existingMachines(ctx, c, ns, names)
		if err != nil || len(left) == 0 {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("old Machine %s still there after %s: see its status and finalizers", left[0].GetName(), p.timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// deleteDeadMachine deletes a Machine whose node is gone: nothing to drain, no hook
// to wait for.
func deleteDeadMachine(ctx context.Context, c ctrlclient.Client, m *unstructured.Unstructured) error {
	ann := map[string]any{excludeDraining: "true", excludeVolumeDetach: "true"}
	for k := range m.GetAnnotations() {
		if strings.HasPrefix(k, preDrainHookPrefix+"/") || strings.HasPrefix(k, preTerminateHookPref+"/") {
			ann[k] = nil
		}
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	if err != nil {
		return err
	}
	if err := c.Patch(ctx, m, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("annotating Machine %s: %w", m.GetName(), err)
	}
	if err := c.Delete(ctx, m); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting Machine %s: %w", m.GetName(), err)
	}
	return nil
}

// --- move-to-management ---

// moveToManagementPhase is the pivot's move, back into the Management cluster.
type moveToManagementPhase struct{ *movePhase }

func (p moveToManagementPhase) Name() string { return "move-to-management" }

func (p moveToManagementPhase) Status(ctx context.Context) (phase.Status, error) {
	st, err := p.movePhase.Status(ctx)
	if err == nil && st.State == phase.Blocked && strings.HasPrefix(st.Reason, "the providers differ") {
		st.Reason += ". Upgrade the Management cluster's providers to the config's first, then run again"
	}
	return st, err
}

// --- the finish, on the Management cluster ---

// managementClients are clients on both clusters once the objects are back.
func managementClients(ctx context.Context, loc *locator, clients Clients) (boot, mgmt ctrlclient.Client, blocked *phase.Status, err error) {
	s, err := loc.get(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if s.Location != LocationManagement || s.TargetPaused {
		return nil, nil, &phase.Status{State: phase.Blocked, Reason: "the objects aren't back in the Management cluster yet (phase move-to-management)"}, nil
	}
	if !s.BootstrapExists || s.BootstrapKC == nil {
		return nil, nil, &phase.Status{State: phase.Blocked, Reason: "the Bootstrap cluster, which holds what recover saved, can't be reached"}, nil
	}
	if boot, err = clients(s.BootstrapKC); err != nil {
		return nil, nil, nil, err
	}
	mgmt, err = clients(s.ManagementKC)
	return boot, mgmt, nil, err
}

// --- restore-webhooks ---

// restoreWebhooksPhase puts the saved webhook configurations back.
type restoreWebhooksPhase struct {
	loc     *locator
	clients Clients
}

func (p *restoreWebhooksPhase) Name() string { return "restore-webhooks" }

func (p *restoreWebhooksPhase) missing(ctx context.Context) (ctrlclient.Client, []unstructured.Unstructured, *phase.Status, error) {
	boot, mgmt, blocked, err := managementClients(ctx, p.loc, p.clients)
	if err != nil || blocked != nil {
		return nil, nil, blocked, err
	}
	cm, err := recoveryConfigMap(ctx, boot)
	if err != nil {
		return nil, nil, nil, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(cm.Data[WebhooksKey]), &list); err != nil {
		return nil, nil, nil, fmt.Errorf("the saved webhooks: %w", err)
	}
	var out []unstructured.Unstructured
	for _, item := range list.Items {
		u := unstructured.Unstructured{Object: item}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(u.GroupVersionKind())
		err := mgmt.Get(ctx, ctrlclient.ObjectKey{Name: u.GetName()}, live)
		if apierrors.IsNotFound(err) {
			out = append(out, u)
		} else if err != nil {
			return nil, nil, nil, err
		}
	}
	return mgmt, out, nil, nil
}

func (p *restoreWebhooksPhase) Status(ctx context.Context) (phase.Status, error) {
	_, missing, blocked, err := p.missing(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	if len(missing) > 0 {
		return phase.Status{State: phase.Next, Reason: fmt.Sprintf("%d to restore", len(missing))}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *restoreWebhooksPhase) Run(ctx context.Context) error {
	mgmt, missing, blocked, err := p.missing(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	for i := range missing {
		if err := mgmt.Create(ctx, &missing[i]); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("restoring %s %s: %w", missing[i].GetKind(), missing[i].GetName(), err)
		}
	}
	return nil
}

// --- leftovers ---

// leftoversPhase deletes what the Break-glass controller and the old machines left in
// the cluster: their autopilot ControlNodes and their leases in kube-node-lease, which
// k0smotron's kcp-cleanup hook would have deleted.
type leftoversPhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	clients Clients
}

func (p *leftoversPhase) Name() string { return "leftovers" }

func (p *leftoversPhase) found(ctx context.Context) (ctrlclient.Client, []ctrlclient.Object, *phase.Status, error) {
	boot, mgmt, blocked, err := managementClients(ctx, p.loc, p.clients)
	if err != nil || blocked != nil {
		return nil, nil, blocked, err
	}
	names, _, err := recordedMachines(ctx, boot)
	if err != nil {
		return nil, nil, nil, err
	}
	names = append([]string{BreakGlassName(p.cfg)}, names...)
	var out []ctrlclient.Object
	for _, n := range names {
		cn := &unstructured.Unstructured{}
		cn.SetGroupVersionKind(controlNodeGVK)
		switch err := mgmt.Get(ctx, ctrlclient.ObjectKey{Name: n}, cn); {
		case err == nil:
			out = append(out, cn)
		case !apierrors.IsNotFound(err) && !isNoMatch(err):
			return nil, nil, nil, err
		}
		for _, lease := range []string{"k0s-ctrl-" + n, n} {
			l := &coordinationv1.Lease{}
			switch err := mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: "kube-node-lease", Name: lease}, l); {
			case err == nil:
				out = append(out, l)
			case !apierrors.IsNotFound(err):
				return nil, nil, nil, err
			}
		}
	}
	return mgmt, out, nil, nil
}

func (p *leftoversPhase) Status(ctx context.Context) (phase.Status, error) {
	_, found, blocked, err := p.found(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	if len(found) > 0 {
		return phase.Status{State: phase.Next, Reason: fmt.Sprintf("%d to delete", len(found))}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *leftoversPhase) Run(ctx context.Context) error {
	mgmt, found, blocked, err := p.found(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	for _, o := range found {
		if err := mgmt.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting %s %s: %w", o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), err)
		}
	}
	return nil
}

// --- resume-gitops ---

// resumeGitOpsPhase resumes what suspend-gitops suspended, the root last.
type resumeGitOpsPhase struct {
	tool    gitops.Tool
	loc     *locator
	clients Clients
}

func (p *resumeGitOpsPhase) Name() string { return "resume-gitops" }

func (p *resumeGitOpsPhase) client(ctx context.Context) (ctrlclient.Client, *phase.Status, error) {
	_, mgmt, blocked, err := managementClients(ctx, p.loc, p.clients)
	return mgmt, blocked, err
}

func (p *resumeGitOpsPhase) Status(ctx context.Context) (phase.Status, error) {
	if p.tool == nil {
		return phase.Status{State: phase.Done, Reason: "no gitops"}, nil
	}
	c, blocked, err := p.client(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	marked, err := p.tool.Marked(ctx, c)
	if err != nil {
		return phase.Status{}, err
	}
	if len(marked) > 0 {
		return phase.Status{State: phase.Next, Reason: fmt.Sprintf("%d to resume", len(marked))}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *resumeGitOpsPhase) Run(ctx context.Context) error {
	c, blocked, err := p.client(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	_, err = p.tool.Resume(ctx, c)
	return err
}

// --- unpause-children ---

// unpauseChildrenPhase unpauses each Child cluster pause-children paused, once the
// Management cluster is healthy and the child's API answers with its Nodes Ready.
type unpauseChildrenPhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	clients Clients
	timeout time.Duration
	// waiting is set by Run when it gave up: what still keeps each child paused.
	waiting []string
}

func (p *unpauseChildrenPhase) Name() string { return "unpause-children" }

func (p *unpauseChildrenPhase) marked(ctx context.Context) (ctrlclient.Client, []byte, []unstructured.Unstructured, *phase.Status, error) {
	_, mgmt, blocked, err := managementClients(ctx, p.loc, p.clients)
	if err != nil || blocked != nil {
		return nil, nil, nil, blocked, err
	}
	s, err := p.loc.get(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterGVK.GroupVersion().WithKind("ClusterList"))
	if err := mgmt.List(ctx, list); err != nil {
		return nil, nil, nil, nil, err
	}
	var out []unstructured.Unstructured
	for _, cl := range list.Items {
		if _, ok := cl.GetAnnotations()[PausedAnnotation]; ok {
			out = append(out, cl)
		}
	}
	return mgmt, s.ManagementKC, out, nil, nil
}

func (p *unpauseChildrenPhase) Status(ctx context.Context) (phase.Status, error) {
	_, _, marked, blocked, err := p.marked(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	switch {
	case len(marked) == 0:
		return phase.Status{State: phase.Done}, nil
	case p.waiting != nil:
		return phase.Status{State: phase.Blocked, Reason: fmt.Sprintf("still paused after %s: %s. Run again once they answer", p.timeout, strings.Join(p.waiting, "; "))}, nil
	}
	return phase.Status{State: phase.Next, Reason: fmt.Sprintf("%d to unpause", len(marked))}, nil
}

func (p *unpauseChildrenPhase) Run(ctx context.Context) error {
	health := &deleteBootstrapPhase{cfg: p.cfg, clients: p.clients}
	deadline := time.Now().Add(p.timeout)
	for {
		mgmt, kc, marked, blocked, err := p.marked(ctx)
		if err != nil {
			return err
		}
		if blocked != nil {
			return errors.New(blocked.Reason)
		}
		if len(marked) == 0 {
			return nil
		}
		var waiting []string
		_, unhealthy, err := health.stableSince(ctx, kc)
		if err != nil {
			return err
		}
		if unhealthy != "" {
			waiting = []string{"the Management cluster isn't healthy: " + unhealthy}
		} else {
			for i := range marked {
				cl := &marked[i]
				why, err := p.childAnswers(ctx, mgmt, cl)
				if err != nil {
					return err
				}
				if why != "" {
					waiting = append(waiting, fmt.Sprintf("Cluster %s/%s: %s", cl.GetNamespace(), cl.GetName(), why))
					continue
				}
				patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:null}},"spec":{"paused":false}}`, PausedAnnotation)
				if err := mgmt.Patch(ctx, cl, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
					return fmt.Errorf("unpausing Cluster %s/%s: %w", cl.GetNamespace(), cl.GetName(), err)
				}
			}
			if len(waiting) == 0 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			p.waiting = waiting
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// childAnswers is "" when the child's API answers with all its Nodes Ready, else why
// not. It reaches the child with Cluster API's kubeconfig Secret for it.
func (p *unpauseChildrenPhase) childAnswers(ctx context.Context, mgmt ctrlclient.Client, cl *unstructured.Unstructured) (string, error) {
	sec := &corev1.Secret{}
	key := ctrlclient.ObjectKey{Namespace: cl.GetNamespace(), Name: cl.GetName() + "-kubeconfig"}
	if err := mgmt.Get(ctx, key, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "no Secret " + key.String(), nil
		}
		return "", err
	}
	c, err := p.clients(sec.Data["value"])
	if err != nil {
		return "its API doesn't answer: " + err.Error(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return "its API doesn't answer: " + err.Error(), nil
	}
	if len(nodes.Items) == 0 {
		return "no Nodes", nil
	}
	for _, n := range nodes.Items {
		if !nodeReady(n) {
			return "Node " + n.Name + " isn't Ready", nil
		}
	}
	return "", nil
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
