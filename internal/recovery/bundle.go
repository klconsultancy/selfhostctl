// Package recovery holds the Recovery bundle (see GLOSSARY.md): the material a
// Recovery needs that only the cluster holds after a bootstrap.
package recovery

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	APIVersion = "selfhostctl/v1alpha1"
	Kind       = "RecoveryBundle"
)

// Bundle is the Recovery bundle. Its JSON is what `selfhostctl recovery export` writes
// and what recovery reads back from the environment.
type Bundle struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Cluster is the Management cluster's Cluster object, namespace/name.
	Cluster  string   `json:"cluster"`
	Endpoint Endpoint `json:"endpoint"`
	// CA is the cluster's certificate authority.
	CA KeyPair `json:"ca"`
	// ServiceAccount signs service-account tokens; Cert is its public key.
	ServiceAccount KeyPair `json:"serviceAccount"`
	// FrontProxyCA is the front proxy's certificate authority.
	FrontProxyCA KeyPair `json:"frontProxyCA"`
}

// Endpoint is the control plane's endpoint, which k0s needs as its externalAddress.
type Endpoint struct {
	Host string `json:"host"`
	Port int64  `json:"port"`
}

// KeyPair is a PEM certificate (or public key) and its PEM private key.
type KeyPair struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// authorities names each key pair as Cluster API's Secret suffixes do.
func (b *Bundle) authorities() []struct {
	name   string
	suffix string
	pair   *KeyPair
	isCert bool
} {
	return []struct {
		name   string
		suffix string
		pair   *KeyPair
		isCert bool
	}{
		{"the cluster CA", "ca", &b.CA, true},
		{"the service-account key", "sa", &b.ServiceAccount, false},
		{"the front-proxy CA", "proxy", &b.FrontProxyCA, true},
	}
}

var clusterGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}

// Export reads the Recovery bundle from the cluster that holds the Management
// cluster's objects: Cluster API's <name>-ca, -sa and -proxy Secrets and the Cluster's
// control-plane endpoint.
func Export(ctx context.Context, c ctrlclient.Client, namespace, name string) (*Bundle, error) {
	b := &Bundle{APIVersion: APIVersion, Kind: Kind, Cluster: namespace + "/" + name}
	for _, a := range b.authorities() {
		var s corev1.Secret
		key := ctrlclient.ObjectKey{Namespace: namespace, Name: name + "-" + a.suffix}
		if err := c.Get(ctx, key, &s); err != nil {
			return nil, fmt.Errorf("%s (Secret %s): %w", a.name, key, err)
		}
		a.pair.Cert, a.pair.Key = string(s.Data["tls.crt"]), string(s.Data["tls.key"])
		if a.pair.Cert == "" || a.pair.Key == "" {
			return nil, fmt.Errorf("%s (Secret %s) lacks tls.crt or tls.key", a.name, key)
		}
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	if err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: name}, cluster); err != nil {
		return nil, fmt.Errorf("the Cluster %s/%s: %w", namespace, name, err)
	}
	host, _, _ := unstructured.NestedString(cluster.Object, "spec", "controlPlaneEndpoint", "host")
	port, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "controlPlaneEndpoint", "port")
	if host == "" || port == 0 {
		return nil, fmt.Errorf("the Cluster %s/%s has no control-plane endpoint yet", namespace, name)
	}
	b.Endpoint = Endpoint{Host: host, Port: port}
	return b, nil
}

// JSON is the bundle as `recovery export` writes it.
func (b *Bundle) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Parse reads a bundle from the environment. Errors never quote it.
func Parse(data string) (*Bundle, error) {
	var b Bundle
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, errors.New("not a Recovery bundle (as `selfhostctl recovery export` writes it)")
	}
	if b.APIVersion != APIVersion || b.Kind != Kind {
		return nil, fmt.Errorf("not a %s %s", APIVersion, Kind)
	}
	return &b, nil
}

// Compare lists how the bundle differs from the cluster's, naming what differs and
// never showing key material. Each private key must also belong to its certificate.
func (b *Bundle) Compare(live *Bundle) []string {
	var diffs []string
	if b.Cluster != live.Cluster {
		diffs = append(diffs, fmt.Sprintf("it is for Cluster %s, not %s", b.Cluster, live.Cluster))
	}
	if b.Endpoint != live.Endpoint {
		diffs = append(diffs, fmt.Sprintf("its control-plane endpoint is %s:%d, the cluster's is %s:%d", b.Endpoint.Host, b.Endpoint.Port, live.Endpoint.Host, live.Endpoint.Port))
	}
	mine, theirs := b.authorities(), live.authorities()
	for i, a := range mine {
		if !reflect.DeepEqual(*a.pair, *theirs[i].pair) {
			diffs = append(diffs, a.name+" differs from the cluster's")
			continue
		}
		if err := matches(a.pair, a.isCert); err != nil {
			diffs = append(diffs, fmt.Sprintf("%s: %v", a.name, err))
		}
	}
	slices.Sort(diffs)
	return diffs
}

// matches checks that the private key belongs to the certificate or public key.
func matches(p *KeyPair, isCert bool) error {
	if isCert {
		if _, err := tls.X509KeyPair([]byte(p.Cert), []byte(p.Key)); err != nil {
			return errors.New("its key doesn't belong to its certificate")
		}
		return nil
	}
	pubBlock, _ := pem.Decode([]byte(p.Cert))
	keyBlock, _ := pem.Decode([]byte(p.Key))
	if pubBlock == nil || keyBlock == nil {
		return errors.New("not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return errors.New("its public key doesn't parse")
	}
	key, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return errors.New("its private key doesn't parse")
	}
	type equaler interface{ Equal(crypto.PublicKey) bool }
	if e, ok := pub.(equaler); !ok || !e.Equal(key.Public()) {
		return errors.New("its private key doesn't belong to its public key")
	}
	return nil
}

func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("not a signing key")
	}
	return s, nil
}
