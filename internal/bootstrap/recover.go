package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/client"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/apply"
	"github.com/klconsultancy/selfhostctl/internal/breakglass"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/gitops"
	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/recovery"
)

// The saved webhooks (docs/design.md#recovery).
const (
	WebhooksNamespace = "kube-system"
	WebhooksConfigMap = "selfhostctl-recovery-webhooks"
	WebhooksKey       = "webhooks.json"
)

// PausedAnnotation marks a Child cluster's Cluster that `recover` paused.
const PausedAnnotation = "selfhostctl.io/paused"

// The host's minimums for a Recovery: the Bootstrap cluster and the Break-glass
// controller side by side.
const (
	minDockerMemory     = 4 << 30
	minInotifyWatches   = 524288
	minInotifyInstances = 512
)

// providerLabel marks the objects clusterctl installed for a provider.
const providerLabel = "cluster.x-k8s.io/provider"

var webhookGVKs = []schema.GroupVersionKind{
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"},
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingWebhookConfiguration"},
}

// BreakGlassName is the Break-glass controller's container for a Management cluster.
func BreakGlassName(cfg *config.ManagementCluster) string {
	return "selfhostctl-" + cfg.Metadata.Name + "-break-glass"
}

// breakGlassCluster is the Break-glass controller's container running c.
func breakGlassCluster(cfg *config.ManagementCluster, docker k0scontainer.Docker, c breakglass.Controller) *k0scontainer.Cluster {
	return &k0scontainer.Cluster{
		Docker: docker,
		Spec: k0scontainer.Spec{
			Name:     BreakGlassName(cfg),
			Image:    breakglass.Image(c.Version),
			Args:     c.Args,
			Network:  cfg.BootstrapCluster.Network,
			Labels:   map[string]string{LabelCluster: cfg.Metadata.Name, LabelRole: "break-glass"},
			Files:    c.Files,
			Readable: c.Readable,
		},
	}
}

func newLocator(cfg *config.ManagementCluster, deps Deps) *locator {
	return &locator{
		cfg:        cfg,
		cluster:    BootstrapCluster(cfg, deps.Docker),
		breakGlass: breakGlassCluster(cfg, deps.Docker, breakglass.Controller{}),
		clients:    deps.Clients,
		probe:      deps.Probe,
		now:        deps.Now,
	}
}

// RecoverPhases are the phases of `selfhostctl recover`, in order.
func RecoverPhases(cfg *config.ManagementCluster, deps Deps) []phase.Phase {
	deps.defaults()
	loc := newLocator(cfg, deps)
	cluster := BootstrapCluster(cfg, deps.Docker)
	bg := breakGlassPhase{cfg: cfg, loc: loc, docker: deps.Docker, clients: deps.Clients, lookup: deps.Lookup, timeout: deps.BreakGlassTimeout}
	phases := []phase.Phase{
		&outagePhase{cfg: cfg, loc: loc},
		newBootstrapClusterPhase(cfg, deps),
		&hostPhase{cluster: cluster, docker: deps.Docker},
		newProvidersPhase(cfg, deps),
		newBeforeApplyPhase(cfg, deps),
		beforeMove{&bg, loc},
		beforeMove{&leasesPhase{cfg: cfg, loc: loc, clients: deps.Clients, timeout: deps.BreakGlassTimeout}, loc},
		beforeMove{&webhooksPhase{loc: loc, clients: deps.Clients}, loc},
		beforeMove{&suspendPhase{tool: gitops.For(cfg.GitOps), loc: loc, clients: deps.Clients}, loc},
		beforeMove{&pauseChildrenPhase{cfg: cfg, loc: loc, clients: deps.Clients}, loc},
		beforeMove{&deleteNodesPhase{loc: loc, clients: deps.Clients}, loc},
		&moveToBootstrapPhase{move: movePhase{cfg: cfg, loc: loc, installer: deps.Providers, mover: deps.Mover, clients: deps.Clients}},
		&deleteBreakGlassPhase{loc: loc, container: bg.container(breakglass.Controller{})},
	}
	for i, p := range phases {
		phases[i] = tracked{p, loc}
	}
	return phases
}

// RecoverySecrets reads what `recover` needs from the environment, or says which
// variables are missing.
func RecoverySecrets(cfg *config.ManagementCluster, lookup func(string) (string, bool)) (breakglass.Secrets, error) {
	r := cfg.Recovery
	if r == nil {
		return breakglass.Secrets{}, errors.New("the config has no recovery")
	}
	get := func(name string) string { v, _ := lookup(name); return v }
	var missing []string
	names := []string{r.BundleFromEnv, r.EncryptionKeyFromEnv}
	if r.Kine != nil {
		names = append(names, r.Kine.DatasourceFromEnv)
	}
	for _, n := range names {
		if get(n) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return breakglass.Secrets{}, fmt.Errorf("empty or unset: %s. Load them for this one command, e.g. op run -- selfhostctl recover", strings.Join(missing, ", "))
	}
	b, err := recovery.Parse(get(r.BundleFromEnv))
	if err != nil {
		return breakglass.Secrets{}, fmt.Errorf("%s: %w", r.BundleFromEnv, err)
	}
	s := breakglass.Secrets{Bundle: b, EncryptionKey: get(r.EncryptionKeyFromEnv)}
	if r.Kine != nil {
		s.Datasource = get(r.Kine.DatasourceFromEnv)
	}
	return s, nil
}

// moved: the Bootstrap cluster (or, later, the Management cluster) holds what the
// Break-glass controller served.
func moved(s Situation) bool {
	return s.WebhooksSaved && !s.BreakGlassHolds &&
		(s.Location == LocationBootstrap || s.Location == LocationSplit || s.Location == LocationManagement)
}

// beforeMove is a phase on the Break-glass controller: done once the objects moved.
type beforeMove struct {
	phase.Phase
	loc *locator
}

func (p beforeMove) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if moved(s) {
		return phase.Status{State: phase.Done, Reason: "moved"}, nil
	}
	return p.Phase.Status(ctx)
}

// notRecovering blocks bootstrap's phases while a Recovery is in progress.
type notRecovering struct {
	phase.Phase
	loc *locator
}

func (p notRecovering) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if s.Recovering() {
		return phase.Status{State: phase.Blocked, Reason: "a Recovery is in progress: run `selfhostctl recover`"}, nil
	}
	return p.Phase.Status(ctx)
}

func breakGlassKubeconfig(s Situation) ([]byte, *phase.Status) {
	if s.BreakGlassKC == nil {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Break-glass controller's API doesn't answer (phase break-glass)"}
	}
	return s.BreakGlassKC, nil
}

func bootstrapKubeconfig(s Situation) ([]byte, *phase.Status) {
	if s.BootstrapKC == nil {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Bootstrap cluster isn't ready (phase bootstrap-cluster)"}
	}
	return s.BootstrapKC, nil
}

// --- outage ---

// outagePhase is the gate of a Recovery: the Management cluster doesn't answer.
type outagePhase struct {
	cfg *config.ManagementCluster
	loc *locator
}

func (p *outagePhase) Name() string { return "outage" }

func (p *outagePhase) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	blocked := func(reason string) (phase.Status, error) {
		return phase.Status{State: phase.Blocked, Reason: reason}, nil
	}
	switch {
	case s.WebhooksSaved:
		return phase.Status{State: phase.Done, Reason: "a Recovery is in progress"}, nil
	case s.ManagementKC != nil:
		reason := "the Management cluster answers: nothing to recover"
		if s.BreakGlassExists {
			reason += fmt.Sprintf(". Remove the Break-glass controller left from an earlier run: docker rm -f -v %s", BreakGlassName(p.cfg))
		}
		return blocked(reason)
	case s.Refused:
		return blocked(s.Reason)
	case s.Location == LocationBootstrap || s.Location == LocationSplit && !s.BreakGlassHolds:
		// Before the pivot, the Management cluster may not answer yet.
		return blocked("the Management cluster's objects are in the Bootstrap cluster: that is `bootstrap`'s to finish, not a Recovery")
	case s.Down:
		return phase.Status{State: phase.Done, Reason: "the Management cluster doesn't answer"}, nil
	}
	path, _ := p.cfg.KubeconfigPath()
	return blocked(fmt.Sprintf("no kubeconfig at %s, so nothing shows which cluster is down: write one from the Recovery bundle with `op run -- selfhostctl kubeconfig renew -f <config>`, then run again", path))
}

func (p *outagePhase) Run(context.Context) error {
	return errors.New("the outage gate has nothing to run")
}

// --- host ---

// hostPhase checks the Docker host can run the Bootstrap cluster and the Break-glass
// controller side by side.
type hostPhase struct {
	cluster *k0scontainer.Cluster
	docker  k0scontainer.Docker
}

func (p *hostPhase) Name() string { return "host" }

func (p *hostPhase) Status(ctx context.Context) (phase.Status, error) {
	info, err := p.docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		return phase.Status{}, fmt.Errorf("docker info: %w", err)
	}
	if mem := info.Info.MemTotal; mem < minDockerMemory {
		return phase.Status{State: phase.Blocked, Reason: fmt.Sprintf(
			"Docker has %.1f GiB of memory; the Bootstrap cluster and the Break-glass controller side by side need at least %d GiB: give Docker more",
			float64(mem)/(1<<30), minDockerMemory>>30)}, nil
	}
	if _, blocked, err := reach(ctx, p.cluster); err != nil || blocked != nil {
		return deref(blocked), err
	}
	out, err := p.cluster.Output(ctx, "cat /proc/sys/fs/inotify/max_user_watches /proc/sys/fs/inotify/max_user_instances")
	if err != nil {
		return phase.Status{}, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return phase.Status{}, fmt.Errorf("reading the inotify limits: %q", out)
	}
	var low []string
	for i, want := range []int64{minInotifyWatches, minInotifyInstances} {
		name := []string{"fs.inotify.max_user_watches", "fs.inotify.max_user_instances"}[i]
		got, err := strconv.ParseInt(fields[i], 10, 64)
		if err != nil {
			return phase.Status{}, fmt.Errorf("%s: %w", name, err)
		}
		if got < want {
			low = append(low, fmt.Sprintf("%s=%d", name, want))
		}
	}
	if len(low) > 0 {
		return phase.Status{State: phase.Blocked, Reason: "the Docker host's inotify limits are too low for the Bootstrap cluster, the Break-glass controller and their pods: raise them on the Docker host with sysctl -w " + strings.Join(low, " ")}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *hostPhase) Run(context.Context) error {
	return errors.New("the host check has nothing to run")
}

// --- break-glass ---

// breakGlassPhase runs the Break-glass controller against the datastore, at the live
// K0sControlPlane's version once its API answers.
type breakGlassPhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	docker  k0scontainer.Docker
	clients Clients
	lookup  func(string) (string, bool)
	timeout time.Duration
}

func (p *breakGlassPhase) Name() string { return "break-glass" }

func (p *breakGlassPhase) container(c breakglass.Controller) *k0scontainer.Cluster {
	return breakGlassCluster(p.cfg, p.docker, c)
}

// kcp is the Cluster's K0sControlPlane in the manifests.
func (p *breakGlassPhase) kcp() (*unstructured.Unstructured, error) {
	objs, err := apply.Load(p.cfg.Path(p.cfg.Cluster.Manifests))
	if err != nil {
		return nil, fmt.Errorf("cluster.manifests: %w", err)
	}
	return breakglass.Find(objs, p.cfg.Cluster.Namespace, p.cfg.ClusterName())
}

func (p *breakGlassPhase) controller(version string) (breakglass.Controller, error) {
	kcp, err := p.kcp()
	if err != nil {
		return breakglass.Controller{}, err
	}
	s, err := RecoverySecrets(p.cfg, p.lookup)
	if err != nil {
		return breakglass.Controller{}, err
	}
	return breakglass.Derive(kcp, version, s)
}

// liveVersion is the K0sControlPlane's spec.version as the Break-glass controller
// serves it; an error while its API doesn't answer.
func (p *breakGlassPhase) liveVersion(ctx context.Context, kc []byte) (string, error) {
	kcp, err := p.kcp()
	if err != nil {
		return "", err
	}
	c, err := p.clients(kc)
	if err != nil {
		return "", err
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(breakglass.K0sControlPlaneGVK)
	if err := c.Get(ctx, ctrlclient.ObjectKeyFromObject(kcp), live); err != nil {
		return "", err
	}
	v, _, _ := unstructured.NestedString(live.Object, "spec", "version")
	return v, nil
}

func (p *breakGlassPhase) Status(ctx context.Context) (phase.Status, error) {
	o, err := p.container(breakglass.Controller{}).Inspect(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if !o.Exists || !o.Running {
		return phase.Status{State: phase.Next}, nil
	}
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if s.BreakGlassKC == nil {
		return phase.Status{State: phase.Next, Reason: "its API doesn't answer yet"}, nil
	}
	live, err := p.liveVersion(ctx, s.BreakGlassKC)
	if apierrors.IsNotFound(err) {
		return phase.Status{State: phase.Blocked, Reason: "the Break-glass controller serves no K0sControlPlane of the Cluster: is recovery.kine's data source the Management cluster's datastore?"}, nil
	}
	if err != nil {
		return phase.Status{}, err
	}
	if live != "" && breakglass.Image(live) != o.Image {
		return phase.Status{State: phase.Next, Reason: fmt.Sprintf("it runs %s, the live K0sControlPlane is at %s", o.Image, live)}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *breakGlassPhase) Run(ctx context.Context) error {
	o, err := p.container(breakglass.Controller{}).Inspect(ctx)
	if err != nil {
		return err
	}
	ctrl := breakglass.Controller{}
	if o.Exists && !o.Running {
		// It holds no state of its own, and a stopped one may have stopped on its
		// config: made again from the current one.
		if err := p.container(ctrl).Remove(ctx); err != nil {
			return err
		}
		o.Exists = false
	}
	if !o.Exists {
		if ctrl, err = p.controller(""); err != nil {
			return err
		}
	}
	bg := p.container(ctrl)
	if err := bg.Ensure(ctx); err != nil {
		return err
	}
	kc, err := p.wait(ctx, bg)
	if err != nil {
		return err
	}
	live, err := p.liveVersion(ctx, kc)
	if err != nil || live == "" {
		return err
	}
	if o, err = bg.Inspect(ctx); err != nil || o.Image == breakglass.Image(live) {
		return err
	}
	// The cluster runs another version than its manifest says (e.g. a rollout stopped
	// halfway): its datastore is the live version's.
	if ctrl, err = p.controller(live); err != nil {
		return err
	}
	if err := bg.Remove(ctx); err != nil {
		return err
	}
	bg = p.container(ctrl)
	if err := bg.Ensure(ctx); err != nil {
		return err
	}
	_, err = p.wait(ctx, bg)
	return err
}

// wait waits until the controller's API serves the K0sControlPlane.
func (p *breakGlassPhase) wait(ctx context.Context, bg *k0scontainer.Cluster) ([]byte, error) {
	deadline := time.Now().Add(p.timeout)
	var last error
	for {
		if o, err := bg.Inspect(ctx); err != nil {
			return nil, err
		} else if !o.Running {
			return nil, fmt.Errorf("the Break-glass controller (container %s) stopped: see docker logs %s", bg.Spec.Name, bg.Spec.Name)
		}
		kc, err := bg.Kubeconfig(ctx)
		if err == nil {
			if _, err = p.liveVersion(ctx, kc); err == nil {
				return kc, nil
			}
			if apierrors.IsNotFound(err) {
				return nil, errors.New("the Break-glass controller serves no K0sControlPlane of the Cluster: is recovery.kine's data source the Management cluster's datastore?")
			}
		}
		last = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the Break-glass controller (container %s) doesn't serve the cluster after %s: %w", bg.Spec.Name, p.timeout, last)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// --- leases ---

// leaseQuiet is how long every other API server and node must have been silent: a
// live one renews its lease about every 10 seconds.
const leaseQuiet = 2 * time.Minute

// leasesPhase is the proof of the outage from the datastore itself, before anything
// changes there: no API server, k0s controller or kubelet but the Break-glass
// controller's still renews its lease. A cluster only cut off from the operator (a VPN,
// an API proxy) still does.
type leasesPhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	clients Clients
	timeout time.Duration
}

func (p *leasesPhase) Name() string { return "leases" }

// apiserverIdentity labels kube-apiserver's identity leases.
const apiserverIdentity = "apiserver.kubernetes.io/identity"

// judge compares every other lease with the Break-glass controller's own: the same
// clock that renewed it, not the operator's. ok is false while its own isn't there.
func (p *leasesPhase) judge(ctx context.Context, c ctrlclient.Client) (live []string, ok bool, err error) {
	own := BreakGlassName(p.cfg)
	var ref time.Time
	type renewed struct {
		name string
		at   time.Time
	}
	var others []renewed
	apiservers := &coordinationv1.LeaseList{}
	if err := c.List(ctx, apiservers, ctrlclient.InNamespace("kube-system"), ctrlclient.HasLabels{apiserverIdentity}); err != nil {
		return nil, false, err
	}
	for _, l := range apiservers.Items {
		at := renewTime(l)
		if l.Labels["kubernetes.io/hostname"] == own {
			if at.After(ref) {
				ref = at
			}
			continue
		}
		others = append(others, renewed{"API server " + l.Labels["kubernetes.io/hostname"], at})
	}
	nodes := &coordinationv1.LeaseList{}
	if err := c.List(ctx, nodes, ctrlclient.InNamespace("kube-node-lease")); err != nil {
		return nil, false, err
	}
	for _, l := range nodes.Items {
		if l.Name == "k0s-ctrl-"+own {
			continue
		}
		others = append(others, renewed{"Lease kube-node-lease/" + l.Name, renewTime(l)})
	}
	if ref.IsZero() {
		return nil, false, nil
	}
	for _, o := range others {
		if ago := ref.Sub(o.at); ago < leaseQuiet {
			live = append(live, fmt.Sprintf("%s (%s ago)", o.name, ago.Round(time.Second)))
		}
	}
	return live, true, nil
}

func renewTime(l coordinationv1.Lease) time.Time {
	if l.Spec.RenewTime != nil {
		return l.Spec.RenewTime.Time
	}
	if l.Spec.AcquireTime != nil {
		return l.Spec.AcquireTime.Time
	}
	return time.Time{}
}

func (p *leasesPhase) client(ctx context.Context) (ctrlclient.Client, *phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	kc, blocked := breakGlassKubeconfig(s)
	if blocked != nil {
		return nil, blocked, nil
	}
	c, err := p.clients(kc)
	return c, nil, err
}

func (p *leasesPhase) Status(ctx context.Context) (phase.Status, error) {
	c, blocked, err := p.client(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	live, ok, err := p.judge(ctx, c)
	if err != nil {
		return phase.Status{}, err
	}
	if !ok {
		return phase.Status{State: phase.Next, Reason: "the Break-glass controller hasn't renewed its own lease yet"}, nil
	}
	if len(live) > 0 {
		return phase.Status{State: phase.Blocked, Reason: fmt.Sprintf(
			"the datastore shows a live cluster: %s renewed its lease within %s. Its API may only be cut off from here (a VPN, an API proxy): nothing in it was changed. "+
				"If so, remove what recover started (docker rm -f -v %s %s). If its machines are really gone, run again in %s",
			strings.Join(live, ", "), leaseQuiet, BreakGlassName(p.cfg), ContainerName(p.cfg), leaseQuiet)}, nil
	}
	return phase.Status{State: phase.Done, Reason: "no other API server or node renews its lease"}, nil
}

// Run waits for the Break-glass controller's own lease; Status then judges.
func (p *leasesPhase) Run(ctx context.Context) error {
	deadline := time.Now().Add(p.timeout)
	for {
		c, blocked, err := p.client(ctx)
		if err != nil {
			return err
		}
		if blocked != nil {
			return errors.New(blocked.Reason)
		}
		if _, ok, err := p.judge(ctx, c); err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Break-glass controller renewed no lease of its own in %s", p.timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// --- webhooks ---

// webhooksPhase saves the providers' webhook configurations into the Bootstrap
// cluster, then removes them: no pod serves them, and the move must pass.
type webhooksPhase struct {
	loc     *locator
	clients Clients
}

func (p *webhooksPhase) Name() string { return "webhooks" }

func (p *webhooksPhase) live(ctx context.Context, c ctrlclient.Client) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	for _, gvk := range webhookGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := c.List(ctx, list, ctrlclient.HasLabels{providerLabel}); err != nil {
			return nil, err
		}
		out = append(out, list.Items...)
	}
	return out, nil
}

func (p *webhooksPhase) sides(ctx context.Context) (bg, boot ctrlclient.Client, s Situation, blocked *phase.Status, err error) {
	if s, err = p.loc.get(ctx); err != nil {
		return nil, nil, s, nil, err
	}
	bgKC, blocked := breakGlassKubeconfig(s)
	if blocked != nil {
		return nil, nil, s, blocked, nil
	}
	bootKC, blocked := bootstrapKubeconfig(s)
	if blocked != nil {
		return nil, nil, s, blocked, nil
	}
	if bg, err = p.clients(bgKC); err != nil {
		return nil, nil, s, nil, err
	}
	boot, err = p.clients(bootKC)
	return bg, boot, s, nil, err
}

func (p *webhooksPhase) Status(ctx context.Context) (phase.Status, error) {
	bg, _, s, blocked, err := p.sides(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	live, err := p.live(ctx, bg)
	if err != nil {
		return phase.Status{}, err
	}
	if !s.WebhooksSaved || len(live) > 0 {
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *webhooksPhase) Run(ctx context.Context) error {
	bg, boot, s, blocked, err := p.sides(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	live, err := p.live(ctx, bg)
	if err != nil {
		return err
	}
	if !s.WebhooksSaved {
		// Saved once: a re-run finds them gone from the cluster and keeps these.
		data, err := saved(live)
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: WebhooksNamespace, Name: WebhooksConfigMap},
			Data:       map[string]string{WebhooksKey: string(data)},
		}
		if err := boot.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("saving the webhooks: %w", err)
		}
	}
	for i := range live {
		if err := bg.Delete(ctx, &live[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s %s: %w", live[i].GetKind(), live[i].GetName(), err)
		}
	}
	return nil
}

// saved is the webhook configurations as a List, without what the API server sets.
func saved(items []unstructured.Unstructured) ([]byte, error) {
	list := make([]any, len(items))
	for i := range items {
		o := items[i].DeepCopy()
		for _, f := range []string{"uid", "resourceVersion", "creationTimestamp", "generation", "managedFields"} {
			unstructured.RemoveNestedField(o.Object, "metadata", f)
		}
		list[i] = o.Object
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	err := enc.Encode(map[string]any{"apiVersion": "v1", "kind": "List", "items": list})
	return buf.Bytes(), err
}

// --- suspend-gitops ---

// suspendPhase suspends the GitOps tool on the Break-glass controller, so nothing
// re-applies the children while they move.
type suspendPhase struct {
	tool    gitops.Tool
	loc     *locator
	clients Clients
}

func (p *suspendPhase) Name() string { return "suspend-gitops" }

func (p *suspendPhase) client(ctx context.Context) (ctrlclient.Client, *phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	kc, blocked := breakGlassKubeconfig(s)
	if blocked != nil {
		return nil, blocked, nil
	}
	c, err := p.clients(kc)
	return c, nil, err
}

func (p *suspendPhase) Status(ctx context.Context) (phase.Status, error) {
	if p.tool == nil {
		return phase.Status{State: phase.Done, Reason: "no gitops in the config"}, nil
	}
	c, blocked, err := p.client(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	left, err := p.tool.Unsuspended(ctx, c)
	if err != nil {
		return phase.Status{}, err
	}
	if len(left) > 0 {
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *suspendPhase) Run(ctx context.Context) error {
	c, blocked, err := p.client(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	_, err = p.tool.Suspend(ctx, c)
	return err
}

// --- pause-children ---

// pauseChildrenPhase pauses every Cluster outside the Management cluster's namespace,
// marking what it pauses.
type pauseChildrenPhase struct {
	cfg     *config.ManagementCluster
	loc     *locator
	clients Clients
}

func (p *pauseChildrenPhase) Name() string { return "pause-children" }

// unpaused lists the Child clusters' Clusters not paused yet.
func (p *pauseChildrenPhase) unpaused(ctx context.Context) (ctrlclient.Client, []unstructured.Unstructured, *phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	kc, blocked := breakGlassKubeconfig(s)
	if blocked != nil {
		return nil, nil, blocked, nil
	}
	c, err := p.clients(kc)
	if err != nil {
		return nil, nil, nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterGVK.GroupVersion().WithKind("ClusterList"))
	if err := c.List(ctx, list); err != nil {
		return nil, nil, nil, err
	}
	var out []unstructured.Unstructured
	for _, cl := range list.Items {
		if paused, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); !paused && cl.GetNamespace() != p.cfg.Cluster.Namespace {
			out = append(out, cl)
		}
	}
	return c, out, nil, nil
}

func (p *pauseChildrenPhase) Status(ctx context.Context) (phase.Status, error) {
	_, left, blocked, err := p.unpaused(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	if len(left) > 0 {
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *pauseChildrenPhase) Run(ctx context.Context) error {
	c, left, blocked, err := p.unpaused(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:"true"}},"spec":{"paused":true}}`, PausedAnnotation)
	for i := range left {
		if err := c.Patch(ctx, &left[i], ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
			return fmt.Errorf("pausing Cluster %s/%s: %w", left[i].GetNamespace(), left[i].GetName(), err)
		}
	}
	return nil
}

// --- delete-nodes ---

// deleteNodesPhase deletes the old Nodes: a new machine with a reused IP would match
// one.
type deleteNodesPhase struct {
	loc     *locator
	clients Clients
}

func (p *deleteNodesPhase) Name() string { return "delete-nodes" }

func (p *deleteNodesPhase) nodes(ctx context.Context) (ctrlclient.Client, []corev1.Node, *phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	kc, blocked := breakGlassKubeconfig(s)
	if blocked != nil {
		return nil, nil, blocked, nil
	}
	c, err := p.clients(kc)
	if err != nil {
		return nil, nil, nil, err
	}
	list := &corev1.NodeList{}
	if err := c.List(ctx, list); err != nil {
		return nil, nil, nil, err
	}
	return c, list.Items, nil, nil
}

func (p *deleteNodesPhase) Status(ctx context.Context) (phase.Status, error) {
	_, nodes, blocked, err := p.nodes(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	if len(nodes) > 0 {
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

func (p *deleteNodesPhase) Run(ctx context.Context) error {
	c, nodes, blocked, err := p.nodes(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	for i := range nodes {
		if err := c.Delete(ctx, &nodes[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting Node %s: %w", nodes[i].Name, err)
		}
	}
	return nil
}

// --- move-to-bootstrap ---

// moveToBootstrapPhase moves the Management cluster's namespace from the Break-glass
// controller into the Bootstrap cluster, behind the pivot's gates.
type moveToBootstrapPhase struct {
	move movePhase
}

func (p *moveToBootstrapPhase) Name() string { return "move-to-bootstrap" }

func (p *moveToBootstrapPhase) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.move.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	switch {
	case s.Location == LocationBreakGlass:
		boot, blocked := bootstrapKubeconfig(s)
		if blocked != nil {
			return *blocked, nil
		}
		gate, err := p.move.gates(ctx, s.BreakGlassKC, boot)
		if err != nil || gate != nil {
			return deref(gate), err
		}
		return phase.Status{State: phase.Next}, nil
	case s.BreakGlassHolds:
		return phase.Status{State: phase.Next, Reason: "a move stopped halfway; running resumes it"}, nil
	case moved(s):
		if s.Location == LocationBootstrap && s.SourcePaused {
			return phase.Status{State: phase.Next, Reason: "moved, but the Cluster is still paused"}, nil
		}
		return phase.Status{State: phase.Done}, nil
	}
	return phase.Status{State: phase.Blocked, Reason: "the Break-glass controller doesn't serve the Cluster yet (phase break-glass)"}, nil
}

func (p *moveToBootstrapPhase) Run(ctx context.Context) error {
	s, err := p.move.loc.get(ctx)
	if err != nil {
		return err
	}
	ns := p.move.cfg.Cluster.Namespace
	switch {
	case s.Location == LocationBreakGlass:
		if err := p.move.mover.Move(ctx, s.BreakGlassKC, s.BootstrapKC, ns, true); err != nil {
			return fmt.Errorf("dry run: %w", err)
		}
		return p.move.mover.Move(ctx, s.BreakGlassKC, s.BootstrapKC, ns, false)
	case s.BreakGlassHolds:
		if s.BootstrapKC == nil {
			return errors.New("a move stopped halfway, and the Bootstrap cluster can't be reached")
		}
		// Unpause the source so the move runs again; it updates what already arrived.
		if err := p.move.setPaused(ctx, s.BreakGlassKC, false); err != nil {
			return err
		}
		return p.move.mover.Move(ctx, s.BreakGlassKC, s.BootstrapKC, ns, false)
	case moved(s):
		// The objects arrived, but the move ended before it unpaused them.
		return p.move.setPaused(ctx, s.BootstrapKC, false)
	}
	return fmt.Errorf("nothing to move at location %s", s.Location)
}

// --- delete-break-glass ---

// deleteBreakGlassPhase removes the Break-glass controller once the objects moved.
type deleteBreakGlassPhase struct {
	loc       *locator
	container *k0scontainer.Cluster
}

func (p *deleteBreakGlassPhase) Name() string { return "delete-break-glass" }

func (p *deleteBreakGlassPhase) Status(ctx context.Context) (phase.Status, error) {
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	switch {
	case !s.BreakGlassExists:
		return phase.Status{State: phase.Done}, nil
	case moved(s):
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Blocked, Reason: "the objects haven't moved to the Bootstrap cluster yet (phase move-to-bootstrap)"}, nil
}

func (p *deleteBreakGlassPhase) Run(ctx context.Context) error {
	return p.container.Remove(ctx)
}

// --- status ---

// Warnings are what `status` warns about: suspended GitOps objects and paused
// Clusters, read from the Break-glass controller while it runs, else from the
// Management cluster.
func Warnings(ctx context.Context, cfg *config.ManagementCluster, deps Deps) ([]string, error) {
	deps.defaults()
	s, err := newLocator(cfg, deps).get(ctx)
	if err != nil {
		return nil, err
	}
	kc := s.BreakGlassKC
	if kc == nil && s.Location == LocationManagement {
		kc = s.ManagementKC
	}
	if kc == nil {
		return nil, nil
	}
	c, err := deps.Clients(kc)
	if err != nil {
		return nil, err
	}
	var out []string
	if tool := gitops.For(cfg.GitOps); tool != nil {
		refs, err := tool.Suspended(ctx, c)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			out = append(out, r+" is suspended")
		}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterGVK.GroupVersion().WithKind("ClusterList"))
	if err := c.List(ctx, list); err != nil && !isNoMatch(err) {
		return nil, err
	}
	for _, cl := range list.Items {
		if paused, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); paused {
			out = append(out, fmt.Sprintf("Cluster %s/%s is paused", cl.GetNamespace(), cl.GetName()))
		}
	}
	return out, nil
}
