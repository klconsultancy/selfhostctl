//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/dockerhost"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

// The phase against a real Docker daemon (DOCKER_HOST or the active context): it
// creates the Bootstrap cluster, and a second run changes nothing.
func TestBootstrapClusterAgainstDocker(t *testing.T) {
	ctrllog.SetLogger(logr.Discard())
	docker, err := dockerhost.New(dockerhost.OS)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-registered first: the client closes after the container is gone.
	t.Cleanup(func() { _ = docker.Close() })

	name := fmt.Sprintf("it%d", time.Now().Unix())
	cfg, err := config.Parse("it.yaml", []byte("apiVersion: selfhostctl/v1alpha1\nkind: ManagementCluster\nmetadata:\n  name: "+name+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := BootstrapCluster(cfg, docker).Remove(context.Background()); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	deps := Deps{Docker: docker, NodesReady: LiveNodesReady}

	lines, err := phase.Report(t.Context(), Phases(cfg, deps))
	if err != nil || lines[0].Status.State != phase.Next {
		t.Fatalf("status before: %v, %v; want next", lines, err)
	}
	var out bytes.Buffer
	if err := phase.Run(t.Context(), Phases(cfg, deps), "", &out); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "bootstrap-cluster: running") {
		t.Errorf("first run didn't run the phase:\n%s", out.String())
	}
	out.Reset()
	if err := phase.Run(t.Context(), Phases(cfg, deps), "", &out); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if strings.Contains(out.String(), "running") {
		t.Errorf("second run ran something:\n%s", out.String())
	}
	lines, err = phase.Report(t.Context(), Phases(cfg, deps))
	if err != nil || lines[0].Status.State != phase.Done {
		t.Fatalf("status after: %v, %v; want done", lines, err)
	}
}
