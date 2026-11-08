package recovery

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ca(t *testing.T, cn string) KeyPair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return KeyPair{
		Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		Key:  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})),
	}
}

func saKey(t *testing.T) KeyPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return KeyPair{
		Cert: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})),
		Key:  string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	}
}

func secret(name string, p KeyPair) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "mgmt", Name: name},
		Data:       map[string][]byte{"tls.crt": []byte(p.Cert), "tls.key": []byte(p.Key)},
	}
}

func cluster() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"controlPlaneEndpoint": map[string]any{"host": "172.18.0.3", "port": int64(6443)}},
	}}
	u.SetGroupVersionKind(clusterGVK)
	u.SetNamespace("mgmt")
	u.SetName("mgmt")
	return u
}

func fakeClient(t *testing.T, objs ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestExportRoundTrip(t *testing.T) {
	c := fakeClient(t, secret("mgmt-ca", ca(t, "ca")), secret("mgmt-sa", saKey(t)), secret("mgmt-proxy", ca(t, "front-proxy")), cluster())
	b, err := Export(t.Context(), c, "mgmt", "mgmt")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	data, err := b.JSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(string(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d := back.Compare(b); len(d) != 0 {
		t.Errorf("a round trip differs: %v", d)
	}
}

func TestCompare(t *testing.T) {
	live := &Bundle{APIVersion: APIVersion, Kind: Kind, Cluster: "mgmt/mgmt", Endpoint: Endpoint{"10.0.0.1", 6443},
		CA: ca(t, "ca"), ServiceAccount: saKey(t), FrontProxyCA: ca(t, "fp")}

	other := *live
	other.CA = ca(t, "another ca")
	d := other.Compare(live)
	if len(d) != 1 || d[0] != "the cluster CA differs from the cluster's" {
		t.Errorf("another CA: %v", d)
	}
	for _, s := range d {
		if strings.Contains(s, "PRIVATE KEY") || strings.Contains(s, "CERTIFICATE") {
			t.Errorf("a difference shows key material: %q", s)
		}
	}

	// The cluster's own pair with a key that doesn't belong: both sides alike, still
	// refused.
	mismatched := *live
	mismatched.ServiceAccount = KeyPair{Cert: live.ServiceAccount.Cert, Key: saKey(t).Key}
	if d := mismatched.Compare(&mismatched); len(d) != 1 || !strings.Contains(d[0], "service-account key: its private key doesn't belong") {
		t.Errorf("a key that doesn't belong: %v", d)
	}

	moved := *live
	moved.Endpoint.Host = "10.0.0.2"
	if d := moved.Compare(live); len(d) != 1 || !strings.Contains(d[0], "control-plane endpoint") {
		t.Errorf("another endpoint: %v", d)
	}
}

func TestParseNeverQuotesItsInput(t *testing.T) {
	_, err := Parse(`{"ca": {"key": "-----BEGIN EC PRIVATE KEY----- secret`)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("Parse: %v", err)
	}
}
