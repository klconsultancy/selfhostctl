package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	configclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/kube"
)

// Clusterctl is the Installer on the clusterctl library.
type Clusterctl struct {
	// GitHubToken only lifts the GitHub API rate limit for clusterctl's release
	// downloads. It is the one value taken from the environment (GITHUB_TOKEN).
	GitHubToken string
	// WaitTimeout bounds the wait for each provider to be available.
	WaitTimeout time.Duration
}

func (c Clusterctl) Installed(ctx context.Context, kubeconfig []byte) ([]Installed, error) {
	k, err := kube.Client(kubeconfig, clusterctlv1.AddToScheme)
	if err != nil {
		return nil, err
	}
	var list clusterctlv1.ProviderList
	if err := k.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil // no clusterctl inventory yet
		}
		return nil, fmt.Errorf("list providers: %w", err)
	}
	out := make([]Installed, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, Installed{
			Provider:    Provider{Type: clusterctlv1.ProviderType(p.Type), Name: p.ProviderName, Version: p.Version},
			Fingerprint: p.Annotations[FingerprintAnnotation],
		})
	}
	return out, nil
}

func (c Clusterctl) Init(ctx context.Context, kubeconfig []byte, w Wanted) error {
	path, done, err := kubeconfigFile(kubeconfig)
	if err != nil {
		return err
	}
	defer done()
	cc, err := c.client(ctx, w)
	if err != nil {
		return err
	}
	opts := clusterctlclient.InitOptions{
		Kubeconfig:          clusterctlclient.Kubeconfig{Path: path},
		WaitProviders:       true,
		WaitProviderTimeout: c.WaitTimeout,
	}
	for _, p := range w.Providers {
		ref := p.Name + ":" + p.Version
		switch p.Type {
		case clusterctlv1.CoreProviderType:
			opts.CoreProvider = ref
		case clusterctlv1.BootstrapProviderType:
			opts.BootstrapProviders = append(opts.BootstrapProviders, ref)
		case clusterctlv1.ControlPlaneProviderType:
			opts.ControlPlaneProviders = append(opts.ControlPlaneProviders, ref)
		case clusterctlv1.InfrastructureProviderType:
			opts.InfrastructureProviders = append(opts.InfrastructureProviders, ref)
		case clusterctlv1.IPAMProviderType:
			opts.IPAMProviders = append(opts.IPAMProviders, ref)
		case clusterctlv1.AddonProviderType:
			opts.AddonProviders = append(opts.AddonProviders, ref)
		}
	}
	if _, err := cc.Init(ctx, opts); err != nil {
		return fmt.Errorf("clusterctl init: %w", err)
	}
	return c.annotate(ctx, kubeconfig, Fingerprint(w.Variables))
}

// kubeconfigFile writes kubeconfig to a private file for the library, which takes a
// path; done removes it.
func kubeconfigFile(kubeconfig []byte) (path string, done func(), err error) {
	dir, err := os.MkdirTemp("", "selfhostctl-")
	if err != nil {
		return "", nil, err
	}
	done = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		done()
		return "", nil, err
	}
	return path, done, nil
}

// Upgrade runs clusterctl's upgrade for the steps alone, each at its version, with w's
// variables; clusterctl also brings cert-manager to the version it ships. Then every
// provider is stamped with w's fingerprint.
func (c Clusterctl) Upgrade(ctx context.Context, kubeconfig []byte, w Wanted, steps []Step) error {
	path, done, err := kubeconfigFile(kubeconfig)
	if err != nil {
		return err
	}
	defer done()
	cc, err := c.client(ctx, w)
	if err != nil {
		return err
	}
	opts := clusterctlclient.ApplyUpgradeOptions{
		Kubeconfig:          clusterctlclient.Kubeconfig{Path: path},
		WaitProviders:       true,
		WaitProviderTimeout: c.WaitTimeout,
	}
	for _, s := range steps {
		ref := s.Name + ":" + s.Version
		switch s.Type {
		case clusterctlv1.CoreProviderType:
			opts.CoreProvider = ref
		case clusterctlv1.BootstrapProviderType:
			opts.BootstrapProviders = append(opts.BootstrapProviders, ref)
		case clusterctlv1.ControlPlaneProviderType:
			opts.ControlPlaneProviders = append(opts.ControlPlaneProviders, ref)
		case clusterctlv1.InfrastructureProviderType:
			opts.InfrastructureProviders = append(opts.InfrastructureProviders, ref)
		case clusterctlv1.IPAMProviderType:
			opts.IPAMProviders = append(opts.IPAMProviders, ref)
		case clusterctlv1.AddonProviderType:
			opts.AddonProviders = append(opts.AddonProviders, ref)
		}
	}
	if err := cc.ApplyUpgrade(ctx, opts); err != nil {
		return fmt.Errorf("clusterctl upgrade: %w", err)
	}
	return c.annotate(ctx, kubeconfig, Fingerprint(w.Variables))
}

// client is a clusterctl client whose variables and provider list come from Wanted
// alone, never from the environment or a clusterctl.yaml.
func (c Clusterctl) client(ctx context.Context, w Wanted) (clusterctlclient.Client, error) {
	reader := configclient.NewMemoryReader()
	for k, v := range w.Variables {
		reader.Set(k, v)
	}
	if c.GitHubToken != "" {
		reader.Set(configclient.GitHubTokenVariable, c.GitHubToken)
	}
	for _, p := range w.Providers {
		if p.URL == "" {
			continue
		}
		if _, err := reader.AddProvider(p.Name, p.Type, p.URL); err != nil {
			return nil, err
		}
	}
	cfg, err := configclient.New(ctx, "", configclient.InjectReader(reader))
	if err != nil {
		return nil, err
	}
	return clusterctlclient.New(ctx, "", clusterctlclient.InjectConfig(cfg))
}

// Adopt stamps a cluster's providers with w's fingerprint, after checking that their
// versions are what w declares: the operator asserts they run with w's variables.
func (c Clusterctl) Adopt(ctx context.Context, kubeconfig []byte, w Wanted) error {
	installed, err := c.Installed(ctx, kubeconfig)
	if err != nil {
		return err
	}
	if len(installed) == 0 {
		return fmt.Errorf("the cluster has no providers")
	}
	if diffs := VersionsMatch(w, installed); len(diffs) > 0 {
		return fmt.Errorf("the cluster's providers differ from the config, so they can't be adopted: %s", Summary(diffs))
	}
	return c.annotate(ctx, kubeconfig, Fingerprint(w.Variables))
}

// Move runs clusterctl move for one namespace, from one cluster to another.
func (c Clusterctl) Move(ctx context.Context, from, to []byte, namespace string, dryRun bool) error {
	dir, err := os.MkdirTemp("", "selfhostctl-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	fromPath, toPath := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	if err := os.WriteFile(fromPath, from, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(toPath, to, 0o600); err != nil {
		return err
	}
	// Move reads no variables; the client only needs its config to exist.
	cc, err := c.client(ctx, Wanted{})
	if err != nil {
		return err
	}
	if err := cc.Move(ctx, clusterctlclient.MoveOptions{
		FromKubeconfig: clusterctlclient.Kubeconfig{Path: fromPath},
		ToKubeconfig:   clusterctlclient.Kubeconfig{Path: toPath},
		Namespace:      namespace,
		DryRun:         dryRun,
	}); err != nil {
		return fmt.Errorf("clusterctl move: %w", err)
	}
	return nil
}

// annotate records the variables' fingerprint on every Provider object.
func (c Clusterctl) annotate(ctx context.Context, kubeconfig []byte, fp string) error {
	k, err := kube.Client(kubeconfig, clusterctlv1.AddToScheme)
	if err != nil {
		return err
	}
	var list clusterctlv1.ProviderList
	if err := k.List(ctx, &list); err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	for i := range list.Items {
		p := &list.Items[i]
		patch := ctrlclient.MergeFrom(p.DeepCopy())
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[FingerprintAnnotation] = fp
		if err := k.Patch(ctx, p, patch); err != nil {
			return fmt.Errorf("annotate provider %s: %w", p.Name, err)
		}
	}
	return nil
}
