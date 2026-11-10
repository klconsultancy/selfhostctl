// Package infrastructure is the seam between `recover` and the infrastructure of the
// Machines it replaces (docs/design.md#recovery). An adapter runs before recover
// deletes the old Machines; the default, none, does nothing, which is what CAPD needs.
package infrastructure

import (
	"context"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// Machines are the old Machines recover replaces, in the cluster that holds them.
type Machines struct {
	Client    ctrlclient.Client
	Namespace string
	Names     []string
}

// Adapter readies the old Machines' infrastructure for their deletion. Like a phase:
// Status says whether it is ready (Done), would be after Run (Next), or isn't and
// can't be made so (Blocked, which blocks recover).
type Adapter interface {
	Status(ctx context.Context, m Machines) (phase.Status, error)
	Run(ctx context.Context, m Machines) error
}

// For is the config's adapter.
func For(i *config.Infrastructure) Adapter {
	return none{}
}

type none struct{}

func (none) Status(context.Context, Machines) (phase.Status, error) {
	return phase.Status{State: phase.Done, Reason: "adapter none"}, nil
}

func (none) Run(context.Context, Machines) error { return nil }
