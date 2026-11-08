package recovery

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// AdminKubeconfig signs a new admin client certificate with the bundle's CA and returns
// a kubeconfig for the bundle's endpoint: the way back in when the operator's
// kubeconfig has expired. It is the one use of the CA key outside a Recovery.
func (b *Bundle) AdminKubeconfig(name string, validity time.Duration, now time.Time) ([]byte, error) {
	ca, err := tls.X509KeyPair([]byte(b.CA.Cert), []byte(b.CA.Key))
	if err != nil {
		return nil, errors.New("the bundle's CA key doesn't belong to its certificate")
	}
	caCert, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "selfhostctl-admin", Organization: []string{"system:masters"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	server := "https://" + net.JoinHostPort(b.Endpoint.Host, strconv.FormatInt(b.Endpoint.Port, 10))
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[name] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: []byte(b.CA.Cert)}
	cfg.AuthInfos[name+"-admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		ClientKeyData:         pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}),
	}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name + "-admin"}
	cfg.CurrentContext = name
	return clientcmd.Write(*cfg)
}

// ClientCertExpiry returns when a kubeconfig's client certificate expires, if it has
// one inline.
func ClientCertExpiry(kubeconfig []byte) (time.Time, bool) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return time.Time{}, false
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return time.Time{}, false
	}
	auth, ok := cfg.AuthInfos[ctx.AuthInfo]
	if !ok || len(auth.ClientCertificateData) == 0 {
		return time.Time{}, false
	}
	block, _ := pem.Decode(auth.ClientCertificateData)
	if block == nil {
		return time.Time{}, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, false
	}
	return cert.NotAfter, true
}

// ExpiryNote describes a kubeconfig's expiry for status: a warning within warnBefore.
func ExpiryNote(kubeconfig []byte, now time.Time, warnBefore time.Duration) string {
	exp, ok := ClientCertExpiry(kubeconfig)
	if !ok {
		return ""
	}
	if left := exp.Sub(now); left < warnBefore {
		return fmt.Sprintf("warning: its certificate expires %s (in %s)", exp.Format(time.DateOnly), left.Round(time.Hour))
	}
	return "its certificate expires " + exp.Format(time.DateOnly)
}
