//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/dockerhost"
	"github.com/klconsultancy/selfhostctl/internal/kube"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
)

// bootstrap's phases against a real Docker daemon (DOCKER_HOST or the active context)
// and clusterctl's real releases: the Bootstrap cluster and its providers, then a
// second run that changes nothing.
func TestBootstrapAgainstDocker(t *testing.T) {
	ctrllog.SetLogger(logr.Discard())
	// The operator's environment must not reach clusterctl: the config turns the gate on.
	t.Setenv("CLUSTER_TOPOLOGY", "false")

	docker, err := dockerhost.New(dockerhost.OS)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-registered first: the client closes after the container is gone.
	t.Cleanup(func() { _ = docker.Close() })

	name := fmt.Sprintf("it%d", time.Now().Unix())
	cfg, err := config.Parse("it.yaml", []byte(`
apiVersion: selfhostctl/v1alpha1
kind: ManagementCluster
metadata:
  name: `+name+`
providers:
  clusterTopology: true
  bootstrap: [{name: k0sproject-k0smotron, version: v2.3.0}]
  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]
  # An explicit URL, as for a fork: the version replaces "latest".
  infrastructure:
    - name: docker
      version: v1.14.4
      url: https://github.com/kubernetes-sigs/cluster-api/releases/latest/infrastructure-components-development.yaml
cluster:
  manifests: cluster.yaml
  namespace: mgmt
`))
	if err != nil {
		t.Fatal(err)
	}
	cluster := BootstrapCluster(cfg, docker)
	t.Cleanup(func() {
		if err := cluster.Remove(context.Background()); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	deps := Deps{
		Docker:     docker,
		NodesReady: LiveNodesReady,
		Providers:  providers.Clusterctl{GitHubToken: os.Getenv("GITHUB_TOKEN"), WaitTimeout: 10 * time.Minute},
		// It starts from nothing, without a kubeconfig.
		First: true,
	}

	states := func() string {
		lines, err := phase.Report(t.Context(), Phases(cfg, deps)[:3])
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var s []string
		for _, l := range lines {
			s = append(s, l.Phase+"="+l.Status.State.String())
		}
		return strings.Join(s, " ")
	}
	if got, want := states(), "bootstrap-cluster=next providers=blocked before-apply=done"; got != want {
		t.Errorf("status before: %s, want %s", got, want)
	}

	var out bytes.Buffer
	if err := phase.Run(t.Context(), Phases(cfg, deps)[:3], "", &out); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := phase.Run(t.Context(), Phases(cfg, deps)[:3], "", &out); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if strings.Contains(out.String(), "running") {
		t.Errorf("second run ran something:\n%s", out.String())
	}
	if got, want := states(), "bootstrap-cluster=done providers=done before-apply=done"; got != want {
		t.Errorf("status after: %s, want %s", got, want)
	}

	// What landed: the gate from the config, the fingerprint on every provider.
	kc, err := cluster.Kubeconfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	k, err := kube.Client(kc, clusterctlv1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	var capi appsv1.Deployment
	if err := k.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "capi-system", Name: "capi-controller-manager"}, &capi); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(capi.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "ClusterTopology=true") {
		t.Errorf("capi-controller-manager args %q lack ClusterTopology=true", args)
	}
	var list clusterctlv1.ProviderList
	if err := k.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	want := providers.Fingerprint(providers.FromConfig(&cfg.Providers).Variables)
	for _, p := range list.Items {
		if got := p.Annotations[providers.FingerprintAnnotation]; got != want {
			t.Errorf("provider %s: fingerprint %q, want %q", p.Name, got, want)
		}
	}
}
