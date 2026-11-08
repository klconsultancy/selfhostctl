package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/recovery"
)

var k0sControlPlaneGVK = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Kind: "K0sControlPlane"}

func obj(gvk schema.GroupVersionKind, name string, content map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("mgmt")
	u.SetName(name)
	return u
}

func conditions(kv ...string) []any {
	var out []any
	for i := 0; i < len(kv); i += 2 {
		out = append(out, map[string]any{"type": kv[i], "status": kv[i+1]})
	}
	return out
}

func readyCluster(cpAvailable, nodeHealthy string, ready int64) []ctrlclient.Object {
	machine := obj(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}, "mgmt-abc",
		map[string]any{"status": map[string]any{"conditions": conditions("NodeHealthy", nodeHealthy)}})
	machine.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "mgmt"})
	return []ctrlclient.Object{
		obj(clusterGVK, "mgmt", map[string]any{
			"spec": map[string]any{
				"controlPlaneRef":      map[string]any{"apiGroup": "controlplane.cluster.x-k8s.io", "kind": "K0sControlPlane", "name": "mgmt"},
				"controlPlaneEndpoint": map[string]any{"host": "172.18.0.3", "port": int64(6443)},
			},
			"status": map[string]any{"conditions": conditions("ControlPlaneAvailable", cpAvailable)},
		}),
		obj(k0sControlPlaneGVK, "mgmt", map[string]any{"spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"readyReplicas": ready}}),
		machine,
	}
}

func fakeClients(t *testing.T, objs ...ctrlclient.Object) Clients {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{clusterGVK.GroupVersion(), k0sControlPlaneGVK.GroupVersion()})
	for _, gvk := range []schema.GroupVersionKind{clusterGVK, k0sControlPlaneGVK, {Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).Build()
	return func([]byte) (ctrlclient.Client, error) { return c, nil }
}

// running is a fake Docker whose Bootstrap cluster runs.
func running(t *testing.T) *fakeDocker {
	t.Helper()
	d := newFakeDocker()
	d.containers["selfhostctl-mgmt-bootstrap"] = &fakeContainer{image: Image, running: true}
	return d
}

func TestReadyPhase(t *testing.T) {
	cfg := testConfig(t, "")
	tests := []struct {
		name    string
		objs    []ctrlclient.Object
		want    phase.State
		waiting string
	}{
		{"no Cluster", nil, phase.Next, "(phase apply)"},
		{"control plane not available", readyCluster("False", "True", 1), phase.Next, "not ControlPlaneAvailable"},
		{"replicas not ready", readyCluster("True", "True", 0), phase.Next, "0 of 1 replicas ready"},
		{"machine not healthy", readyCluster("True", "False", 1), phase.Next, "mgmt-abc is not NodeHealthy"},
		{"ready", readyCluster("True", "True", 1), phase.Done, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &readyPhase{cfg: cfg, cluster: BootstrapCluster(cfg, running(t)), clients: fakeClients(t, tt.objs...), timeout: time.Millisecond}
			st, err := p.Status(t.Context())
			if err != nil || st.State != tt.want || !strings.Contains(st.Reason, tt.waiting) {
				t.Errorf("Status: %+v %v, want %v %q", st, err, tt.want, tt.waiting)
			}
		})
	}
}

func keyPair(t *testing.T) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

func pkiSecrets(t *testing.T) []ctrlclient.Object {
	t.Helper()
	var out []ctrlclient.Object
	for _, suffix := range []string{"ca", "proxy"} {
		crt, key := keyPair(t)
		out = append(out, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "mgmt", Name: "mgmt-" + suffix},
			Data: map[string][]byte{"tls.crt": []byte(crt), "tls.key": []byte(key)}})
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	kder, _ := x509.MarshalECPrivateKey(key)
	out = append(out, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "mgmt", Name: "mgmt-sa"}, Data: map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}),
	}})
	return out
}

const recoveryConfig = `recovery:
  profile: k0smotron-kine
  bundleFromEnv: MGMT_BUNDLE
  encryptionKeyFromEnv: MGMT_ENCRYPTION_KEY
  kine: {datasourceFromEnv: MGMT_KINE}
`

func TestRecoveryBundlePhase(t *testing.T) {
	objs := append(readyCluster("True", "True", 1), pkiSecrets(t)...)
	clients := fakeClients(t, objs...)
	c, _ := clients(nil)
	live, err := recovery.Export(t.Context(), c, "mgmt", "mgmt")
	if err != nil {
		t.Fatal(err)
	}
	good, _ := live.JSON()
	otherCfg := *live
	otherCfg.CA.Cert, otherCfg.CA.Key = keyPair(t)
	other, _ := otherCfg.JSON()

	full := map[string]string{"MGMT_BUNDLE": string(good), "MGMT_ENCRYPTION_KEY": "k", "MGMT_KINE": "postgres://x"}
	with := func(changes map[string]string) func(string) (string, bool) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		for k, v := range changes {
			env[k] = v
		}
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}

	tests := []struct {
		name   string
		config string
		lookup func(string) (string, bool)
		want   phase.State
		reason string
	}{
		{"no recovery profile", "", with(nil), phase.Done, "no recovery profile"},
		{"no bundle", recoveryConfig, with(map[string]string{"MGMT_BUNDLE": ""}), phase.Manual, "recovery export"},
		{"not a bundle", recoveryConfig, with(map[string]string{"MGMT_BUNDLE": "secret-junk"}), phase.Manual, "not a Recovery bundle"},
		{"another cluster's bundle", recoveryConfig, with(map[string]string{"MGMT_BUNDLE": string(other)}), phase.Manual, "the cluster CA differs"},
		{"no encryption key", recoveryConfig, with(map[string]string{"MGMT_ENCRYPTION_KEY": ""}), phase.Manual, "MGMT_ENCRYPTION_KEY"},
		{"all there, but no beforeApply file uses the key", recoveryConfig, with(nil), phase.Done, "warning: no beforeApply file uses ${MGMT_ENCRYPTION_KEY}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t, "")
			if tt.config != "" {
				var err error
				cfg, err = config.Parse("t.yaml", []byte(strings.Replace(testYAML, "bootstrapCluster:", tt.config+"bootstrapCluster:", 1)))
				if err != nil {
					t.Fatal(err)
				}
			}
			p := &recoveryBundlePhase{cfg: cfg, cluster: BootstrapCluster(cfg, running(t)), clients: clients, lookup: tt.lookup}
			st, err := p.Status(t.Context())
			if err != nil || st.State != tt.want || !strings.Contains(st.Reason, tt.reason) {
				t.Fatalf("Status: %+v %v, want %v %q", st, err, tt.want, tt.reason)
			}
			for _, secret := range []string{"PRIVATE KEY", "secret-junk"} {
				if strings.Contains(st.Reason, secret) {
					t.Errorf("the reason shows %q: %s", secret, st.Reason)
				}
			}
		})
	}
}

func TestKubeconfigPhase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kube", "mgmt.kubeconfig")
	cfg, err := config.Parse("t.yaml", []byte(strings.Replace(testYAML, "/nonexistent/selfhostctl-test/mgmt.kubeconfig", path, 1)))
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "mgmt", Name: "mgmt-kubeconfig"}, Data: map[string][]byte{"value": []byte("apiVersion: v1\nkind: Config\n")}}
	p := &kubeconfigPhase{cfg: cfg, cluster: BootstrapCluster(cfg, running(t)), clients: fakeClients(t, secret)}
	if st, err := p.Status(t.Context()); err != nil || st.State != phase.Next {
		t.Fatalf("before: %+v %v", st, err)
	}
	if err := p.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig %v, mode %v; want 0600", err, info.Mode().Perm())
	}
	if st, err := p.Status(t.Context()); err != nil || st.State != phase.Done {
		t.Errorf("after: %+v %v, want done", st, err)
	}
}
