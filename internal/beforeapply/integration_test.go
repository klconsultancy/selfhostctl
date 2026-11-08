//go:build integration

package beforeapply_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/klconsultancy/selfhostctl/internal/beforeapply"
	"github.com/klconsultancy/selfhostctl/internal/bootstrap"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/dockerhost"
	"github.com/klconsultancy/selfhostctl/internal/kube"
	"github.com/klconsultancy/selfhostctl/internal/phase"
)

const secret = "s3cret-token-value"

func TestPrerequisitesAgainstACluster(t *testing.T) {
	ctrllog.SetLogger(logr.Discard())
	ctx := t.Context()
	docker, err := dockerhost.New(dockerhost.OS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })

	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("secret.yaml", `apiVersion: v1
kind: Namespace
metadata:
  name: proxy
---
apiVersion: v1
kind: Secret
metadata:
  name: proxy-token
  namespace: proxy
stringData:
  token: ${PROXY_TOKEN}
`)
	// A name the API server rejects, echoing it in its error.
	write("invalid.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x-${BAD_NAME}\n  namespace: proxy\n")
	write("values.yaml", "endpoint: https://${PROXY_HOST}:8006\n")
	chart, _ := filepath.Abs("testdata/chart")
	name := fmt.Sprintf("ba%d", time.Now().Unix())
	write("m.yaml", `apiVersion: selfhostctl/v1alpha1
kind: ManagementCluster
metadata:
  name: `+name+`
providers:
  bootstrap: [{name: k0sproject-k0smotron, version: v2.3.0}]
  controlPlane: [{name: k0sproject-k0smotron, version: v2.3.0}]
  infrastructure: [{name: docker, version: v1.14.4}]
beforeApply:
  - manifest: {path: secret.yaml}
    clusters: [bootstrap]
  - helm: {chart: `+chart+`, release: prereq, namespace: proxy, values: values.yaml}
    clusters: [bootstrap]
  - manifest: {path: invalid.yaml}
    clusters: [management]
`)
	cfg, err := config.Load(filepath.Join(dir, "m.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PROXY_TOKEN": secret, "PROXY_HOST": "proxy.example", "BAD_NAME": "Not_A_Valid_Name_" + secret}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if err := cfg.CheckEnv(lookup); err != nil {
		t.Fatal(err)
	}

	// Only the Bootstrap cluster itself.
	cluster := bootstrap.BootstrapCluster(cfg, docker)
	t.Cleanup(func() {
		if err := cluster.Remove(context.Background()); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	deps := bootstrap.Deps{Docker: docker, NodesReady: bootstrap.LiveNodesReady}
	if err := phase.Run(ctx, bootstrap.Phases(cfg, deps)[:1], "", io.Discard); err != nil {
		t.Fatal(err)
	}
	kc, err := cluster.Kubeconfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	k, err := kube.Client(kc)
	if err != nil {
		t.Fatal(err)
	}

	a := beforeapply.Applier{Config: cfg, Lookup: lookup}
	for _, p := range cfg.BeforeApply[:2] {
		st, err := a.State(ctx, kc, p)
		if err != nil || st.Done {
			t.Fatalf("%s before: %+v %v, want not done", p.Name(), st, err)
		}
		if err := a.Apply(ctx, kc, p); err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
		if st, err := a.State(ctx, kc, p); err != nil || !st.Done {
			t.Fatalf("%s after: %+v %v, want done", p.Name(), st, err)
		}
	}

	var s corev1.Secret
	if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: "proxy", Name: "proxy-token"}, &s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["token"]) != secret {
		t.Errorf("secret token %q, want the env's value", s.Data["token"])
	}
	var cm corev1.ConfigMap
	if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: "proxy", Name: "prereq"}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["endpoint"] != "https://proxy.example:8006" {
		t.Errorf("chart's ConfigMap %v, want the substituted value", cm.Data)
	}

	// Applying again changes nothing.
	rv := s.ResourceVersion
	if err := a.Apply(ctx, kc, cfg.BeforeApply[0]); err != nil {
		t.Fatal(err)
	}
	if err := k.Get(ctx, ctrlclient.ObjectKeyFromObject(&s), &s); err != nil || s.ResourceVersion != rv {
		t.Errorf("a second apply changed the Secret: resourceVersion %s -> %s (%v)", rv, s.ResourceVersion, err)
	}

	// The API server's error echoes the name; the secret in it is redacted.
	err = a.Apply(ctx, kc, cfg.BeforeApply[2])
	if err == nil {
		t.Fatal("an invalid name was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error shows the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("the error doesn't show where the value was: %v", err)
	}
}
