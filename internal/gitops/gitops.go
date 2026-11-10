// Package gitops is the seam between `recover` and the GitOps tool the config names
// (docs/design.md#gitops). Flux is its one implementation.
package gitops

import (
	"context"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/flux"
)

// Tool stops and restarts a GitOps tool's reconciliation in a cluster. Each method
// returns the objects it is about, e.g. "Kustomization flux-system/flux-system".
type Tool interface {
	// Suspend stops everything that applies to the cluster, marking what it stopped.
	Suspend(ctx context.Context, c ctrlclient.Client) ([]string, error)
	// Resume restarts what Suspend marked; what the operator stopped stays stopped.
	Resume(ctx context.Context, c ctrlclient.Client) ([]string, error)
	// Suspended lists what is stopped, by Suspend or by the operator.
	Suspended(ctx context.Context, c ctrlclient.Client) ([]string, error)
	// Unsuspended lists what Suspend would still stop.
	Unsuspended(ctx context.Context, c ctrlclient.Client) ([]string, error)
}

// For is the config's GitOps tool; nil when the config has none.
func For(g *config.GitOps) Tool {
	if g == nil || g.Flux == nil {
		return nil
	}
	return fluxTool{root: g.Flux.FluxRoot()}
}

type fluxTool struct{ root config.ObjectRef }

func (f fluxTool) Suspend(ctx context.Context, c ctrlclient.Client) ([]string, error) {
	refs, err := flux.Suspend(ctx, c, f.root)
	return names(refs), err
}

func (f fluxTool) Resume(ctx context.Context, c ctrlclient.Client) ([]string, error) {
	refs, err := flux.Resume(ctx, c, f.root)
	return names(refs), err
}

func (fluxTool) Suspended(ctx context.Context, c ctrlclient.Client) ([]string, error) {
	refs, err := flux.Suspended(ctx, c)
	return names(refs), err
}

func (fluxTool) Unsuspended(ctx context.Context, c ctrlclient.Client) ([]string, error) {
	refs, err := flux.Unsuspended(ctx, c)
	return names(refs), err
}

func names(refs []flux.Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	return out
}
