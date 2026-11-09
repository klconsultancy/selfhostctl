package recovery

import (
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

func TestAdminKubeconfig(t *testing.T) {
	b := &Bundle{CA: ca(t, "ca"), Endpoint: Endpoint{Host: "10.0.0.1", Port: 6443}}
	now := time.Now()
	kc, err := b.AdminKubeconfig("mgmt", 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(kc)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters["mgmt"].Server; got != "https://10.0.0.1:6443" {
		t.Errorf("server %q", got)
	}
	exp, ok := ClientCertExpiry(kc)
	if !ok || exp.Sub(now) < 29*24*time.Hour || exp.Sub(now) > 31*24*time.Hour {
		t.Errorf("expiry %v %v, want in 30 days", exp, ok)
	}
	if note := ExpiryNote(kc, now, 60*24*time.Hour); !strings.HasPrefix(note, "warning:") {
		t.Errorf("a kubeconfig expiring within the warning window: %q", note)
	}
	if note := ExpiryNote(kc, now, 7*24*time.Hour); strings.HasPrefix(note, "warning:") {
		t.Errorf("a kubeconfig expiring later: %q", note)
	}
	// The bundle's CA must have signed it.
	if _, err := (&Bundle{CA: KeyPair{Cert: b.CA.Cert, Key: ca(t, "other").Key}}).AdminKubeconfig("mgmt", time.Hour, now); err == nil {
		t.Error("a CA key that doesn't belong to its certificate must be refused")
	}
}
