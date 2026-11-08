//go:build integration

// Package smoke proves that the libraries the tool is built on work together in one
// module (#3, docs/adr/0001-core-libraries.md): the Docker SDK runs a Bootstrap
// cluster (one k0s container), the clusterctl library installs providers on it,
// controller-runtime reads them back, and the Helm SDK installs a chart.
//
// Run with: go test -tags integration ./internal/smoke/
package smoke

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/klconsultancy/selfhostctl/internal/bootstrap"
)

const (
	// The core version this release line is built against (the support matrix).
	capiVersion = "v1.14.4"
	// The Bootstrap cluster's k0s, the one the tool pins.
	bootstrapImage = bootstrap.Image
)

func TestLibrariesTogether(t *testing.T) {
	ctx := t.Context()

	docker, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer docker.Close()
	if _, err := docker.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("docker ping: %v", err)
	}

	kubeconfig := bootstrapCluster(t, docker)

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := clusterctlv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := ctrlclient.New(restConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("controller-runtime client: %v", err)
	}
	waitFor(t, 5*time.Minute, "a Ready node", func() error { return nodeReady(ctx, c) })

	// clusterctl library: providers, with the inventory `move` relies on. Bootstrap and
	// control plane are named explicitly: on a first run the library adds kubeadm's
	// latest release for any that are left out.
	clusterctl, err := clusterctlclient.New(ctx, "")
	if err != nil {
		t.Fatalf("clusterctl client: %v", err)
	}
	if _, err := clusterctl.Init(ctx, clusterctlclient.InitOptions{
		Kubeconfig:            clusterctlclient.Kubeconfig{Path: kubeconfig},
		CoreProvider:          "cluster-api:" + capiVersion,
		BootstrapProviders:    []string{"kubeadm:" + capiVersion},
		ControlPlaneProviders: []string{"kubeadm:" + capiVersion},
		WaitProviders:         true,
		WaitProviderTimeout:   5 * time.Minute,
	}); err != nil {
		t.Fatalf("clusterctl init: %v", err)
	}

	var providers clusterctlv1.ProviderList
	if err := c.List(ctx, &providers); err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if got := len(providers.Items); got != 3 {
		t.Fatalf("providers in the inventory: %d, want 3", got)
	}
	for _, p := range providers.Items {
		if p.Version != capiVersion {
			t.Errorf("provider %s at %s, want %s", p.Name, p.Version, capiVersion)
		}
	}

	// Helm SDK: a chart installed and waited for.
	settings := cli.New()
	settings.KubeConfig = kubeconfig
	cfg := action.NewConfiguration()
	if err := cfg.Init(settings.RESTClientGetter(), "default", "secret"); err != nil {
		t.Fatalf("helm configuration: %v", err)
	}
	chart, err := loader.Load("testdata/smoke-chart")
	if err != nil {
		t.Fatalf("load chart: %v", err)
	}
	install := action.NewInstall(cfg)
	install.ReleaseName = "smoke"
	install.Namespace = "default"
	install.WaitStrategy = kube.StatusWatcherStrategy
	install.Timeout = 2 * time.Minute
	if _, err := install.RunWithContext(ctx, chart, map[string]any{"message": "hello"}); err != nil {
		t.Fatalf("helm install: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: "default", Name: "smoke"}, &cm); err != nil || cm.Data["message"] != "hello" {
		t.Fatalf("the chart's ConfigMap: %v %v", cm.Data, err)
	}
}

// bootstrapCluster runs one k0s container through the Docker SDK, its API published on
// a random localhost port, and returns the path of its admin kubeconfig.
func bootstrapCluster(t *testing.T, docker *client.Client) string {
	t.Helper()
	ctx := t.Context()

	pull, err := docker.ImagePull(ctx, bootstrapImage, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull %s: %v", bootstrapImage, err)
	}
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull %s: %v", bootstrapImage, err)
	}

	api := network.MustParsePort("6443/tcp")
	name := "selfhostctl-smoke-" + fmt.Sprint(time.Now().Unix())
	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        bootstrapImage,
			Hostname:     name,
			Cmd:          []string{"k0s", "controller", "--enable-worker", "--no-taints"},
			ExposedPorts: network.PortSet{api: {}},
			// k0s's own advice for k0s in Docker: its data dir off the overlay filesystem.
			Volumes: map[string]struct{}{"/var/lib/k0s": {}},
		},
		HostConfig: &container.HostConfig{
			Privileged:   true,
			Tmpfs:        map[string]string{"/run": ""},
			PortBindings: network.PortMap{api: {{HostIP: netip.MustParseAddr("127.0.0.1")}}},
		},
	})
	if err != nil {
		t.Fatalf("create the Bootstrap cluster: %v", err)
	}
	t.Cleanup(func() {
		// The test's context is already cancelled here.
		if _, err := docker.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			t.Errorf("remove the Bootstrap cluster: %v", err)
		}
	})
	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start the Bootstrap cluster: %v", err)
	}

	inspected, err := docker.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect the Bootstrap cluster: %v", err)
	}
	bindings := inspected.Container.NetworkSettings.Ports[api]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		t.Fatalf("the Bootstrap cluster's API port is not published: %v", inspected.Container.NetworkSettings.Ports)
	}

	var adminConf []byte
	waitFor(t, 3*time.Minute, "the Bootstrap cluster's admin.conf", func() error {
		adminConf, err = readFile(ctx, docker, created.ID, "/var/lib/k0s/pki/admin.conf")
		return err
	})
	adminConf = bytes.ReplaceAll(adminConf, []byte("https://localhost:6443"), []byte("https://127.0.0.1:"+bindings[0].HostPort))
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, adminConf, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// readFile reads one file out of a container, through the API's tar stream.
func readFile(ctx context.Context, docker *client.Client, id, path string) ([]byte, error) {
	res, err := docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, err
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(tr)
	if err == nil && len(data) == 0 {
		err = errors.New("empty")
	}
	return data, err
}

func nodeReady(ctx context.Context, c ctrlclient.Client) error {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return err
	}
	for _, n := range nodes.Items {
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				return nil
			}
		}
	}
	return errors.New("no Ready node yet")
}

func waitFor(t *testing.T, timeout time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not after %s: %v", what, timeout, strings.TrimSpace(err.Error()))
		}
		time.Sleep(3 * time.Second)
	}
}
