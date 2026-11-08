package bootstrap

import (
	"context"
	"errors"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

// providersPhase installs the declared providers on the Bootstrap cluster.
type providersPhase struct {
	cluster   *k0scontainer.Cluster
	wanted    providers.Wanted
	installer providers.Installer
}

func newProvidersPhase(cfg *config.ManagementCluster, deps Deps) *providersPhase {
	return &providersPhase{
		cluster:   BootstrapCluster(cfg, deps.Docker),
		wanted:    providers.FromConfig(&cfg.Providers),
		installer: deps.Providers,
	}
}

func (p *providersPhase) Name() string { return "providers" }

func (p *providersPhase) kubeconfig(ctx context.Context) ([]byte, *phase.Status, error) {
	o, err := p.cluster.Inspect(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !o.Running {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Bootstrap cluster doesn't run (phase bootstrap-cluster)"}, nil
	}
	kc, err := p.cluster.Kubeconfig(ctx)
	if errors.Is(err, k0scontainer.ErrNotReady) {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Bootstrap cluster isn't ready yet (phase bootstrap-cluster)"}, nil
	}
	return kc, nil, err
}

func (p *providersPhase) Status(ctx context.Context) (phase.Status, error) {
	kc, blocked, err := p.kubeconfig(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	installed, err := p.installer.Installed(ctx, kc)
	if err != nil {
		return phase.Status{}, err
	}
	switch m, diffs := providers.Compare(p.wanted, installed); m {
	case providers.None:
		return phase.Status{State: phase.Next}, nil
	case providers.Exact:
		return phase.Status{State: phase.Done}, nil
	default:
		return phase.Status{State: phase.Blocked, Reason: "the Bootstrap cluster's providers differ from the config: " +
			providers.Summary(diffs) + ". A Bootstrap cluster holds nothing yet: delete its container and run again"}, nil
	}
}

func (p *providersPhase) Run(ctx context.Context) error {
	kc, blocked, err := p.kubeconfig(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	return p.installer.Init(ctx, kc, p.wanted)
}

func deref(s *phase.Status) phase.Status {
	if s == nil {
		return phase.Status{}
	}
	return *s
}
