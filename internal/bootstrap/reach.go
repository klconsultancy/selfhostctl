package bootstrap

import (
	"context"
	"errors"

	"github.com/klconsultancy/selfhostctl/internal/k0scontainer"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// reach returns the Bootstrap cluster's kubeconfig, or the Blocked status of a phase
// that needs it while it isn't up.
func reach(ctx context.Context, cluster *k0scontainer.Cluster) ([]byte, *phase.Status, error) {
	o, err := cluster.Inspect(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !o.Running {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Bootstrap cluster doesn't run (phase bootstrap-cluster)"}, nil
	}
	kc, err := cluster.Kubeconfig(ctx)
	if errors.Is(err, k0scontainer.ErrNotReady) {
		return nil, &phase.Status{State: phase.Blocked, Reason: "the Bootstrap cluster isn't ready yet (phase bootstrap-cluster)"}, nil
	}
	return kc, nil, err
}

// mustReach is reach for Run: a missing Bootstrap cluster is an error there.
func mustReach(ctx context.Context, cluster *k0scontainer.Cluster) ([]byte, error) {
	kc, blocked, err := reach(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if blocked != nil {
		return nil, errors.New(blocked.Reason)
	}
	return kc, nil
}
