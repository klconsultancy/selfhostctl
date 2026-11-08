// Package bootstrap holds the phases of `selfhostctl bootstrap`.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
	"github.com/klconsultancy/selfhostctl/internal/kube"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

// Image is the Bootstrap cluster's k0s, pinned per tool release like the CAPI core
// (docs/design.md#bootstrap-cluster).
const Image = "docker.io/k0sproject/k0s:v1.36.5-k0s.0"

// Labels mark the tool's containers, so they can be told apart from anything else on
// the host.
const (
	LabelCluster = "io.selfhostctl.cluster"
	LabelRole    = "io.selfhostctl.role"
)

// ContainerName is the Bootstrap cluster's container for a Management cluster.
func ContainerName(cfg *config.ManagementCluster) string {
	return "selfhostctl-" + cfg.Metadata.Name + "-bootstrap"
}

// Deps is what the phases need from the outside world.
type Deps struct {
	Docker k0scontainer.Docker
	// NodesReady reports whether the cluster behind a kubeconfig has Ready nodes;
	// kube.NodesReady through a client outside tests.
	NodesReady func(ctx context.Context, kubeconfig []byte) (bool, error)
	// ReadyTimeout bounds the wait for a new Bootstrap cluster.
	ReadyTimeout time.Duration
	// Providers reads and installs the Cluster API providers;
	// providers.Clusterctl outside tests.
	Providers providers.Installer
	// Prerequisites judges and installs the beforeApply entries.
	Prerequisites Prerequisites
	// Clients turns a kubeconfig into a client; LiveClients when nil.
	Clients Clients
	// Lookup reads the environment; os.LookupEnv when nil.
	Lookup func(string) (string, bool)
	// ClusterReadyTimeout bounds the wait for the Management cluster to be ready.
	ClusterReadyTimeout time.Duration
	// Mover moves the namespace between clusters; providers.Clusterctl outside tests.
	Mover Mover
	// First is `bootstrap --first`: the operator asserts no Management cluster exists
	// yet, so a Bootstrap cluster may be created without a handle proving it.
	First bool
	// Probe reports whether something answers TLS at host:port; ProbeTLS when nil.
	Probe func(ctx context.Context, hostport string) bool
	// Now is time.Now outside tests.
	Now func() time.Time
}

func (d *Deps) defaults() {
	if d.Clients == nil {
		d.Clients = LiveClients
	}
	if d.Lookup == nil {
		d.Lookup = os.LookupEnv
	}
	if d.ClusterReadyTimeout == 0 {
		d.ClusterReadyTimeout = 30 * time.Minute
	}
	if d.Probe == nil {
		d.Probe = ProbeTLS
	}
	if d.Now == nil {
		d.Now = time.Now
	}
}

// Locate finds where the Management cluster's objects live now.
func Locate(ctx context.Context, cfg *config.ManagementCluster, deps Deps) (Situation, error) {
	deps.defaults()
	l := &locator{cfg: cfg, cluster: BootstrapCluster(cfg, deps.Docker), clients: deps.Clients, probe: deps.Probe}
	return l.get(ctx)
}

// PivotFrom is the first phase of `selfhostctl pivot`.
const PivotFrom = "management-providers"

// LiveNodesReady is Deps.NodesReady against a real cluster.
func LiveNodesReady(ctx context.Context, kubeconfig []byte) (bool, error) {
	c, err := kube.Client(kubeconfig)
	if err != nil {
		return false, err
	}
	return kube.NodesReady(ctx, c)
}

// Phases are bootstrap's phases, in order.
func Phases(cfg *config.ManagementCluster, deps Deps) []phase.Phase {
	deps.defaults()
	cluster := BootstrapCluster(cfg, deps.Docker)
	loc := &locator{cfg: cfg, cluster: cluster, clients: deps.Clients, probe: deps.Probe}

	onManagement := func(name string) func(ctx context.Context) ([]byte, *phase.Status, error) {
		return func(ctx context.Context) ([]byte, *phase.Status, error) {
			s, err := loc.get(ctx)
			if err != nil {
				return nil, nil, err
			}
			kc, blocked := managementKC(s, name)
			return kc, blocked, nil
		}
	}
	var forManagement []config.Prerequisite
	for _, p := range cfg.BeforeApply {
		if p.IsOn(config.OnManagement) {
			forManagement = append(forManagement, p)
		}
	}
	afterPivot := onManagement("move")
	wanted := providers.FromConfig(&cfg.Providers)

	phases := []phase.Phase{
		guardedBootstrapCluster{Phase: newBootstrapClusterPhase(cfg, deps), loc: loc, first: deps.First},
		prePivot{newProvidersPhase(cfg, deps), loc},
		prePivot{newBeforeApplyPhase(cfg, deps), loc},
		prePivot{&applyPhase{cfg: cfg, cluster: cluster, clients: deps.Clients}, loc},
		prePivot{&readyPhase{cfg: cfg, cluster: cluster, clients: deps.Clients, timeout: deps.ClusterReadyTimeout}, loc},
		prePivot{&recoveryBundlePhase{cfg: cfg, cluster: cluster, clients: deps.Clients, lookup: deps.Lookup}, loc},
		prePivot{&kubeconfigPhase{cfg: cfg, cluster: cluster, clients: deps.Clients}, loc},
		&managementProvidersPhase{loc: loc, wanted: wanted, installer: deps.Providers},
		&prerequisitesPhase{name: "management-before-apply", entries: forManagement, prereqs: deps.Prerequisites, kc: onManagement("kubeconfig")},
		&movePhase{cfg: cfg, loc: loc, installer: deps.Providers, mover: deps.Mover, clients: deps.Clients},
		&prerequisitesPhase{name: "after-pivot", entries: cfg.AfterPivot, prereqs: deps.Prerequisites, kc: func(ctx context.Context) ([]byte, *phase.Status, error) {
			s, err := loc.get(ctx)
			if err != nil {
				return nil, nil, err
			}
			if s.Location != LocationManagement || s.TargetPaused {
				return nil, &phase.Status{State: phase.Blocked, Reason: "the Management cluster doesn't manage itself yet (phase move)"}, nil
			}
			return afterPivot(ctx)
		}},
		&deleteBootstrapPhase{cfg: cfg, loc: loc, cluster: cluster, clients: deps.Clients, now: deps.Now},
	}
	for i, p := range phases {
		phases[i] = tracked{p, loc}
	}
	return phases
}

// BootstrapCluster is the Bootstrap cluster of a Management cluster.
func BootstrapCluster(cfg *config.ManagementCluster, docker k0scontainer.Docker) *k0scontainer.Cluster {
	mounts := make([]k0scontainer.Mount, len(cfg.BootstrapCluster.ExtraMounts))
	for i, m := range cfg.BootstrapCluster.ExtraMounts {
		mounts[i] = k0scontainer.Mount{HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	return &k0scontainer.Cluster{
		Docker: docker,
		Spec: k0scontainer.Spec{
			Name:    ContainerName(cfg),
			Image:   Image,
			Args:    []string{"controller", "--enable-worker", "--no-taints"},
			Network: cfg.BootstrapCluster.Network,
			Mounts:  mounts,
			Labels:  map[string]string{LabelCluster: cfg.Metadata.Name, LabelRole: "bootstrap"},
		},
	}
}

// bootstrapClusterPhase creates the Bootstrap cluster: one k0s container.
type bootstrapClusterPhase struct {
	cluster *k0scontainer.Cluster
	deps    Deps
}

func newBootstrapClusterPhase(cfg *config.ManagementCluster, deps Deps) *bootstrapClusterPhase {
	if deps.ReadyTimeout == 0 {
		deps.ReadyTimeout = 5 * time.Minute
	}
	return &bootstrapClusterPhase{cluster: BootstrapCluster(cfg, deps.Docker), deps: deps}
}

func (p *bootstrapClusterPhase) Name() string { return "bootstrap-cluster" }

func (p *bootstrapClusterPhase) Status(ctx context.Context) (phase.Status, error) {
	o, err := p.cluster.Inspect(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if !o.Exists || !o.Running {
		return phase.Status{State: phase.Next}, nil
	}
	if o.Image != p.cluster.Spec.Image {
		return phase.Status{State: phase.Blocked, Reason: fmt.Sprintf(
			"container %s runs %s, this release needs %s: remove it (docker rm -f -v %s) and run again",
			p.cluster.Spec.Name, o.Image, p.cluster.Spec.Image, p.cluster.Spec.Name)}, nil
	}
	ready, err := p.ready(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if !ready {
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

// ready: the container's API answers and its node is Ready.
func (p *bootstrapClusterPhase) ready(ctx context.Context) (bool, error) {
	kc, err := p.cluster.Kubeconfig(ctx)
	if errors.Is(err, k0scontainer.ErrNotReady) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ok, err := p.deps.NodesReady(ctx, kc)
	if err != nil {
		// The API isn't up yet.
		return false, nil
	}
	return ok, nil
}

func (p *bootstrapClusterPhase) Run(ctx context.Context) error {
	if err := p.cluster.Ensure(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(p.deps.ReadyTimeout)
	for {
		ok, err := p.ready(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Bootstrap cluster (container %s) has no Ready node after %s", p.cluster.Spec.Name, p.deps.ReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
