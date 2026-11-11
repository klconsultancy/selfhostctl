package bootstrap

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

// UpgradePhases are the phases of `selfhostctl upgrade`: the live Management
// cluster's providers brought to the config's versions and variables
// (docs/design.md#upgrade).
func UpgradePhases(cfg *config.ManagementCluster, deps Deps) []phase.Phase {
	deps.defaults()
	loc := newLocator(cfg, deps)
	phases := []phase.Phase{
		&upgradeGatePhase{loc: loc},
		&upgradeProvidersPhase{loc: loc, wanted: providers.FromConfig(&cfg.Providers), installer: deps.Providers, upgrader: deps.Upgrader},
	}
	for i, p := range phases {
		phases[i] = tracked{p, loc}
	}
	return phases
}

// upgradeGatePhase: the Management cluster manages itself and answers.
type upgradeGatePhase struct {
	loc *locator
}

func (p *upgradeGatePhase) Name() string { return "management" }

func (p *upgradeGatePhase) Status(ctx context.Context) (phase.Status, error) {
	if _, blocked, err := upgradeKubeconfig(ctx, p.loc); err != nil || blocked != nil {
		return deref(blocked), err
	}
	s, err := p.loc.get(ctx)
	if err != nil {
		return phase.Status{}, err
	}
	if s.Recovering() {
		return phase.Status{State: phase.Done, Reason: "a Recovery waits to move the objects back to the Management cluster, which answers"}, nil
	}
	return phase.Status{State: phase.Done, Reason: "the Management cluster manages itself"}, nil
}

func (p *upgradeGatePhase) Run(context.Context) error {
	return errors.New("the management gate has nothing to run")
}

// upgradeKubeconfig is the Management cluster's kubeconfig once it manages itself, or
// during a Recovery once its new machines answer and the objects wait in the
// Bootstrap cluster to move back: `move-to-management` needs the same providers on
// both sides, and no Cluster on the Management cluster is reconciled meanwhile.
func upgradeKubeconfig(ctx context.Context, loc *locator) ([]byte, *phase.Status, error) {
	s, err := loc.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	if s.Recovering() {
		if s.BreakGlassExists || s.Location != LocationBootstrap || s.ManagementKC == nil {
			return nil, &phase.Status{State: phase.Blocked, Reason: "a Recovery is in progress: run `selfhostctl recover`; upgrade only runs once recover's move-to-management asks for it"}, nil
		}
		return s.ManagementKC, nil, nil
	}
	if s.Location != LocationManagement || s.TargetPaused {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Management cluster doesn't manage itself (location " + s.Location.String() + "): finish `selfhostctl bootstrap` first"}, nil
	}
	kc, blocked := managementKC(s, "kubeconfig")
	return kc, blocked, nil
}

// upgradeProvidersPhase upgrades the providers that differ from the config.
type upgradeProvidersPhase struct {
	loc       *locator
	wanted    providers.Wanted
	installer providers.Installer
	upgrader  providers.Upgrader
}

func (p *upgradeProvidersPhase) Name() string { return "providers" }

func (p *upgradeProvidersPhase) plan(ctx context.Context) ([]byte, []providers.Step, []providers.Installed, *phase.Status, error) {
	kc, blocked, err := upgradeKubeconfig(ctx, p.loc)
	if err != nil || blocked != nil {
		return nil, nil, nil, blocked, err
	}
	installed, err := p.installer.Installed(ctx, kc)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(installed) == 0 {
		return nil, nil, nil, &phase.Status{State: phase.Blocked, Reason: "the Management cluster has no providers"}, nil
	}
	steps, why, adopt := providers.Plan(p.wanted, installed)
	switch {
	case len(why) > 0:
		return nil, nil, nil, &phase.Status{State: phase.Blocked, Reason: providers.Summary(why)}, nil
	case adopt:
		return nil, nil, nil, &phase.Status{State: phase.Manual, Reason: "the Management cluster's providers carry no selfhostctl fingerprint: " + adoptInstruction}, nil
	}
	return kc, steps, installed, nil, nil
}

func (p *upgradeProvidersPhase) Status(ctx context.Context) (phase.Status, error) {
	_, steps, installed, blocked, err := p.plan(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	if len(steps) == 0 {
		versions := make([]string, 0, len(installed))
		for _, i := range installed {
			versions = append(versions, i.String())
		}
		slices.Sort(versions)
		return phase.Status{State: phase.Done, Reason: strings.Join(versions, ", ")}, nil
	}
	plan := make([]string, 0, len(steps))
	for _, s := range steps {
		plan = append(plan, s.String())
	}
	return phase.Status{State: phase.Next, Reason: strings.Join(plan, "; ") + "; cert-manager to the version clusterctl ships"}, nil
}

func (p *upgradeProvidersPhase) Run(ctx context.Context) error {
	kc, steps, _, blocked, err := p.plan(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return errors.New(blocked.Reason)
	}
	return p.upgrader.Upgrade(ctx, kc, p.wanted, steps)
}
