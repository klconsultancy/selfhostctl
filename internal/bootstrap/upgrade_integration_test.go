//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
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

// upgrade's clusterctl path against a real cluster and clusterctl's real releases:
// CAPD at v1.14.3 without the ClusterTopology gate, then the config at v1.14.4 with
// it. The core, at the same version, is installed again for the gate; a second plan
// is empty.
func TestUpgradeAgainstDocker(t *testing.T) {
	ctrllog.SetLogger(logr.Discard())
	t.Setenv("CLUSTER_TOPOLOGY", "false")

	docker, err := dockerhost.New(dockerhost.OS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })

	name := fmt.Sprintf("up%d", time.Now().Unix())
	parse := func(capd string, topology bool) *config.ManagementCluster {
		cfg, err := config.Parse("it.yaml", []byte(fmt.Sprintf(`
apiVersion: selfhostctl/v1alpha1
kind: ManagementCluster
metadata:
  name: %s
providers:
  clusterTopology: %t
  bootstrap: [{name: k0sproject-k0smotron, version: v2.3.0}]
  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]
  infrastructure:
    - name: docker
      version: %s
      url: https://github.com/kubernetes-sigs/cluster-api/releases/latest/infrastructure-components-development.yaml
cluster:
  manifests: cluster.yaml
  namespace: mgmt
`, name, topology, capd)))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	before, after := parse("v1.14.3", false), parse("v1.14.4", true)

	cluster := BootstrapCluster(before, docker)
	t.Cleanup(func() {
		if err := cluster.Remove(context.Background()); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	cc := providers.Clusterctl{GitHubToken: os.Getenv("GITHUB_TOKEN"), WaitTimeout: 10 * time.Minute}
	deps := Deps{Docker: docker, NodesReady: LiveNodesReady, Providers: cc, First: true}
	var out bytes.Buffer
	if err := phase.Run(t.Context(), Phases(before, deps)[:2], "", &out); err != nil {
		t.Fatalf("the cluster at v1.14.3: %v\n%s", err, out.String())
	}
	kc, err := cluster.Kubeconfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	wanted := providers.FromConfig(&after.Providers)
	installed, err := cc.Installed(t.Context(), kc)
	if err != nil {
		t.Fatal(err)
	}
	steps, blocked, adopt := providers.Plan(wanted, installed)
	if len(blocked) > 0 || adopt {
		t.Fatalf("plan blocked %v, adopt %v", blocked, adopt)
	}
	var plan []string
	for _, s := range steps {
		plan = append(plan, s.String())
	}
	if !slices.Contains(plan, "infrastructure-docker v1.14.3 → v1.14.4") || len(plan) != len(wanted.Providers) {
		t.Errorf("plan %q: want CAPD to v1.14.4 and every other provider again for the gate", plan)
	}
	if err := cc.Upgrade(t.Context(), kc, wanted, steps); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	installed, err = cc.Installed(t.Context(), kc)
	if err != nil {
		t.Fatal(err)
	}
	if m, diffs := providers.Compare(wanted, installed); m != providers.Exact {
		t.Errorf("after the upgrade: %v", diffs)
	}
	if steps, blocked, adopt := providers.Plan(wanted, installed); len(steps)+len(blocked) > 0 || adopt {
		t.Errorf("a second plan: steps %v, blocked %v, adopt %v", steps, blocked, adopt)
	}

	k, err := kube.Client(kc, clusterctlv1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []ctrlclient.ObjectKey{{Namespace: "capi-system", Name: "capi-controller-manager"}, {Namespace: "capd-system", Name: "capd-controller-manager"}} {
		var dep appsv1.Deployment
		if err := k.Get(t.Context(), d, &dep); err != nil {
			t.Fatal(err)
		}
		args := strings.Join(dep.Spec.Template.Spec.Containers[0].Args, " ")
		if !strings.Contains(args, "ClusterTopology=true") {
			t.Errorf("%s args %q lack ClusterTopology=true", d.Name, args)
		}
	}
}
