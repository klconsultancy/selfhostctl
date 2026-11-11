// Package infrastructure is the seam between `recover` and the infrastructure of the
// Machines it replaces (docs/design.md#recovery). An adapter is asked twice: before
// anything moves, whether the old machines are really gone (Gone), and before recover
// deletes the old Machines, to ready their infrastructure for that (Status and Run).
// The default, none, does nothing, which is what CAPD needs.
package infrastructure

import (
	"context"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// Machines are the old Machines recover replaces, in the cluster that holds them.
type Machines struct {
	Client    ctrlclient.Client
	Namespace string
	Cluster   string
	Names     []string
}

// Adapter knows one infrastructure provider's machines.
//
// Gone is a gate: Done when the old machines can't come back, Blocked when they still
// exist. Status and Run are like a phase's: Status says whether the old Machines'
// infrastructure is ready for their deletion (Done), would be after Run (Next), or
// isn't and can't be made so (Blocked, which blocks recover).
type Adapter interface {
	Gone(ctx context.Context, m Machines) (phase.Status, error)
	Status(ctx context.Context, m Machines) (phase.Status, error)
	Run(ctx context.Context, m Machines) error
}

// For is the config's adapter.
func For(i *config.Infrastructure) Adapter {
	if i.AdapterName() == "capmox" {
		return capmox{timeout: 10 * time.Minute, poll: 2 * time.Second}
	}
	return none{}
}

type none struct{}

func (none) Gone(context.Context, Machines) (phase.Status, error) {
	return phase.Status{State: phase.Done, Reason: "adapter none"}, nil
}

func (none) Status(context.Context, Machines) (phase.Status, error) {
	return phase.Status{State: phase.Done, Reason: "adapter none"}, nil
}

func (none) Run(context.Context, Machines) error { return nil }
