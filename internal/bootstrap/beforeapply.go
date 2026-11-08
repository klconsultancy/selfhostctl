package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/klconsultancy/selfhostctl/internal/beforeapply"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// Prerequisites judges and installs beforeApply entries; beforeapply.Applier
// outside tests.
type Prerequisites interface {
	State(ctx context.Context, kubeconfig []byte, p config.Prerequisite) (beforeapply.State, error)
	Apply(ctx context.Context, kubeconfig []byte, p config.Prerequisite) error
}

// beforeApplyPhase installs the beforeApply entries meant for the Bootstrap cluster.
type beforeApplyPhase struct {
	cluster *k0scontainer.Cluster
	entries []config.Prerequisite
	prereqs Prerequisites
}

func newBeforeApplyPhase(cfg *config.ManagementCluster, deps Deps) *beforeApplyPhase {
	var entries []config.Prerequisite
	for _, p := range cfg.BeforeApply {
		if p.IsOn(config.OnBootstrap) {
			entries = append(entries, p)
		}
	}
	return &beforeApplyPhase{cluster: BootstrapCluster(cfg, deps.Docker), entries: entries, prereqs: deps.Prerequisites}
}

func (p *beforeApplyPhase) Name() string { return "before-apply" }

func (p *beforeApplyPhase) kubeconfig(ctx context.Context) ([]byte, string, error) {
	o, err := p.cluster.Inspect(ctx)
	if err != nil {
		return nil, "", err
	}
	if !o.Running {
		return nil, "the Bootstrap cluster doesn't run (phase bootstrap-cluster)", nil
	}
	kc, err := p.cluster.Kubeconfig(ctx)
	if errors.Is(err, k0scontainer.ErrNotReady) {
		return nil, "the Bootstrap cluster isn't ready yet (phase bootstrap-cluster)", nil
	}
	return kc, "", err
}

func (p *beforeApplyPhase) Status(ctx context.Context) (phase.Status, error) {
	if len(p.entries) == 0 {
		return phase.Status{State: phase.Done}, nil
	}
	kc, blocked, err := p.kubeconfig(ctx)
	if err != nil || blocked != "" {
		return phase.Status{State: phase.Blocked, Reason: blocked}, err
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
		if !st.Done {
			next = true
		}
	}
	if next {
		return phase.Status{State: phase.Next}, nil
	}
	return phase.Status{State: phase.Done}, nil
}

// Run installs, in order, every entry that isn't done.
func (p *beforeApplyPhase) Run(ctx context.Context) error {
	kc, blocked, err := p.kubeconfig(ctx)
	if err != nil {
		return err
	}
	if blocked != "" {
		return errors.New(blocked)
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
