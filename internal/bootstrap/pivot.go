package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

// Mover moves a namespace's Cluster API objects between clusters;
// providers.Clusterctl outside tests.
type Mover interface {
	Move(ctx context.Context, from, to []byte, namespace string, dryRun bool) error
}

func isNoMatch(err error) bool { return meta.IsNoMatchError(err) }

const adoptInstruction = "run `selfhostctl providers adopt -f <config>` if they run with exactly the config's providers.variables, then run again"

// --- wrappers ---

// tracked re-locates after a phase ran: running is what moves the location.
type tracked struct {
	phase.Phase
	loc *locator
}

func (t tracked) Run(ctx context.Context) error {
	defer t.loc.invalidate()
	return t.Phase.Run(ctx)
}

// prePivot is a phase of the Bootstrap cluster: done once the objects have left it.
type prePivot struct {
	phase.Phase
	loc *locator
}

func (p prePivot) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	switch s.Location {
	case LocationManagement:
		return phase.Status{State: phase.Done, Reason: "pivoted"}, nil
	case LocationSplit:
		return phase.Status{State: phase.Done, Reason: "a move is in progress (phase move)"}, nil
	}
	return p.Phase.Status(ctx)
}

// guardedBootstrapCluster fails closed: a Bootstrap cluster is only created when
// locate proved there is no Management cluster, or the operator says so with --first.
type guardedBootstrapCluster struct {
	phase.Phase
	loc   *locator
	first bool
}

func (g guardedBootstrapCluster) Status(ctx context.Context) (phase.Status, error) {
	s, err := g.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	switch {
	case s.Location == LocationManagement:
		return phase.Status{State: phase.Done, Reason: "pivoted"}, nil
	case s.Location == LocationUnknown && (!g.first || s.Unanswered):
		return phase.Status{State: phase.Blocked, Reason: s.Reason}, nil
	case s.Location == LocationUnknown && !s.BootstrapExists:
		if ep := g.loc.knownEndpoint(); ep != "" && g.loc.probe(ctx, ep) {
			return phase.Status{State: phase.Blocked, Reason: fmt.Sprintf("something answers TLS at %s, the Management cluster's endpoint, so --first is refused: a Management cluster may run there", ep)}, nil
		}
	}
	return g.Phase.Status(ctx)
}

// --- the Management cluster's own providers and prerequisites ---

func managementKC(s Situation, ready string) ([]byte, *phase.Status) {
	switch {
	case s.Location == LocationNone || s.Location == LocationUnknown:
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Management cluster isn't up yet (phase " + ready + ")"}
	case s.ManagementKC == nil:
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Management cluster can't be reached through its kubeconfig (phase kubeconfig)"}
	}
	return s.ManagementKC, nil
}

// managementProvidersPhase installs the declared providers on the Management cluster.
type managementProvidersPhase struct {
	loc       *locator
	wanted    providers.Wanted
	installer providers.Installer
}

func (p *managementProvidersPhase) Name() string { return "management-providers" }

func (p *managementProvidersPhase) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	kc, blocked := managementKC(s, "ready")
	if blocked != nil {
		return *blocked, nil
	}
	installed, err := p.installer.Installed(ctx, kc)
	if err != nil {
		return phase.Status{}, err
	}
	m, diffs := providers.Compare(p.wanted, installed)
	switch {
	case m == providers.None:
		return phase.Status{State: phase.Next}, nil
	case m == providers.Exact:
		return phase.Status{State: phase.Done}, nil
	case providers.Unstamped(p.wanted, installed):
		return phase.Status{State: phase.Manual, Reason: "the Management cluster's providers carry no selfhostctl fingerprint: " + adoptInstruction}, nil
	}
	return phase.Status{State: phase.Blocked, Reason: "the Management cluster's providers differ from the config: " + providers.Summary(diffs) + ". Bring them to the config's versions first"}, nil
}

func (p *managementProvidersPhase) Run(ctx context.Context) error {
	s, err := p.loc.get(ctx)
	if err != nil {
		return err
	}
	kc, blocked := managementKC(s, "ready")
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	return p.installer.Init(ctx, kc, p.wanted)
}

// prerequisitesPhase installs prerequisites on a cluster that kc finds.
type prerequisitesPhase struct {
	name    string
	entries []config.Prerequisite
	prereqs Prerequisites
	// kc finds the cluster, or why the phase is blocked.
	kc func(ctx context.Context) ([]byte, *phase.Status, error)
}

func (p *prerequisitesPhase) Name() string { return p.name }

func (p *prerequisitesPhase) Status(ctx context.Context) (phase.Status, error) {
	if len(p.entries) == 0 {
		return phase.Status{State: phase.Done}, nil
	}
	kc, blocked, err := p.kc(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	next := false
	for _, e := range p.entries {
		st, err := p.prereqs.State(ctx, kc, e)
		if err != nil {
			return phase.Status{}, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if st.Blocked != "" {
			return phase.Status{State: phase.Blocked, Reason: e.Name() + ": " + st.Blocked}, nil
		}
		next = next || !st.Done
	}
	if next {
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *prerequisitesPhase) Run(ctx context.Context) error {
	kc, blocked, err := p.kc(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	for _, e := range p.entries {
		st, err := p.prereqs.State(ctx, kc, e)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		if st.Done {
			continue
		}
		if err := p.prereqs.Apply(ctx, kc, e); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return nil
}

// --- move ---

// movePhase moves the Management cluster's namespace from the Bootstrap cluster into
// the Management cluster, behind the providers-match and namespace gates.
type movePhase struct {
	cfg       *config.ManagementCluster
	loc       *locator
	installer providers.Installer
	mover     Mover
	clients   Clients
}

func (p *movePhase) Name() string { return "move" }

// gates are the preconditions of every move from source to target.
func (p *movePhase) gates(ctx context.Context, source, target []byte) (*phase.Status, error) {
	src, err := p.installer.Installed(ctx, source)
	if err != nil {
		return nil, err
	}
	dst, err := p.installer.Installed(ctx, target)
	if err != nil {
		return nil, err
	}
	diffs, unstamped := providers.Between(src, dst)
	if len(diffs) > 0 {
		return &phase.Status{State: phase.Blocked, Reason: "the providers differ between the clusters: " + providers.Summary(diffs)}, nil
	}
	if len(unstamped) > 0 {
		return &phase.Status{State: phase.Manual, Reason: fmt.Sprintf("providers without a selfhostctl fingerprint (%s): %s", providers.Summary(unstamped), adoptInstruction)}, nil
	}

	c, err := p.clients(source)
	if err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterGVK.GroupVersion().WithKind("ClusterList"))
	if err := c.List(ctx, list, ctrlclient.InNamespace(p.cfg.Cluster.Namespace)); err != nil {
		return nil, err
	}
	var others []string
	for _, cl := range list.Items {
		if cl.GetName() != p.cfg.ClusterName() {
			others = append(others, cl.GetName())
		}
	}
	if len(others) > 0 {
		slices.Sort(others)
		return &phase.Status{State: phase.Blocked, Reason: fmt.Sprintf("namespace %s holds other Clusters (%v): a move covers the Management cluster's namespace only, so they would move with it", p.cfg.Cluster.Namespace, others)}, nil
	}
	return nil, nil
}

func (p *movePhase) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	switch s.Location {
	case LocationManagement:
		if s.TargetPaused {
			return phase.Status{State: phase.Next, Reason: "moved, but the Cluster is still paused"}, nil
		}
		return phase.Status{State: phase.Done}, nil
	case LocationSplit:
		return phase.Status{State: phase.Next, Reason: "a move stopped halfway; running resumes it"}, nil
	case LocationBootstrap:
		kc, blocked := managementKC(s, "kubeconfig")
		if blocked != nil {
			return *blocked, nil
		}
		gate, err := p.gates(ctx, s.BootstrapKC, kc)
		if err != nil || gate != nil {
			return deref(gate), err
		}
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Blocked, Reason: "the Cluster isn't in the Bootstrap cluster yet (phase apply)"}, nil
}

func (p *movePhase) setPaused(ctx context.Context, kc []byte, paused bool) error {
	c, err := p.clients(kc)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(clusterGVK)
	u.SetNamespace(p.cfg.Cluster.Namespace)
	u.SetName(p.cfg.ClusterName())
	patch := ctrlclient.RawPatch("application/merge-patch+json", fmt.Appendf(nil, `{"spec":{"paused":%t}}`, paused))
	return c.Patch(ctx, u, patch)
}

func (p *movePhase) Run(ctx context.Context) error {
	s, err := p.loc.get(ctx)
	if err != nil {
		return err
	}
	ns := p.cfg.Cluster.Namespace
	switch s.Location {
	case LocationManagement:
		// The objects arrived, but the move ended before it unpaused them.
		return p.setPaused(ctx, s.ManagementKC, false)
	case LocationSplit:
		if s.BootstrapKC == nil || s.ManagementKC == nil {
			return errors.New("a move stopped halfway, and one of the clusters can't be reached")
		}
		// Unpause the source so the move runs again; it updates what already arrived.
		if err := p.setPaused(ctx, s.BootstrapKC, false); err != nil {
			return err
		}
		return p.mover.Move(ctx, s.BootstrapKC, s.ManagementKC, ns, false)
	case LocationBootstrap:
		if err := p.mover.Move(ctx, s.BootstrapKC, s.ManagementKC, ns, true); err != nil {
			return fmt.Errorf("dry run: %w", err)
		}
		return p.mover.Move(ctx, s.BootstrapKC, s.ManagementKC, ns, false)
	}
	return fmt.Errorf("nothing to move at location %s", s.Location)
}

// --- delete-bootstrap-cluster ---

// deleteBootstrapPhase deletes the Bootstrap cluster once the Management cluster has
// managed itself for the stable window.
type deleteBootstrapPhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	cluster *k0scontainer.Cluster
	clients Clients
	now     func() time.Time
}

func (p *deleteBootstrapPhase) Name() string { return "delete-bootstrap-cluster" }

// stableSince is when the Management cluster last changed: the latest of its
// Available condition's transition and its newest Machine. Zero with a reason while it
// isn't healthy.
func (p *deleteBootstrapPhase) stableSince(ctx context.Context, kc []byte) (time.Time, string, error) {
	c, err := p.clients(kc)
	if err != nil {
		return time.Time{}, "", err
	}
	ns, name := p.cfg.Cluster.Namespace, p.cfg.ClusterName()
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		return time.Time{}, "", err
	}
	var since time.Time
	for _, cond := range []string{"Available", "RemoteConnectionProbe"} {
		t, ok := conditionSince(cl, cond)
		if !ok {
			return time.Time{}, fmt.Sprintf("the Management cluster's Cluster isn't %s", cond), nil
		}
		if t.After(since) {
			since = t
		}
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineListGVK)
	if err := c.List(ctx, machines, ctrlclient.InNamespace(ns), ctrlclient.MatchingLabels{"cluster.x-k8s.io/cluster-name": name}); err != nil {
		return time.Time{}, "", err
	}
	if len(machines.Items) == 0 {
		return time.Time{}, "the Management cluster has no Machines", nil
	}
	for i := range machines.Items {
		m := &machines.Items[i]
		if !conditionTrue(m, "NodeHealthy") {
			return time.Time{}, fmt.Sprintf("Machine %s isn't NodeHealthy", m.GetName()), nil
		}
		if t := m.GetCreationTimestamp().Time; t.After(since) {
			since = t
		}
	}
	return since, "", nil
}

// conditionSince returns when a True condition last turned True.
func conditionSince(u *unstructured.Unstructured, condition string) (time.Time, bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] != condition || m["status"] != "True" {
			continue
		}
		s, _ := m["lastTransitionTime"].(string)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, true
		}
		return t, true
	}
	return time.Time{}, false
}

func (p *deleteBootstrapPhase) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if !s.BootstrapExists {
		return phase.Status{State: phase.Done}, nil
	}
	if s.Location != LocationManagement || s.TargetPaused {
		return phase.Status{State: phase.Blocked, Reason: "the Management cluster doesn't manage itself yet (phase move)"}, nil
	}
	since, unhealthy, err := p.stableSince(ctx, s.ManagementKC)
	if err != nil {
		return phase.Status{}, err
	}
	window := p.cfg.BootstrapCluster.StableWindow()
	if unhealthy != "" {
		return phase.Status{State: phase.Manual, Reason: fmt.Sprintf("the Bootstrap cluster stays: %s; run again once it is", unhealthy)}, nil
	}
	if stable := p.now().Sub(since); stable < window {
		return phase.Status{State: phase.Manual, Reason: fmt.Sprintf("the Bootstrap cluster stays: the Management cluster has been stable for %s of %s; run again then", stable.Round(time.Second), window)}, nil
	}
	return phase.Status{State: phase.Next}, nil
}

func (p *deleteBootstrapPhase) Run(ctx context.Context) error {
	return p.cluster.Remove(ctx)
}
