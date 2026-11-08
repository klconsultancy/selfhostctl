// Package beforeapply installs the config's beforeApply prerequisites on a cluster:
// manifests applied server-side and Helm charts installed or upgraded, both with
// ${VAR} from the environment. The same code serves the Bootstrap cluster and, later,
// the Management cluster itself.
package beforeapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klconsultancy/selfhostctl/internal/apply"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/envsubst"
	selfkube "github.com/klconsultancy/selfhostctl/internal/kube"
)

// State of one prerequisite on one cluster.
type State struct {
	Done bool
	// Blocked is set when the prerequisite can't be brought to done by running.
	Blocked string
}

// Applier judges and installs prerequisites.
type Applier struct {
	Config *config.ManagementCluster
	// Lookup is os.LookupEnv outside tests.
	Lookup func(string) (string, bool)
	// HelmTimeout bounds a chart's install or upgrade, its wait included.
	HelmTimeout time.Duration
}

// State judges p on the cluster behind kubeconfig, changing nothing.
func (a Applier) State(ctx context.Context, kubeconfig []byte, p config.Prerequisite) (State, error) {
	switch {
	case p.Manifest != nil:
		return a.manifestState(ctx, kubeconfig, p.Manifest)
	case p.Helm != nil:
		return a.helmState(kubeconfig, p.Helm)
	}
	return State{}, errors.New("a prerequisite with neither manifest nor helm")
}

// Apply installs p on the cluster behind kubeconfig.
func (a Applier) Apply(ctx context.Context, kubeconfig []byte, p config.Prerequisite) error {
	switch {
	case p.Manifest != nil:
		return a.applyManifest(ctx, kubeconfig, p.Manifest)
	case p.Helm != nil:
		return a.applyChart(ctx, kubeconfig, p.Helm)
	}
	return errors.New("a prerequisite with neither manifest nor helm")
}

// expandFile reads a file the config names and substitutes ${VAR}.
func (a Applier) expandFile(name string) (string, []string, error) {
	data, err := os.ReadFile(a.Config.Path(name))
	if err != nil {
		return "", nil, err
	}
	out, values, err := envsubst.Expand(string(data), a.Lookup)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, values, nil
}

// --- manifests ---

func (a Applier) objects(m *config.Manifest) ([]*unstructured.Unstructured, apply.Redactor, error) {
	text, values, err := a.expandFile(m.Path)
	if err != nil {
		return nil, nil, err
	}
	redact := func(s string) string { return envsubst.Redact(s, values) }
	objs, err := apply.Decode(strings.NewReader(text), redact)
	if err != nil {
		return nil, redact, fmt.Errorf("%s: %w", m.Path, err)
	}
	return objs, redact, nil
}

func (a Applier) manifestState(ctx context.Context, kubeconfig []byte, m *config.Manifest) (State, error) {
	objs, redact, err := a.objects(m)
	if err != nil {
		return State{}, err
	}
	c, err := selfkube.Client(kubeconfig)
	if err != nil {
		return State{}, err
	}
	done, err := apply.InPlace(ctx, c, objs, redact)
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", m.Path, err)
	}
	return State{Done: done}, nil
}

func (a Applier) applyManifest(ctx context.Context, kubeconfig []byte, m *config.Manifest) error {
	objs, redact, err := a.objects(m)
	if err != nil {
		return err
	}
	c, err := selfkube.Client(kubeconfig)
	if err != nil {
		return err
	}
	if err := apply.Objects(ctx, c, objs, redact); err != nil {
		return fmt.Errorf("%s: %w", m.Path, err)
	}
	return nil
}

// redacted formats an error and then removes every substituted value from all of it:
// object names and API messages alike can carry one.
func redacted(values []string, format string, args ...any) error {
	return errors.New(envsubst.Redact(fmt.Sprintf(format, args...), values))
}

// --- Helm ---

// helm is a Helm configuration for one cluster and namespace. Helm's SDK takes a
// kubeconfig path; the file lives until done is called.
func helmConfig(kubeconfig []byte, namespace string) (*action.Configuration, *cli.EnvSettings, func(), error) {
	dir, err := os.MkdirTemp("", "selfhostctl-helm-")
	if err != nil {
		return nil, nil, nil, err
	}
	done := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		done()
		return nil, nil, nil, err
	}
	settings := cli.New()
	settings.KubeConfig = path
	settings.SetNamespace(namespace)
	cfg := action.NewConfiguration()
	if err := cfg.Init(settings.RESTClientGetter(), namespace, "secret"); err != nil {
		done()
		return nil, nil, nil, err
	}
	reg, err := registry.NewClient(registry.ClientOptWriter(io.Discard), registry.ClientOptCredentialsFile(settings.RegistryConfig))
	if err != nil {
		done()
		return nil, nil, nil, err
	}
	cfg.RegistryClient = reg
	return cfg, settings, done, nil
}

// values reads the chart's values file, substituted, as Helm compares them.
func (a Applier) values(h *config.Helm) (map[string]any, []string, error) {
	if h.Values == "" {
		return map[string]any{}, nil, nil
	}
	text, secrets, err := a.expandFile(h.Values)
	if err != nil {
		return nil, nil, err
	}
	vals := map[string]any{}
	if err := yaml.Unmarshal([]byte(text), &vals); err != nil {
		return nil, secrets, redacted(secrets, "%s: %v", h.Values, err)
	}
	return normalize(vals), secrets, nil
}

// normalize gives values the shape Helm returns them in (JSON numbers, maps).
func normalize(v map[string]any) map[string]any {
	data, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	return out
}

func (a Applier) chartRef(h *config.Helm) string {
	if strings.HasPrefix(h.Chart, "oci://") || h.Repo != "" {
		return h.Chart
	}
	return a.Config.Path(h.Chart) // a local directory
}

func (a Applier) loadChart(cfg *action.Configuration, settings *cli.EnvSettings, h *config.Helm) (chart.Charter, error) {
	opts := action.ChartPathOptions{RepoURL: h.Repo, Version: h.Version}
	install := action.NewInstall(cfg) // only for its chart locator and registry client
	install.ChartPathOptions = opts
	path, err := install.LocateChart(a.chartRef(h), settings)
	if err != nil {
		return nil, fmt.Errorf("chart %s: %w", h.Chart, err)
	}
	return loader.Load(path)
}

func (a Applier) helmState(kubeconfig []byte, h *config.Helm) (State, error) {
	cfg, settings, done, err := helmConfig(kubeconfig, h.Namespace)
	if err != nil {
		return State{}, err
	}
	defer done()
	rel, err := action.NewGet(cfg).Run(h.Release)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("release %s: %w", h.Release, err)
	}
	acc, err := release.NewAccessor(rel)
	if err != nil {
		return State{}, err
	}
	switch status := acc.Status(); {
	case strings.HasPrefix(status, "pending"):
		return State{Blocked: fmt.Sprintf("release %s in namespace %s is %s: another Helm operation runs or was interrupted; finish or roll it back, then run again", h.Release, h.Namespace, status)}, nil
	case status != "deployed":
		return State{}, nil // failed or superseded: an upgrade fixes it
	}
	version := h.Version
	if version == "" {
		// A local chart: compare with its own version.
		ch, err := a.loadChart(cfg, settings, h)
		if err != nil {
			return State{}, err
		}
		version = chartVersion(ch)
	}
	if chartVersion(acc.Chart()) != version {
		return State{}, nil
	}
	want, _, err := a.values(h)
	if err != nil {
		return State{}, err
	}
	have, err := action.NewGetValues(cfg).Run(h.Release)
	if err != nil {
		return State{}, fmt.Errorf("release %s: %w", h.Release, err)
	}
	if !reflect.DeepEqual(normalize(have), want) {
		return State{}, nil
	}
	return State{Done: true}, nil
}

func chartVersion(ch chart.Charter) string {
	acc, err := chart.NewDefaultAccessor(ch)
	if err != nil {
		return ""
	}
	v, _ := acc.MetadataAsMap()["version"].(string)
	return v
}

func (a Applier) applyChart(ctx context.Context, kubeconfig []byte, h *config.Helm) error {
	cfg, settings, done, err := helmConfig(kubeconfig, h.Namespace)
	if err != nil {
		return err
	}
	defer done()
	vals, secrets, err := a.values(h)
	if err != nil {
		return err
	}
	ch, err := a.loadChart(cfg, settings, h)
	if err != nil {
		return err
	}
	timeout := a.HelmTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	_, err = action.NewGet(cfg).Run(h.Release)
	switch {
	case errors.Is(err, driver.ErrReleaseNotFound):
		install := action.NewInstall(cfg)
		install.ReleaseName = h.Release
		install.Namespace = h.Namespace
		install.CreateNamespace = true
		install.WaitStrategy = kube.StatusWatcherStrategy
		install.Timeout = timeout
		install.RollbackOnFailure = false
		_, err = install.RunWithContext(ctx, ch, vals)
	case err == nil:
		upgrade := action.NewUpgrade(cfg)
		upgrade.Namespace = h.Namespace
		upgrade.WaitStrategy = kube.StatusWatcherStrategy
		upgrade.Timeout = timeout
		upgrade.ServerSideApply = "auto"
		_, err = upgrade.RunWithContext(ctx, h.Release, ch, vals)
	}
	if err != nil {
		return redacted(secrets, "release %s: %v", h.Release, err)
	}
	return nil
}
