package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/apply"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
	"github.com/klconsultancy/selfhostctl/internal/kube"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/recovery"
)

// Clients turns a kubeconfig into a client; kube.Client outside tests, a fake in them.
type Clients func(kubeconfig []byte) (ctrlclient.Client, error)

// LiveClients is Deps.Clients against real clusters.
func LiveClients(kubeconfig []byte) (ctrlclient.Client, error) { return kube.Client(kubeconfig) }

var (
	clusterGVK     = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	machineListGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"}
)

// --- apply ---

// applyPhase applies the Management cluster's manifests to the Bootstrap cluster.
type applyPhase struct {
	cfg     *config.ManagementCluster
	cluster *k0scontainer.Cluster
	clients Clients
}

func (p *applyPhase) Name() string { return "apply" }

func (p *applyPhase) Status(ctx context.Context) (phase.Status, error) {
	kc, blocked, err := reach(ctx, p.cluster)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	objs, err := apply.Load(p.cfg.Path(p.cfg.Cluster.Manifests))
	if err != nil {
		return phase.Status{}, fmt.Errorf("cluster.manifests: %w", err)
	}
	c, err := p.clients(kc)
	if err != nil {
		return phase.Status{}, err
	}
	done, err := apply.InPlace(ctx, c, objs, apply.None)
	if err != nil {
		return phase.Status{}, err
	}
	if done {
		return phase.Status{State: phase.Done}, nil
	}
	return phase.Status{State: phase.Next}, nil
}

func (p *applyPhase) Run(ctx context.Context) error {
	kc, err := mustReach(ctx, p.cluster)
	if err != nil {
		return err
	}
	objs, err := apply.Load(p.cfg.Path(p.cfg.Cluster.Manifests))
	if err != nil {
		return fmt.Errorf("cluster.manifests: %w", err)
	}
	c, err := p.clients(kc)
	if err != nil {
		return err
	}
	return apply.Objects(ctx, c, objs, apply.None)
}

// --- ready ---

// readyPhase waits until the control plane is up and as many machines as it wants are
// healthy Nodes.
type readyPhase struct {
	cfg     *config.ManagementCluster
	cluster *k0scontainer.Cluster
	clients Clients
	timeout time.Duration
}

func (p *readyPhase) Name() string { return "ready" }

// check returns "" when the cluster is ready, else what it waits for.
func (p *readyPhase) check(ctx context.Context, c ctrlclient.Client) (string, error) {
	ns, name := p.cfg.Cluster.Namespace, p.cfg.ClusterName()
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("no Cluster %s/%s (phase apply)", ns, name), nil
		}
		return "", err
	}
	if !conditionTrue(cl, "ControlPlaneAvailable") {
		return fmt.Sprintf("Cluster %s/%s is not ControlPlaneAvailable yet", ns, name), nil
	}

	// The control plane: as many ready replicas as it wants.
	ref, _, _ := unstructured.NestedStringMap(cl.Object, "spec", "controlPlaneRef")
	mapping, err := c.RESTMapper().RESTMapping(schema.GroupKind{Group: ref["apiGroup"], Kind: ref["kind"]})
	if err != nil {
		return "", fmt.Errorf("control plane %s: %w", ref["kind"], err)
	}
	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(mapping.GroupVersionKind)
	if err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: ref["name"]}, cp); err != nil {
		return "", fmt.Errorf("control plane %s %s: %w", ref["kind"], ref["name"], err)
	}
	want, _, _ := unstructured.NestedInt64(cp.Object, "spec", "replicas")
	ready, _, _ := unstructured.NestedInt64(cp.Object, "status", "readyReplicas")
	if want == 0 || ready < want {
		return fmt.Sprintf("%s %s has %d of %d replicas ready", ref["kind"], ref["name"], ready, want), nil
	}

	// Every Machine of the Cluster is a healthy Node.
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineListGVK)
	if err := c.List(ctx, machines, ctrlclient.InNamespace(ns), ctrlclient.MatchingLabels{"cluster.x-k8s.io/cluster-name": name}); err != nil {
		return "", err
	}
	if len(machines.Items) == 0 {
		return "no Machines yet", nil
	}
	for i := range machines.Items {
		if !conditionTrue(&machines.Items[i], "NodeHealthy") {
			return fmt.Sprintf("Machine %s is not NodeHealthy yet", machines.Items[i].GetName()), nil
		}
	}
	return "", nil
}

func conditionTrue(u *unstructured.Unstructured, condition string) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == condition && m["status"] == "True" {
			return true
		}
	}
	return false
}

func (p *readyPhase) Status(ctx context.Context) (phase.Status, error) {
	kc, blocked, err := reach(ctx, p.cluster)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	c, err := p.clients(kc)
	if err != nil {
		return phase.Status{}, err
	}
	waiting, err := p.check(ctx, c)
	if err != nil {
		return phase.Status{}, err
	}
	if waiting != "" {
		return phase.Status{State: phase.Next, Reason: waiting}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *readyPhase) Run(ctx context.Context) error {
	kc, err := mustReach(ctx, p.cluster)
	if err != nil {
		return err
	}
	c, err := p.clients(kc)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(p.timeout)
	for {
		waiting, err := p.check(ctx, c)
		if err != nil {
			return err
		}
		if waiting == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s: %s", p.timeout, waiting)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// --- recovery-bundle ---

// recoveryBundlePhase is a Manual checkpoint: a bootstrap doesn't finish without a
// Recovery bundle in the operator's environment that matches the cluster, and the
// other inputs a Recovery reads.
type recoveryBundlePhase struct {
	cfg     *config.ManagementCluster
	cluster *k0scontainer.Cluster
	clients Clients
	lookup  func(string) (string, bool)
}

func (p *recoveryBundlePhase) Name() string { return "recovery-bundle" }

func (p *recoveryBundlePhase) Status(ctx context.Context) (phase.Status, error) {
	r := p.cfg.Recovery
	if r == nil {
		return phase.Status{State: phase.Done, Reason: "no recovery profile configured"}, nil
	}
	kc, blocked, err := reach(ctx, p.cluster)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	c, err := p.clients(kc)
	if err != nil {
		return phase.Status{}, err
	}
	live, err := recovery.Export(ctx, c, p.cfg.Cluster.Namespace, p.cfg.ClusterName())
	if err != nil {
		return phase.Status{State: phase.Blocked, Reason: "the cluster's Recovery bundle isn't readable yet: " + err.Error()}, nil
	}

	export := fmt.Sprintf("run `selfhostctl recovery export -f <config>` into your secret store, then run again with it in %s", r.BundleFromEnv)
	value, _ := p.lookup(r.BundleFromEnv)
	if value == "" {
		return phase.Status{State: phase.Manual, Reason: fmt.Sprintf("no Recovery bundle in %s, so the cluster could not be recovered: %s", r.BundleFromEnv, export)}, nil
	}
	b, err := recovery.Parse(value)
	if err != nil {
		return phase.Status{State: phase.Manual, Reason: fmt.Sprintf("%s: %v; %s", r.BundleFromEnv, err, export)}, nil
	}
	if diffs := b.Compare(live); len(diffs) > 0 {
		return phase.Status{State: phase.Manual, Reason: fmt.Sprintf("the Recovery bundle in %s doesn't match the cluster (%s): %s", r.BundleFromEnv, joinDiffs(diffs), export)}, nil
	}

	var missing []string
	for _, name := range []string{r.EncryptionKeyFromEnv, r.Kine.DatasourceFromEnv} {
		if v, _ := p.lookup(name); v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return phase.Status{State: phase.Manual, Reason: fmt.Sprintf("a Recovery also reads %s, which are unset or empty: put them in the environment and run again", joinDiffs(missing))}, nil
	}

	// The machines encrypt with what beforeApply substitutes; a Recovery with what
	// recovery.encryptionKeyFromEnv holds. Warn when no beforeApply file uses that one.
	refs, err := p.cfg.FileEnvRefs()
	if err != nil {
		return phase.Status{}, err
	}
	for _, ref := range refs {
		if ref == r.EncryptionKeyFromEnv {
			return phase.Status{State: phase.Done}, nil
		}
	}
	return phase.Status{State: phase.Done, Reason: fmt.Sprintf("warning: no beforeApply file uses ${%s}; check that the machines encrypt with the key a Recovery will read", r.EncryptionKeyFromEnv)}, nil
}

func (p *recoveryBundlePhase) Run(context.Context) error {
	return errors.New("recovery-bundle is a manual checkpoint")
}

func joinDiffs(d []string) string {
	var b bytes.Buffer
	for i, s := range d {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(s)
	}
	return b.String()
}

// --- kubeconfig ---

// kubeconfigPhase writes the Management cluster's admin kubeconfig, as Cluster API
// keeps it in the <cluster>-kubeconfig Secret, for the operator and as the tool's handle
// to the Management cluster. It runs after the pivot too: when Cluster API renews the
// certificate in the Secret, the file follows.
type kubeconfigPhase struct {
	cfg     *config.ManagementCluster
	cluster *k0scontainer.Cluster
	clients Clients
	// source finds the cluster that holds the Secret; the Bootstrap cluster when nil.
	source func(ctx context.Context) ([]byte, *phase.Status, error)
	now    func() time.Time
}

func (p *kubeconfigPhase) Name() string { return "kubeconfig" }

func (p *kubeconfigPhase) want(ctx context.Context) ([]byte, string, *phase.Status, error) {
	path, err := p.cfg.KubeconfigPath()
	if err != nil {
		return nil, "", nil, err
	}
	source := p.source
	if source == nil {
		source = func(ctx context.Context) ([]byte, *phase.Status, error) { return reach(ctx, p.cluster) }
	}
	kc, blocked, err := source(ctx)
	if err != nil || blocked != nil {
		return nil, path, blocked, err
	}
	c, err := p.clients(kc)
	if err != nil {
		return nil, path, nil, err
	}
	var s corev1.Secret
	key := ctrlclient.ObjectKey{Namespace: p.cfg.Cluster.Namespace, Name: p.cfg.ClusterName() + "-kubeconfig"}
	if err := c.Get(ctx, key, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, path, &phase.Status{State: phase.Blocked, Reason: fmt.Sprintf("no Secret %s yet (phase ready)", key)}, nil
		}
		return nil, path, nil, err
	}
	return s.Data["value"], path, nil, nil
}

func (p *kubeconfigPhase) Status(ctx context.Context) (phase.Status, error) {
	want, path, blocked, err := p.want(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	note := path
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	if n := recovery.ExpiryNote(want, now(), 30*24*time.Hour); n != "" {
		note += "; " + n
	}
	have, err := os.ReadFile(path)
	if err == nil && bytes.Equal(have, want) {
		return phase.Status{State: phase.Done, Reason: note}, nil
	}
	return phase.Status{State: phase.Next, Reason: note}, nil
}

func (p *kubeconfigPhase) Run(ctx context.Context) error {
	want, path, blocked, err := p.want(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
