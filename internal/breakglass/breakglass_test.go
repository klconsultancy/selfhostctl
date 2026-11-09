package breakglass

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klconsultancy/selfhostctl/internal/recovery"
)

const manifests = `
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata: {name: mgmt, namespace: mgmt}
spec:
  controlPlaneRef: {apiGroup: controlplane.cluster.x-k8s.io, kind: K0sControlPlane, name: mgmt-cp}
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: K0sControlPlane
metadata: {name: mgmt-cp, namespace: mgmt}
spec:
  version: v1.36.5+k0s.0
  k0sConfigSpec:
    args:
      - --enable-worker
      - --no-taints
      - --disable-components=konnectivity-server,metrics-server
      - --kubelet-extra-args=--max-pods=200
    k0s:
      apiVersion: k0s.k0sproject.io/v1beta1
      kind: ClusterConfig
      spec:
        api:
          port: 7443
          extraArgs:
            anonymous-auth: "true"
            encryption-provider-config: /etc/k0s/encryption-config.yaml
        storage:
          type: kine
          kine:
            dataSource: postgres://from-the-manifest
            extraArgs: {metrics-bind-address: "0"}
        network:
          podCIDR: 10.244.0.0/16
          serviceCIDR: 10.96.0.0/12
          provider: calico
        telemetry: {enabled: false}
`

func load(t *testing.T, docs string) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for d := range strings.SplitSeq(docs, "\n---\n") {
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(d), &u.Object); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

var secrets = Secrets{
	Bundle: &recovery.Bundle{
		Endpoint:       recovery.Endpoint{Host: "10.0.0.10", Port: 6443},
		CA:             recovery.KeyPair{Cert: "ca-cert", Key: "ca-key"},
		ServiceAccount: recovery.KeyPair{Cert: "sa-pub", Key: "sa-key"},
		FrontProxyCA:   recovery.KeyPair{Cert: "fp-cert", Key: "fp-key"},
	},
	Datasource:    "postgres://from-the-environment",
	EncryptionKey: "c2VjcmV0",
}

func TestDerive(t *testing.T) {
	kcp, err := Find(load(t, manifests), "mgmt", "mgmt")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Derive(kcp, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "v1.36.5+k0s.0" || Image(c.Version) != "docker.io/k0sproject/k0s:v1.36.5-k0s.0" {
		t.Errorf("version %s, image %s", c.Version, Image(c.Version))
	}
	wantArgs := []string{"controller", "--config=/etc/k0s/k0s.yaml", "--enable-dynamic-config", "--disable-components=konnectivity-server,metrics-server"}
	if !slices.Equal(c.Args, wantArgs) {
		t.Errorf("args %v, want %v", c.Args, wantArgs)
	}

	var cfg map[string]any
	if err := yaml.Unmarshal(c.Files[ConfigPath], &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"apiVersion": "k0s.k0sproject.io/v1beta1",
		"kind":       "ClusterConfig",
		"metadata":   map[string]any{"name": "k0s"},
		"spec": map[string]any{
			// No port: the container publishes 6443. No telemetry, no network provider:
			// with dynamic config they come from the datastore.
			"api": map[string]any{
				"externalAddress": "10.0.0.10",
				"sans":            []any{"127.0.0.1", "localhost"},
				"extraArgs":       map[string]any{"anonymous-auth": "true", "encryption-provider-config": "/etc/k0s/encryption-config.yaml"},
			},
			"storage": map[string]any{"type": "kine", "kine": map[string]any{
				"dataSource": "postgres://from-the-environment",
				"extraArgs":  map[string]any{"metrics-bind-address": "0"},
			}},
			"network": map[string]any{"podCIDR": "10.244.0.0/16", "serviceCIDR": "10.96.0.0/12"},
		},
	}
	got, _ := yaml.Marshal(cfg)
	wantYAML, _ := yaml.Marshal(want)
	if string(got) != string(wantYAML) {
		t.Errorf("k0s.yaml:\n%s\nwant:\n%s", got, wantYAML)
	}

	enc := string(c.Files["/etc/k0s/encryption-config.yaml"])
	if !strings.Contains(enc, "aescbc") || !strings.Contains(enc, "name: key1") || !strings.Contains(enc, `secret: "c2VjcmV0"`) {
		t.Errorf("encryption config:\n%s", enc)
	}
	for path, want := range map[string]string{
		"/var/lib/k0s/pki/ca.crt": "ca-cert", "/var/lib/k0s/pki/ca.key": "ca-key",
		"/var/lib/k0s/pki/sa.pub": "sa-pub", "/var/lib/k0s/pki/sa.key": "sa-key",
		"/var/lib/k0s/pki/front-proxy-ca.crt": "fp-cert", "/var/lib/k0s/pki/front-proxy-ca.key": "fp-key",
	} {
		if string(c.Files[path]) != want {
			t.Errorf("%s = %q, want %q", path, c.Files[path], want)
		}
	}
}

func TestDeriveAtTheLiveVersion(t *testing.T) {
	kcp, _ := Find(load(t, manifests), "mgmt", "mgmt")
	c, err := Derive(kcp, "v1.37.1+k0s.0", secrets)
	if err != nil || c.Version != "v1.37.1+k0s.0" {
		t.Fatalf("version %s, %v", c.Version, err)
	}
}

func TestDeriveRefuses(t *testing.T) {
	tests := []struct {
		name, from, to string
		secrets        func(Secrets) Secrets
		want           string
	}{
		{name: "not kine", from: "type: kine\n", to: "type: etcd\n", want: "storage.type kine"},
		{name: "no key", secrets: func(s Secrets) Secrets { s.EncryptionKey = ""; return s }, want: "no encryption key"},
		{name: "no data source", secrets: func(s Secrets) Secrets { s.Datasource = ""; return s }, want: "no kine data source"},
		{name: "no version", from: "version: v1.36.5+k0s.0\n", to: "\n", want: "no spec.version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kcp, err := Find(load(t, strings.Replace(manifests, tt.from, tt.to, 1)), "mgmt", "mgmt")
			if err != nil {
				t.Fatal(err)
			}
			s := secrets
			if tt.secrets != nil {
				s = tt.secrets(s)
			}
			if _, err := Derive(kcp, "", s); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err %v, want %q", err, tt.want)
			}
		})
	}
}

func TestFindNeedsAK0sControlPlane(t *testing.T) {
	objs := load(t, strings.Replace(manifests, "kind: K0sControlPlane, name", "kind: KubeadmControlPlane, name", 1))
	if _, err := Find(objs, "mgmt", "mgmt"); err == nil || !strings.Contains(err.Error(), "KubeadmControlPlane") {
		t.Errorf("err %v", err)
	}
	if _, err := Find(load(t, manifests), "mgmt", "other"); err == nil {
		t.Error("found a Cluster that isn't there")
	}
}

func TestDisabledSeparateArg(t *testing.T) {
	got := disabled([]string{"--disable-components", "metrics-server,konnectivity-server", "--enable-worker"})
	if !slices.Equal(got, []string{"konnectivity-server", "metrics-server"}) {
		t.Errorf("%v", got)
	}
}
