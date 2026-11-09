package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/apply"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/envsubst"
	"github.com/klconsultancy/selfhostctl/internal/flux"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// GitOpsPhase names the phase `gitops sync` runs alone.
const GitOpsPhase = "gitops"

// gitopsPhase gives Flux its git credential and waits until Flux synced the root
// Kustomization from a fresh fetch (docs/design.md#gitops). Flux comes from the
// cluster's own definition.
type gitopsPhase struct {
	cfg *config.ManagementCluster
	// kc is the Management cluster's kubeconfig once it manages itself.
	kc      func(ctx context.Context) ([]byte, *phase.Status, error)
	clients Clients
	lookup  func(string) (string, bool)
	now     func() time.Time
	// timeout bounds each wait: for Flux's CRDs, then for the sync.
	timeout, poll time.Duration
}

func (p *gitopsPhase) Name() string { return GitOpsPhase }

// credential is the Secret to apply and a redactor that hides the key.
func (p *gitopsPhase) credential() (*unstructured.Unstructured, apply.Redactor, error) {
	cs := p.cfg.GitOps.Flux.CredentialSecret
	identity, _ := p.lookup(cs.FromEnv)
	knownHosts, err := os.ReadFile(p.cfg.Path(cs.KnownHosts))
	if err != nil {
		return nil, nil, fmt.Errorf("gitops.flux.credentialSecret.knownHosts: %w", err)
	}
	redact := func(s string) string { return envsubst.Redact(s, []string{identity}) }
	// A backstop to the check at load: an empty value never reaches the cluster.
	s, err := flux.Credential{Namespace: cs.Namespace, Name: cs.Name, Identity: identity, KnownHosts: string(knownHosts)}.Secret()
	if err != nil {
		return nil, nil, fmt.Errorf("%w (%s)", err, cs.FromEnv)
	}
	return s, redact, nil
}

func (p *gitopsPhase) client(ctx context.Context) (ctrlclient.Client, *phase.Status, error) {
	kc, blocked, err := p.kc(ctx)
	if err != nil || blocked != nil {
		return nil, blocked, err
	}
	c, err := p.clients(kc)
	return c, nil, err
}

func (p *gitopsPhase) Status(ctx context.Context) (phase.Status, error) {
	if p.cfg.GitOps == nil {
		return phase.Status{State: phase.Done, Reason: "no gitops in the config"}, nil
	}
	c, blocked, err := p.client(ctx)
	if err != nil || blocked != nil {
		return deref(blocked), err
	}
	secret, redact, err := p.credential()
	if err != nil {
		return phase.Status{}, err
	}
	inPlace, err := secretHolds(ctx, c, secret)
	if err != nil {
		return phase.Status{}, errors.New(redact(err.Error()))
	}
	s, err := flux.State(ctx, c, p.cfg.GitOps.Flux.FluxRoot())
	if errors.Is(err, flux.ErrNoFlux) || (err == nil && (!inPlace || s.Waiting != "")) {
		return phase.Status{State: phase.Next}, nil
	}
	if err != nil {
		return phase.Status{}, err
	}
	return phase.Status{State: phase.Done, Reason: "synced " + s.Revision}, nil
}

// secretHolds reports whether the live Secret already holds every field of want.
func secretHolds(ctx context.Context, c ctrlclient.Client, want *unstructured.Unstructured) (bool, error) {
	live := &corev1.Secret{}
	err := c.Get(ctx, ctrlclient.ObjectKeyFromObject(want), live)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, _, _ := unstructured.NestedStringMap(want.Object, "data")
	for k, v := range data {
		if base64.StdEncoding.EncodeToString(live.Data[k]) != v {
			return false, nil
		}
	}
	return true, nil
}

func (p *gitopsPhase) Run(ctx context.Context) error {
	c, blocked, err := p.client(ctx)
	if err != nil {
		return err
	}
	if blocked != nil {
		return &phase.BlockedError{Phase: p.Name(), Reason: blocked.Reason}
	}
	root := p.cfg.GitOps.Flux.FluxRoot()
	// Flux's CRDs come with the cluster's own install of it.
	deadline := time.Now().Add(p.timeout)
	for {
		_, err := flux.State(ctx, c, root)
		if !errors.Is(err, flux.ErrNoFlux) {
			break
		}
		if time.Now().After(deadline) {
			return &phase.BlockedError{Phase: p.Name(), Reason: flux.ErrNoFlux.Error()}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.poll):
		}
	}
	secret, redact, err := p.credential()
	if err != nil {
		return err
	}
	if err := apply.Objects(ctx, c, []*unstructured.Unstructured{secret}, redact); err != nil {
		return err
	}
	at := p.now().UTC().Format(time.RFC3339Nano)
	if err := flux.Reconcile(ctx, c, root, at, p.timeout, p.poll); err != nil {
		return errors.New(redact(err.Error()))
	}
	return nil
}
